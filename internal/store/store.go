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
	"unicode"

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
const itemCols = "id,relpath,size,mtime,content_hash,type,mime,width,height,duration,thumb_ready,views,indexed_at,seen_at,missing_since"

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
	ID         int64
	Size       int64
	MTime      int64
	ThumbReady bool
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
		`SELECT relpath,id,size,mtime,thumb_ready FROM items WHERE missing_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]IndexRow)
	for rows.Next() {
		var rp string
		var r IndexRow
		var thumb int64
		if err := rows.Scan(&rp, &r.ID, &r.Size, &r.MTime, &thumb); err != nil {
			return nil, err
		}
		r.ThumbReady = thumb != 0
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

		// Keep FTS in sync from ALL of the item's tags (manual + any ai), so
		// search finds either kind. FTS5 has no upsert; rebuildItemFTS does
		// delete+insert.
		if err := rebuildItemFTS(ctx, tx, id, it.Relpath); err != nil {
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
		`SELECT `+itemCols+` FROM items WHERE missing_since IS NULL ORDER BY indexed_at DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// SearchItems runs an FTS5 query over path + tags.
func (s *Store) SearchItems(ctx context.Context, q string, limit int) ([]model.Item, error) {
	match := ftsQuery(q)
	if match == "" {
		return nil, nil // nothing searchable in the query
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixCols("i")+`
		FROM items_fts f JOIN items i ON i.id = f.rowid
		WHERE items_fts MATCH ? AND i.missing_since IS NULL
		ORDER BY f.rank
		LIMIT ?`, match, limit)
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
			views                    int64
			missing                  sql.NullInt64
		)
		if err := rows.Scan(
			&it.ID, &it.Relpath, &it.Size, &mtime, &hash, &it.Type, &mime,
			&width, &height, &dur, &thumb, &views, &indexedAt, &seenAt, &missing,
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
		it.Views = int(views)
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

	if _, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE rowid=?`, itemID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO items_fts(rowid,relpath,tags) VALUES(?,?,?)`,
		itemID, relpath, strings.Join(names, " "))
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
func (s *Store) RandomItem(ctx context.Context, q string) (item model.Item, found bool, err error) {
	var rows *sql.Rows
	if q != "" {
		match := ftsQuery(q)
		if match == "" {
			return model.Item{}, false, nil
		}
		rows, err = s.db.QueryContext(ctx, `
			SELECT `+prefixCols("i")+`
			FROM items_fts f JOIN items i ON i.id = f.rowid
			WHERE items_fts MATCH ? AND i.missing_since IS NULL
			ORDER BY RANDOM() LIMIT 1`, match)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT `+itemCols+` FROM items
			WHERE missing_since IS NULL
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
