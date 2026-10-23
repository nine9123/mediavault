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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"

	"mediavault/internal/media"
	"mediavault/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is bumped whenever schema.sql changes in a way that requires a
// rebuild. On mismatch the cache is dropped and recreated — no migrations.
const schemaVersion = "2"

// itemCols is the canonical column order for every items SELECT, so a single
// scanItem helper can decode any of them.
const itemCols = "id,relpath,size,mtime,content_hash,type,mime,width,height,duration,offset_s,thumb_ready,preview_ready,views,indexed_at,seen_at,missing_since,description"

type Store struct {
	db *sql.DB

	// In-memory cache of perceptual signatures for brute-force similarity
	// search (fast at personal-library scale; no external vector DB needed).
	vecMu   sync.Mutex
	vecs    map[int64][]float32
	vecInit bool

	// Caption text-embedding cache (semantic search).
	embMu   sync.Mutex
	embs    map[int64][]float32
	embInit bool
}

// ItemWrite is what the scanner hands the writer for one file.
type ItemWrite struct {
	Item       model.Item
	ManualTags []model.Tag // sidecar-derived; synced as this item's manual tags
	Vector     []float32   // perceptual signature; nil to leave unchanged

	// SidecarMTime is the sidecar's mtime (unix ns, 0 if there is none) as of
	// the read that produced ManualTags, so the next scan can tell whether the
	// sidecar has been edited since.
	SidecarMTime int64
	// TagsOnly syncs ManualTags and SidecarMTime onto an already-indexed item
	// and leaves everything else about it alone: the sidecar changed, the media
	// file didn't. Only Item.Relpath is read.
	TagsOnly bool
}

// ChainDef is a fully-resolved chain the scanner asks us to (re)create.
type ChainDef struct {
	Key, Title, Kind, Source string
	Relpaths                 []string // in reading order
}

