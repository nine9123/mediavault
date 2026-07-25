// Package store is the only package that talks to SQLite. It treats the DB as
// a disposable cache: on a schema-version mismatch it drops everything and
// re-applies schema.sql (the source of truth is the filesystem, so this is
// safe). All writes go through a single goroutine in the scanner, so we never
// fight SQLITE_BUSY; readers (the HTTP layer) run concurrently under WAL.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"mediavault/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is bumped whenever schema.sql changes in a way that requires a
// rebuild. On mismatch the cache is dropped and recreated — no migrations.
const schemaVersion = "1"

// itemCols is the canonical column order for every items SELECT, so a single
// scanItem helper can decode any of them.
const itemCols = "id,relpath,size,mtime,content_hash,type,mime,width,height,duration,thumb_ready,indexed_at,seen_at,missing_since"

type Store struct{ db *sql.DB }

// ItemWrite is what the scanner hands the writer for one file.
type ItemWrite struct {
	Item       model.Item
	ManualTags []model.Tag // sidecar-derived; synced as this item's manual tags
}

// ChainDef is a fully-resolved chain the scanner asks us to (re)create.
type ChainDef struct {
	Key, Title, Kind, Source string
	Relpaths                 []string // in reading order
}

// IndexRow is the cheap snapshot used for change-detection.
type IndexRow struct {
	ID    int64
	Size  int64
	MTime int64
}

// Open connects, sets pragmas per-connection (via DSN so every pooled conn
// gets them), and ensures the schema is current.
func Open(path string) (*Store, error) {
	// modernc applies each _pragma on connection open. WAL is persistent;
	// busy_timeout and foreign_keys are per-connection, hence the DSN form.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4) // a few readers + our one writer is plenty at this scale
	s := &Store{db: db}
	if err := s.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ensureSchema() error {
	var got string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&got)
	switch {
	case err == nil && got == schemaVersion:
		return nil // cache is current
	case err == nil && got != schemaVersion:
		if derr := s.dropAll(); derr != nil { // stale cache -> rebuild
			return derr
		}
	default:
		// err != nil: fresh DB (no meta table). Fall through and create.
	}
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, schemaVersion)
	return err
}

func (s *Store) dropAll() error {
	for _, t := range []string{
		"items_fts", "scan_runs", "chain_items", "chains",
		"item_tags", "tags", "items", "meta",
	} {
		if _, err := s.db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			return err
		}
	}
	return nil
}

// LoadIndex returns a relpath -> (id,size,mtime) snapshot of the *present*
// files. Missing (soft-deleted) files are excluded so that if one reappears it
// is treated as new and reprocessed (which clears its missing flag). At this
// scale the whole map is tiny, so change-detection needs zero per-file DB hits.
func (s *Store) LoadIndex(ctx context.Context) (map[string]IndexRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT relpath,id,size,mtime FROM items WHERE missing_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]IndexRow)
	for rows.Next() {
		var rp string
		var r IndexRow
		if err := rows.Scan(&rp, &r.ID, &r.Size, &r.MTime); err != nil {
			return nil, err
		}
		out[rp] = r
	}
	return out, rows.Err()
}

// WriteBatch upserts a batch of items and syncs their manual tags + FTS rows,
// all in one transaction. Called only from the scanner's single writer.
func (s *Store) WriteBatch(ctx context.Context, batch []ItemWrite) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, w := range batch {
		it := w.Item
		var id int64
		err := tx.QueryRowContext(ctx, `
INSERT INTO items
  (relpath,size,mtime,content_hash,type,mime,width,height,duration,thumb_ready,indexed_at,seen_at,missing_since)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NULL)
ON CONFLICT(relpath) DO UPDATE SET
  size=excluded.size, mtime=excluded.mtime,
  content_hash=COALESCE(excluded.content_hash, items.content_hash),
  type=excluded.type, mime=excluded.mime,
  width=excluded.width, height=excluded.height, duration=excluded.duration,
  thumb_ready=excluded.thumb_ready, indexed_at=excluded.indexed_at,
  seen_at=excluded.seen_at, missing_since=NULL
RETURNING id`,
			it.Relpath, it.Size, it.MTime.Unix(), nullStr(it.ContentHash),
			string(it.Type), it.MIME, it.Width, it.Height, it.Duration,
			b2i(it.ThumbReady), it.IndexedAt.Unix(), it.SeenAt.Unix(),
		).Scan(&id)
		if err != nil {
			return fmt.Errorf("upsert %s: %w", it.Relpath, err)
		}

		// Sidecar is the source of truth for manual tags: clear this item's
		// manual tags and re-add from the sidecar. AI tags are left untouched.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM item_tags WHERE item_id=? AND source='manual'`, id); err != nil {
			return err
		}
		for _, t := range w.ManualTags {
			tagID, err := upsertTag(ctx, tx, t)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO item_tags(item_id,tag_id,source) VALUES(?,?, 'manual')`,
				id, tagID); err != nil {
				return err
			}
		}

		// Keep FTS in sync (rowid = item id). FTS5 has no upsert; delete+insert.
		if _, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE rowid=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO items_fts(rowid,relpath,tags) VALUES(?,?,?)`,
			id, it.Relpath, tagNames(w.ManualTags)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkMissing flags files a scan no longer saw. Returns how many newly flipped
// to missing. The scanner guards against calling this when the whole root is
// unavailable, so this won't nuke everything on an unmount.
func (s *Store) MarkMissing(ctx context.Context, relpaths []string, at time.Time) (int, error) {
	if len(relpaths) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE items SET missing_since=? WHERE relpath=? AND missing_since IS NULL`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, rp := range relpaths {
		res, err := stmt.ExecContext(ctx, at.Unix(), rp)
		if err != nil {
			return 0, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// ReplaceAllChains rebuilds every chain from scratch (chains are fully derived,
// so wiping and reinserting each scan is the simplest correct approach).
func (s *Store) ReplaceAllChains(ctx context.Context, defs []ChainDef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM chains`); err != nil { // cascades chain_items
		return err
	}
	idOf, err := tx.PrepareContext(ctx, `SELECT id FROM items WHERE relpath=?`)
	if err != nil {
		return err
	}
	defer idOf.Close()
	insChain, err := tx.PrepareContext(ctx,
		`INSERT INTO chains(key,title,kind,source) VALUES(?,?,?,?) RETURNING id`)
	if err != nil {
		return err
	}
	defer insChain.Close()
	insItem, err := tx.PrepareContext(ctx,
		`INSERT INTO chain_items(chain_id,item_id,position) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer insItem.Close()

	for _, d := range defs {
		var cid int64
		if err := insChain.QueryRowContext(ctx, d.Key, d.Title, d.Kind, d.Source).Scan(&cid); err != nil {
			return err
		}
		pos := 0
		for _, rp := range d.Relpaths {
			var iid int64
			if err := idOf.QueryRowContext(ctx, rp).Scan(&iid); err != nil {
				continue // not indexed (yet); skip silently
			}
			if _, err := insItem.ExecContext(ctx, cid, iid, pos); err != nil {
				return err
			}
			pos++
		}
	}
	return tx.Commit()
}

// RecordScanRun stores one row of scan telemetry.
func (s *Store) RecordScanRun(ctx context.Context, st model.ScanStats) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scan_runs(started_at,ended_at,seen,added,updated,missing,errors)
		 VALUES (?,?,?,?,?,?,?)`,
		st.Started.Unix(), st.Ended.Unix(), st.Seen, st.Added, st.Updated, st.Missing, st.Errors)
	return err
}