// IndexRow is the cheap snapshot used for change-detection.
type IndexRow struct {
	ID           int64
	Size         int64
	MTime        int64
	ThumbReady   bool
	PreviewReady bool
	HasVector    bool
	Offset       float64 // poster offset as last read from the sidecar
	SidecarMTime int64   // unix ns of the sidecar last synced; 0 = none
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
		return s.ensureColumns() // current, but still apply additive column migrations
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
	if _, err = s.db.Exec(
		`INSERT INTO meta(key,value) VALUES('schema_version',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, schemaVersion); err != nil {
		return err
	}
	return s.ensureColumns()
}

// ensureColumns applies additive, non-destructive column migrations so an
// existing (version-matched) cache can gain new optional columns without a full
// drop-and-rescan. Each step must be idempotent.
func (s *Store) ensureColumns() error {
	has, err := s.hasColumn("items", "views")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.db.Exec(`ALTER TABLE items ADD COLUMN views INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add items.views: %w", err)
		}
	}
	hasOff, err := s.hasColumn("items", "offset_s")
	if err != nil {
		return err
	}
	if !hasOff {
		if _, err := s.db.Exec(`ALTER TABLE items ADD COLUMN offset_s REAL`); err != nil {
			return fmt.Errorf("add items.offset_s: %w", err)
		}
	}
	hasPrev, err := s.hasColumn("items", "preview_ready")
	if err != nil {
		return err
	}
	if !hasPrev {
		if _, err := s.db.Exec(`ALTER TABLE items ADD COLUMN preview_ready INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add items.preview_ready: %w", err)
		}
	}
	hasDesc, err := s.hasColumn("items", "description")
	if err != nil {
		return err
	}
	if !hasDesc {
		if _, err := s.db.Exec(`ALTER TABLE items ADD COLUMN description TEXT`); err != nil {
			return fmt.Errorf("add items.description: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS item_vectors (
		item_id INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
		dim     INTEGER NOT NULL,
		vec     BLOB    NOT NULL
	)`); err != nil {
		return fmt.Errorf("create item_vectors: %w", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS item_embeddings (
		item_id INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
		dim     INTEGER NOT NULL,
		vec     BLOB    NOT NULL
	)`); err != nil {
		return fmt.Errorf("create item_embeddings: %w", err)
	}
	hasFaces, err := s.hasColumn("items", "faces_done")
	if err != nil {
		return err
	}
	if !hasFaces {
		if _, err := s.db.Exec(`ALTER TABLE items ADD COLUMN faces_done INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add items.faces_done: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS people (
		id   INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE
	)`); err != nil {
		return fmt.Errorf("create people: %w", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS faces (
		id        INTEGER PRIMARY KEY,
		item_id   INTEGER NOT NULL REFERENCES items(id)  ON DELETE CASCADE,
		person_id INTEGER          REFERENCES people(id) ON DELETE SET NULL,
		x         REAL NOT NULL,
		y         REAL NOT NULL,
		w         REAL NOT NULL,
		h         REAL NOT NULL,
		score     REAL,
		dim       INTEGER NOT NULL,
		vec       BLOB    NOT NULL
	)`); err != nil {
		return fmt.Errorf("create faces: %w", err)
	}
	for _, ix := range []string{
		`CREATE INDEX IF NOT EXISTS idx_faces_item   ON faces(item_id)`,
		`CREATE INDEX IF NOT EXISTS idx_faces_person ON faces(person_id)`,
	} {
		if _, err := s.db.Exec(ix); err != nil {
			return fmt.Errorf("index faces: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS face_group_match (
		face_id INTEGER PRIMARY KEY REFERENCES faces(id) ON DELETE CASCADE,
		match   REAL NOT NULL
	)`); err != nil {
		return fmt.Errorf("create face_group_match: %w", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS face_group_merge (
		face_id  INTEGER PRIMARY KEY REFERENCES faces(id) ON DELETE CASCADE,
		merge_id INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create face_group_merge: %w", err)
	}
	if _, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_face_group_merge ON face_group_merge(merge_id)`); err != nil {
		return fmt.Errorf("index face_group_merge: %w", err)
	}
	hasSc, err := s.hasColumn("items", "sidecar_mtime")
	if err != nil {
		return err
	}
	if !hasSc {
		if _, err := s.db.Exec(`ALTER TABLE items ADD COLUMN sidecar_mtime INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add items.sidecar_mtime: %w", err)
		}
	}
	return nil
}

// hasColumn reports whether table has a column named col. table is an internal
// constant, never user input.
func (s *Store) hasColumn(table, col string) (bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) dropAll() error {
	for _, t := range []string{
		"items_fts", "scan_runs", "chain_items", "chains",
		"item_vectors", "item_embeddings", "faces", "people",
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
		`SELECT relpath,id,size,mtime,thumb_ready,preview_ready,
		        (SELECT 1 FROM item_vectors v WHERE v.item_id=items.id),
		        COALESCE(offset_s,0),sidecar_mtime
		 FROM items WHERE missing_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]IndexRow)
	for rows.Next() {
		var rp string
		var r IndexRow
		var thumb, prev int64
		var hasVec sql.NullInt64
		if err := rows.Scan(&rp, &r.ID, &r.Size, &r.MTime, &thumb, &prev, &hasVec, &r.Offset, &r.SidecarMTime); err != nil {
			return nil, err
		}
		r.ThumbReady = thumb != 0
		r.PreviewReady = prev != 0
		r.HasVector = hasVec.Valid
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

	type cached struct {
		id  int64
		vec []float32
	}
	var vecs []cached
	for _, w := range batch {
		it := w.Item
		var id int64
		if w.TagsOnly {
			err := tx.QueryRowContext(ctx,
				`UPDATE items SET sidecar_mtime=? WHERE relpath=? RETURNING id`,
				w.SidecarMTime, it.Relpath).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				continue // gone since the walk saw it; nothing to tag
			}
			if err != nil {
				return fmt.Errorf("sync tags %s: %w", it.Relpath, err)
			}
			if err := syncManualTags(ctx, tx, id, it.Relpath, w.ManualTags); err != nil {
				return err
			}
			continue
		}
		err := tx.QueryRowContext(ctx, `
INSERT INTO items
  (relpath,size,mtime,content_hash,type,mime,width,height,duration,offset_s,thumb_ready,preview_ready,indexed_at,seen_at,sidecar_mtime,missing_since)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
ON CONFLICT(relpath) DO UPDATE SET
  size=excluded.size, mtime=excluded.mtime,
  content_hash=COALESCE(excluded.content_hash, items.content_hash),
  type=excluded.type, mime=excluded.mime,
  width=excluded.width, height=excluded.height, duration=excluded.duration,
  offset_s=excluded.offset_s,
  thumb_ready=excluded.thumb_ready, preview_ready=excluded.preview_ready,
  indexed_at=excluded.indexed_at,
  seen_at=excluded.seen_at, sidecar_mtime=excluded.sidecar_mtime, missing_since=NULL
RETURNING id`,
			it.Relpath, it.Size, it.MTime.Unix(), nullStr(it.ContentHash),
			string(it.Type), it.MIME, it.Width, it.Height, it.Duration, nullFloat(it.Offset),
			b2i(it.ThumbReady), b2i(it.PreviewReady), it.IndexedAt.Unix(), it.SeenAt.Unix(), w.SidecarMTime,
		).Scan(&id)
		if err != nil {
			return fmt.Errorf("upsert %s: %w", it.Relpath, err)
		}

		if w.Vector != nil {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO item_vectors(item_id,dim,vec) VALUES(?,?,?)
				 ON CONFLICT(item_id) DO UPDATE SET dim=excluded.dim, vec=excluded.vec`,
				id, len(w.Vector), encodeVec(w.Vector)); err != nil {
				return err
			}
			vecs = append(vecs, cached{id, w.Vector})
		}

		if err := syncManualTags(ctx, tx, id, it.Relpath, w.ManualTags); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Only after the commit: a rolled-back batch must not leave vectors in the
	// similarity cache that the table doesn't have.
	for _, c := range vecs {
		s.cacheVec(c.id, c.vec)
	}
	return nil
}

// syncManualTags makes tags the item's complete set of manual tags and
// refreshes its FTS row. The sidecar is the source of truth for manual tags, so
// the old set is cleared rather than merged into; AI tags are left untouched.
func syncManualTags(ctx context.Context, tx *sql.Tx, id int64, relpath string, tags []model.Tag) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM item_tags WHERE item_id=? AND source='manual'`, id); err != nil {
		return err
	}
	for _, t := range tags {
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
	// Keep FTS in sync from ALL of the item's tags (manual + any ai), so
	// search finds either kind. FTS5 has no upsert; rebuildItemFTS does
	// delete+insert.
	return rebuildItemFTS(ctx, tx, id, relpath)
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
		`SELECT `+itemCols+` FROM items WHERE missing_since IS NULL ORDER BY indexed_at DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// SearchItems runs an FTS5 query over path + tags.
// SearchItems runs the tag/filename FTS search, optionally constrained to a
// media type (typ empty = any). With empty text but a type, it lists items of
// that type; with both empty it returns nothing.
func (s *Store) SearchItems(ctx context.Context, text, typ string, limit int) ([]model.Item, error) {
	if match := ftsQuery(text); match != "" {
		q := `SELECT ` + prefixCols("i") + `
			FROM items_fts f JOIN items i ON i.id = f.rowid
			WHERE items_fts MATCH ? AND i.missing_since IS NULL`
		args := []any{match}
		if typ != "" {
			q += ` AND i.type = ?`
			args = append(args, typ)
		}
		q += ` ORDER BY f.rank LIMIT ?`
		args = append(args, limit)
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanItems(rows)
	}
	if typ != "" {
		rows, err := s.db.QueryContext(ctx,
			`SELECT `+itemCols+` FROM items WHERE type = ? AND missing_since IS NULL
			 ORDER BY indexed_at DESC, id DESC LIMIT ?`, typ, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanItems(rows)
	}
	return nil, nil
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
			dur, off                 sql.NullFloat64
			thumb, prev              int64
			views                    int64
			missing                  sql.NullInt64
			desc                     sql.NullString
		)
		if err := rows.Scan(
			&it.ID, &it.Relpath, &it.Size, &mtime, &hash, &it.Type, &mime,
			&width, &height, &dur, &off, &thumb, &prev, &views, &indexedAt, &seenAt, &missing, &desc,
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
		it.Offset = off.Float64
		it.ThumbReady = thumb != 0
		it.PreviewReady = prev != 0
		it.Views = int(views)
		it.Description = desc.String
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

// rebuildItemFTS refreshes the FTS row for an item from ALL its current tags
// (manual + ai). FTS5 has no upsert, so we delete then insert by rowid = id.
func rebuildItemFTS(ctx context.Context, tx *sql.Tx, itemID int64, relpath string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT t.namespace, t.name FROM item_tags it JOIN tags t ON t.id = it.tag_id WHERE it.item_id = ?`,
		itemID)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var ns, nm string
		if err := rows.Scan(&ns, &nm); err != nil {
			rows.Close()
			return err
		}
		if ns != "" {
			names = append(names, ns+":"+nm)
		} else {
			names = append(names, nm)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close() // must close before further statements on the same tx

	content := strings.Join(names, " ")
	var desc sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT description FROM items WHERE id=?`, itemID).Scan(&desc); err == nil && desc.Valid && desc.String != "" {
		content = strings.TrimSpace(content + " " + desc.String)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE rowid=?`, itemID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO items_fts(rowid,relpath,tags) VALUES(?,?,?)`,
		itemID, relpath, content)
	return err
}

// ftsQuery turns free text into a safe FTS5 MATCH string. It tokenises on any
// non-alphanumeric character — matching how the content is indexed — so a
// namespaced tag typed literally (e.g. "artist:jane") becomes two prefix terms
// and never trips FTS5's column-filter syntax. Returns "" when there are no
// searchable tokens.
func ftsQuery(q string) string {
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String()+"*")
			b.Reset()
		}
	}
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(tokens, " ")
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

// nullFloat keeps "unset" distinguishable from a real 0 in the column, matching
// how the sidecar omits the field entirely.
func nullFloat(f float64) any {
	if f <= 0 {
		return nil
	}
	return f
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- stats + per-item tags (for the UI) ----

// ScanRunInfo is a single scan_runs row, exposed for the stats endpoint.
type ScanRunInfo struct {
	Started int64
	Ended   int64 // 0 if the run hasn't finished
	Seen    int
	Added   int
	Updated int
	Missing int
	Errors  int
}

// Stats is the collection summary shown in the UI header.
type Stats struct {
	Items int
	Last  *ScanRunInfo // nil if no scan has run yet
}

// Stats returns the present-item count and the most recent scan run.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE missing_since IS NULL`).Scan(&st.Items); err != nil {
		return st, err
	}
	var r ScanRunInfo
	var ended sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT started_at, ended_at, seen, added, updated, missing, errors
		FROM scan_runs ORDER BY id DESC LIMIT 1`).
		Scan(&r.Started, &ended, &r.Seen, &r.Added, &r.Updated, &r.Missing, &r.Errors)
	if err == sql.ErrNoRows {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	r.Ended = ended.Int64
	st.Last = &r
	return st, nil
}

// ItemTag is one tag applied to an item, with its source.
type ItemTag struct {
	Namespace  string
	Name       string
	Source     string // manual | ai
	Confidence float64
}

// TagsForItem returns every tag on an item (manual and ai), ordered.
func (s *Store) TagsForItem(ctx context.Context, itemID int64) ([]ItemTag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.namespace, t.name, it.source, COALESCE(it.confidence, 0)
		FROM item_tags it JOIN tags t ON t.id = it.tag_id
		WHERE it.item_id = ?
		ORDER BY t.namespace, t.name, it.source`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemTag
	for rows.Next() {
		var t ItemTag
		if err := rows.Scan(&t.Namespace, &t.Name, &t.Source, &t.Confidence); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- id-cursor listing (stable under concurrent inserts) ----
//
// Browsing pages by item id instead of OFFSET. New rows always get higher ids,
// so paging toward older ids never repeats or skips a row even while a scan is
// inserting — and the UI can prepend genuinely-new rows at the top.

// ItemsOlder returns present items newest-first (by id). With beforeID > 0 it
// returns only items older than that id (the "load more" cursor).
func (s *Store) ItemsOlder(ctx context.Context, beforeID int64, limit int) ([]model.Item, error) {
	q := `SELECT ` + itemCols + ` FROM items WHERE missing_since IS NULL`
	args := []any{}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ItemsNewer returns present items with id greater than afterID, oldest-first,
// so the UI can prepend them in order as a scan indexes new files.
func (s *Store) ItemsNewer(ctx context.Context, afterID int64, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items WHERE missing_since IS NULL AND id > ? ORDER BY id ASC LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// SetManualTags replaces an item's manual tags (from the tag editor). It syncs
// the DB immediately — clearing this item's manual tags, inserting the new set,
// and refreshing FTS — so the change is searchable without waiting for a scan.
// AI tags are untouched. The sidecar (written by the caller) stays the durable
// source of truth.
func (s *Store) SetManualTags(ctx context.Context, relpath string, tags []model.Tag) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM items WHERE relpath=?`, relpath).Scan(&id); err != nil {
		return err // sql.ErrNoRows if the item isn't indexed
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_tags WHERE item_id=? AND source='manual'`, id); err != nil {
		return err
	}
	for _, t := range tags {
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
	if err := rebuildItemFTS(ctx, tx, id, relpath); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- discovery: random + tag-based recommendations ----

// RandomItem returns one present item at random. If q is non-empty it draws
// only from items matching that search query (so "random" respects a filter).
// found is false when there's nothing to pick (empty library or no matches).
// RandomItem returns one present item at random. text (FTS) and typ (media
// type) optionally constrain the pool, so "random" can respect an active
// search/type filter. found is false when nothing matches.
func (s *Store) RandomItem(ctx context.Context, text, typ string) (item model.Item, found bool, err error) {
	var rows *sql.Rows
	switch {
	case ftsQuery(text) != "":
		q := `SELECT ` + prefixCols("i") + `
			FROM items_fts f JOIN items i ON i.id = f.rowid
			WHERE items_fts MATCH ? AND i.missing_since IS NULL`
		args := []any{ftsQuery(text)}
		if typ != "" {
			q += ` AND i.type = ?`
			args = append(args, typ)
		}
		q += ` ORDER BY RANDOM() LIMIT 1`
		rows, err = s.db.QueryContext(ctx, q, args...)
	case typ != "":
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+itemCols+` FROM items WHERE type = ? AND missing_since IS NULL
			 ORDER BY RANDOM() LIMIT 1`, typ)
	default:
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+itemCols+` FROM items WHERE missing_since IS NULL
			 ORDER BY RANDOM() LIMIT 1`)
	}
	if err != nil {
		return model.Item{}, false, err
	}
	defer rows.Close()
	items, err := scanItems(rows)
	if err != nil || len(items) == 0 {
		return model.Item{}, false, err
	}
	return items[0], true, nil
}

// RecommendByTags returns items related to the seed (identified by relpath),
// best first. The rank blends tag overlap with popularity: each shared tag is
// worth 1, and an item's view count adds up to ~3 more (capped), so a
// frequently-opened item can edge ahead of one with a couple more tags without
// letting popularity override a much stronger tag match. Both manual and AI
// tags count; a shared tag counts once regardless of source. Returns nothing
// when the seed has no tags. The score drives ORDER BY but isn't selected, so
// the row shape stays exactly itemCols.
func (s *Store) RecommendByTags(ctx context.Context, relpath string, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixCols("it")+`
		FROM item_tags t
		JOIN items it ON it.id = t.item_id
		WHERE t.tag_id IN (
			SELECT it2.tag_id FROM item_tags it2
			JOIN items seed ON seed.id = it2.item_id
			WHERE seed.relpath = ?
		)
		AND it.relpath != ?
		AND it.missing_since IS NULL
		GROUP BY it.id
		ORDER BY (COUNT(DISTINCT t.tag_id) + MIN(it.views, 60) * 0.05) DESC,
		         COUNT(DISTINCT t.tag_id) DESC,
		         it.views DESC,
		         it.indexed_at DESC
		LIMIT ?`, relpath, relpath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// IncrementViews bumps an item's view counter (called when it's opened in the
// viewer). The counter feeds recommendation ranking.
func (s *Store) IncrementViews(ctx context.Context, relpath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE items SET views = views + 1 WHERE relpath = ? AND missing_since IS NULL`, relpath)
	return err
}

// MostViewed returns the most-opened items (excluding excludeRelpath), used as a
// popularity fallback when tag-based recommendations come up empty. Only items
// that have actually been viewed qualify.
func (s *Store) MostViewed(ctx context.Context, excludeRelpath string, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+itemCols+` FROM items
		WHERE missing_since IS NULL AND views > 0 AND relpath != ?
		ORDER BY views DESC, indexed_at DESC
		LIMIT ?`, excludeRelpath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// TopTags returns the most-used manual tags across present items, most-used
// first — the user's own tagging vocabulary, for quick re-tagging suggestions.
func (s *Store) TopTags(ctx context.Context, limit int) ([]model.Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.namespace, t.name
		FROM item_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN items i ON i.id = it.item_id
		WHERE it.source = 'manual' AND i.missing_since IS NULL
		GROUP BY t.id
		ORDER BY COUNT(*) DESC, t.name COLLATE NOCASE
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Tag
	for rows.Next() {
		var t model.Tag
		if err := rows.Scan(&t.Namespace, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- perceptual similarity: brute-force cosine over cached signatures ----
// No external vector DB: at personal-library scale a full scan of a few tens of
// thousands of small vectors is a few milliseconds. Swap in an ANN index /
// Qdrant only if the library grows into the millions.

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// cacheVec adds a committed vector to the similarity cache, but only once that
// cache has been loaded. Before then there is nothing to keep in step: the
// first similarity search reads every vector from the table anyway, so holding
// them in memory during a scan would only cost RAM (a whole library's worth on
// the first scan) for a feature that may never be used this run. The caller
// must have committed the row first, so a load that starts after this check
// sees it in the table; one already running holds vecMu, and this waits for it.
func (s *Store) cacheVec(id int64, v []float32) {
	s.vecMu.Lock()
	if s.vecInit {
		s.vecs[id] = v
	}
	s.vecMu.Unlock()
}

func (s *Store) loadVecs(ctx context.Context) error {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	if s.vecInit {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, vec FROM item_vectors`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := make(map[int64][]float32)
	for rows.Next() {
		var id int64
		var b sql.RawBytes // decodeVec copies, so no need for Scan's own copy too
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		m[id] = decodeVec(b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.vecs = m
	s.vecInit = true
	return nil
}

// SimilarItems returns items whose perceptual signature is closest to the
// seed's (cosine), most-similar first. Empty if the seed has no signature yet.
func (s *Store) SimilarItems(ctx context.Context, relpath string, limit int) ([]model.Item, error) {
	seed, err := s.ItemByRelpath(ctx, relpath)
	if err != nil {
		return nil, err
	}
	if err := s.loadVecs(ctx); err != nil {
		return nil, err
	}

	// Scored in place under the lock rather than copying the whole library's
	// worth of candidates out first, and only the best limit are kept.
	s.vecMu.Lock()
	sv := s.vecs[seed.ID]
	var top topScores
	if sv != nil {
		top = newTopScores(limit)
		for id, v := range s.vecs {
			if id != seed.ID {
				top.offer(id, media.Cosine(sv, v))
			}
		}
	}
	s.vecMu.Unlock()
	if sv == nil {
		return nil, nil
	}
	all := top.sorted()

	ids := make([]int64, len(all))
	for i, a := range all {
		ids[i] = a.id
	}
	byID, err := s.ItemsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.Item, 0, len(ids))
	for _, id := range ids { // preserve similarity order
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// scored is one similarity-search candidate.
type scored struct {
	id    int64
	score float64
}

// topScores keeps the k highest-scoring candidates seen so far, as a min-heap on
// score: the weakest kept candidate is at the root, so most of a library is
// rejected with one comparison and memory stays at k entries rather than one per
// item.
type topScores struct {
	k int
	h []scored
}

func newTopScores(k int) topScores {
	if k < 0 {
		k = 0
	}
	return topScores{k: k, h: make([]scored, 0, k)}
}

func (t *topScores) offer(id int64, score float64) {
	if len(t.h) < t.k {
		t.h = append(t.h, scored{id, score})
		// sift up
		for i := len(t.h) - 1; i > 0; {
			p := (i - 1) / 2
			if t.h[p].score <= t.h[i].score {
				break
			}
			t.h[p], t.h[i] = t.h[i], t.h[p]
			i = p
		}
		return
	}
	if t.k == 0 || score <= t.h[0].score {
		return
	}
	t.h[0] = scored{id, score}
	// sift down
	for i, n := 0, len(t.h); ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < n && t.h[l].score < t.h[m].score {
			m = l
		}
		if r < n && t.h[r].score < t.h[m].score {
			m = r
		}
		if m == i {
			break
		}
		t.h[i], t.h[m] = t.h[m], t.h[i]
		i = m
	}
}

// sorted returns the kept candidates, best first.
func (t *topScores) sorted() []scored {
	sort.Slice(t.h, func(i, j int) bool { return t.h[i].score > t.h[j].score })
	return t.h
}

// ItemsByIDs fetches present items by id (absent/missing ids omitted).
func (s *Store) ItemsByIDs(ctx context.Context, ids []int64) (map[int64]model.Item, error) {
	out := make(map[int64]model.Item, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items WHERE missing_since IS NULL AND id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanItems(rows)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		out[it.ID] = it
	}
	return out, nil
}

// ---- AI captions (natural-language search) ----

// SetDescription stores a caption for an item and reindexes it so the caption
// is searchable in the existing full-text search.
func (s *Store) SetDescription(ctx context.Context, relpath, desc string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE items SET description=? WHERE relpath=? AND missing_since IS NULL RETURNING id`,
		desc, relpath).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return nil // vanished; nothing to do
		}
		return err
	}
	if err := rebuildItemFTS(ctx, tx, id, relpath); err != nil {
		return err
	}
	// Drop the old embedding: it encodes the PREVIOUS caption, and nothing else
	// would ever refresh it (CaptionedButUnembedded only looks for items with no
	// embedding at all). Without this, regenerating a caption leaves semantic
	// search matching text that is no longer displayed anywhere.
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_embeddings WHERE item_id=?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.forgetEmb(id) // keep the in-memory cache in step with the table
	return nil
}

// forgetEmb evicts one embedding from the in-memory search cache.
func (s *Store) forgetEmb(id int64) {
	s.embMu.Lock()
	delete(s.embs, id)
	s.embMu.Unlock()
}

// SetOffset records the poster-frame offset for an item (the sidecar remains the
// source of truth; this is the queryable mirror).
func (s *Store) SetOffset(ctx context.Context, relpath string, offset float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE items SET offset_s=? WHERE relpath=? AND missing_since IS NULL`,
		nullFloat(offset), relpath)
	return err
}

// UndescribedItems returns present, visual items still needing a caption, newest
// first (so recently-added media gets described first).
func (s *Store) UndescribedItems(ctx context.Context, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items
		 WHERE missing_since IS NULL AND width > 0 AND (description IS NULL OR description='')
		 ORDER BY indexed_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ---- caption embeddings: semantic search (brute-force cosine) ----

// cacheEmb adds a stored embedding to the search cache once it is loaded; see
// cacheVec for why it is left alone before then.
func (s *Store) cacheEmb(id int64, v []float32) {
	s.embMu.Lock()
	if s.embInit {
		s.embs[id] = v
	}
	s.embMu.Unlock()
}

func (s *Store) loadEmbs(ctx context.Context) error {
	s.embMu.Lock()
	defer s.embMu.Unlock()
	if s.embInit {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, vec FROM item_embeddings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := make(map[int64][]float32)
	for rows.Next() {
		var id int64
		var b sql.RawBytes // decodeVec copies, so no need for Scan's own copy too
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		m[id] = decodeVec(b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.embs = m
	s.embInit = true
	return nil
}

// SetEmbedding stores a caption embedding for an item.
func (s *Store) SetEmbedding(ctx context.Context, relpath string, vec []float32) error {
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM items WHERE relpath=? AND missing_since IS NULL`, relpath).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO item_embeddings(item_id,dim,vec) VALUES(?,?,?)
		 ON CONFLICT(item_id) DO UPDATE SET dim=excluded.dim, vec=excluded.vec`,
		id, len(vec), encodeVec(vec)); err != nil {
		return err
	}
	s.cacheEmb(id, vec)
	return nil
}

// CaptionedButUnembedded returns captioned items lacking a caption embedding
// (backfill when an embedding model is enabled after captioning), newest first.
func (s *Store) CaptionedButUnembedded(ctx context.Context, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items i
		 WHERE i.missing_since IS NULL AND i.description IS NOT NULL AND i.description != ''
		   AND NOT EXISTS (SELECT 1 FROM item_embeddings e WHERE e.item_id=i.id)
		 ORDER BY i.indexed_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// SemanticSearch ranks items by cosine of their caption embedding against qvec,
// optionally constrained to a media type. qvec must match the stored dimension.
func (s *Store) SemanticSearch(ctx context.Context, qvec []float32, typ string, limit int) ([]model.Item, error) {
	if err := s.loadEmbs(ctx); err != nil {
		return nil, err
	}
	// Restrict the candidate pool up front when a type filter is active.
	//
	// This used to rank every embedding, truncate to limit*4, and only then drop
	// rows of the wrong type — which returns nothing at all whenever the top
	// limit*4 matches happen to be the wrong type. On a library that is mostly
	// video, "type:image <phrase>" reliably came back empty, because 800 videos
	// scored above the best image. Filtering first makes the result depend on the
	// ranking rather than on the type mix of the library.
	var allow map[int64]bool
	if typ != "" {
		var err error
		if allow, err = s.idsOfType(ctx, typ); err != nil {
			return nil, err
		}
		if len(allow) == 0 {
			return nil, nil
		}
	}
	top := newTopScores(limit)
	s.embMu.Lock()
	for id, v := range s.embs {
		if allow != nil && !allow[id] {
			continue
		}
		if len(v) == len(qvec) {
			top.offer(id, media.Cosine(qvec, v))
		}
	}
	s.embMu.Unlock()
	all := top.sorted()
	ids := make([]int64, len(all))
	for i, a := range all {
		ids[i] = a.id
	}
	byID, err := s.ItemsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.Item, 0, limit)
	for _, a := range all {
		it, ok := byID[a.id]
		if !ok {
			continue
		}
		if typ != "" && string(it.Type) != typ {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// idsOfType returns the ids of present items of one media type, as a set for
// filtering the in-memory embedding scan.
func (s *Store) idsOfType(ctx context.Context, typ string) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM items WHERE type=? AND missing_since IS NULL`, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// CaptionCounts reports captioning/embedding progress for the status UI:
// how many present items are visual, how many are captioned, how many embedded.
func (s *Store) CaptionCounts(ctx context.Context) (visual, captioned, embedded int, err error) {
	if err = s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN width>0 THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN width>0 AND description IS NOT NULL AND description!='' THEN 1 ELSE 0 END),0)
		FROM items WHERE missing_since IS NULL`).Scan(&visual, &captioned); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM item_embeddings e JOIN items i ON i.id=e.item_id WHERE i.missing_since IS NULL`).Scan(&embedded)
	return
}

// ---- resets for regenerating captions ----

// ResetCaptions clears every caption and its embedding and rebuilds full-text
// search so stale caption text is dropped. The background worker then
// re-captions from scratch (used when changing the vision model or prompt).
func (s *Store) ResetCaptions(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET description=NULL`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_embeddings`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, relpath FROM items WHERE missing_since IS NULL`)
	if err != nil {
		return err
	}
	type ir struct {
		id  int64
		rel string
	}
	var list []ir
	for rows.Next() {
		var r ir
		if err := rows.Scan(&r.id, &r.rel); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range list { // rebuild FTS without caption text
		if err := rebuildItemFTS(ctx, tx, r.id, r.rel); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.embMu.Lock()
	s.embs = map[int64][]float32{}
	s.embInit = true
	s.embMu.Unlock()
	return nil
}

// ResetEmbeddings clears only caption embeddings (e.g. after switching the
// embedding model); the worker re-embeds existing captions.
func (s *Store) ResetEmbeddings(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM item_embeddings`); err != nil {
		return err
	}
	s.embMu.Lock()
	s.embs = map[int64][]float32{}
	s.embInit = true
	s.embMu.Unlock()
	return nil
}