// ListItems returns present items, newest path last, for the grid.
func (s *Store) ListItems(ctx context.Context, limit, offset int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items WHERE missing_since IS NULL ORDER BY relpath LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// SearchItems runs an FTS5 query over path + tags.
func (s *Store) SearchItems(ctx context.Context, q string, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixCols("i")+`
		FROM items_fts f JOIN items i ON i.id = f.rowid
		WHERE items_fts MATCH ? AND i.missing_since IS NULL
		ORDER BY f.rank
		LIMIT ?`, ftsQuery(q), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ItemByRelpath fetches a single item (used by the raw/thumb endpoints).
func (s *Store) ItemByRelpath(ctx context.Context, relpath string) (model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE relpath=?`, relpath)
	if err != nil {
		return model.Item{}, err
	}
	defer rows.Close()
	items, err := scanItems(rows)
	if err != nil {
		return model.Item{}, err
	}
	if len(items) == 0 {
		return model.Item{}, sql.ErrNoRows
	}
	return items[0], nil
}

// ---- helpers ----

func scanItems(rows *sql.Rows) ([]model.Item, error) {
	var out []model.Item
	for rows.Next() {
		var (
			it                       model.Item
			mtime, indexedAt, seenAt int64
			hash, mime               sql.NullString
			width, height            sql.NullInt64
			dur                      sql.NullFloat64
			thumb                    int64
			missing                  sql.NullInt64
		)
		if err := rows.Scan(
			&it.ID, &it.Relpath, &it.Size, &mtime, &hash, &it.Type, &mime,
			&width, &height, &dur, &thumb, &indexedAt, &seenAt, &missing,
		); err != nil {
			return nil, err
		}
		it.MTime = time.Unix(mtime, 0)
		it.IndexedAt = time.Unix(indexedAt, 0)
		it.SeenAt = time.Unix(seenAt, 0)
		it.ContentHash = hash.String
		it.MIME = mime.String
		it.Width = int(width.Int64)
		it.Height = int(height.Int64)
		it.Duration = dur.Float64
		it.ThumbReady = thumb != 0
		if missing.Valid {
			t := time.Unix(missing.Int64, 0)
			it.MissingSince = &t
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func upsertTag(ctx context.Context, tx *sql.Tx, t model.Tag) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO tags(namespace,name) VALUES(?,?)`, t.Namespace, t.Name); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE namespace=? AND name=?`, t.Namespace, t.Name).Scan(&id)
	return id, err
}

func tagNames(tags []model.Tag) string {
	parts := make([]string, 0, len(tags))
	for _, t := range tags {
		if t.Namespace != "" {
			parts = append(parts, t.Namespace+":"+t.Name)
		} else {
			parts = append(parts, t.Name)
		}
	}
	return strings.Join(parts, " ")
}

// ftsQuery turns free text into a safe FTS5 MATCH string: each whitespace token
// becomes a prefix term ("cat" -> cat*), ANDed together.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = strings.ReplaceAll(f, `"`, "") + "*"
	}
	if len(fields) == 0 {
		return `""`
	}
	return strings.Join(fields, " ")
}

func prefixCols(alias string) string {
	cols := strings.Split(itemCols, ",")
	for i, c := range cols {
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ",")
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
