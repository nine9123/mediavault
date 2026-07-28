-- schema.sql — the entire DB. Every row here is DERIVED from (media files +
-- sidecars), so this database is a disposable cache: delete it and a rescan
-- rebuilds it. Human-authored data (tags, ratings, chain order) lives in
-- sidecar files in the media tree, NOT here — this is just a queryable mirror.
--
-- Migrations: none. store.schemaVersion is bumped whenever this file changes;
-- on mismatch the app DROPs everything and re-applies this file, then scans.

PRAGMA foreign_keys = ON;

-- key/value bag; holds schema_version so the app can detect a stale cache.
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per media file.
CREATE TABLE IF NOT EXISTS items (
    id            INTEGER PRIMARY KEY,        -- internal; NOT stable across rebuilds
    relpath       TEXT    NOT NULL UNIQUE,    -- path under media root; the stable address
    size          INTEGER NOT NULL,           -- bytes  }- together, the cheap change key:
    mtime         INTEGER NOT NULL,           -- unix s }- a stat, no bytes read
    content_hash  TEXT,                       -- nullable; dedup + move-detection only
    type          TEXT    NOT NULL,           -- image | animated | video
    mime          TEXT,
    width         INTEGER,
    height        INTEGER,
    duration      REAL,                       -- seconds; video only
    thumb_ready   INTEGER NOT NULL DEFAULT 0, -- 0/1
    preview_ready INTEGER NOT NULL DEFAULT 0, -- 0/1; animated preview exists
    views         INTEGER NOT NULL DEFAULT 0, -- times opened in the viewer (popularity signal)
    description   TEXT,                        -- AI caption (optional); indexed into items_fts
    indexed_at    INTEGER NOT NULL,           -- unix s
    seen_at       INTEGER NOT NULL,           -- unix s; last scan that saw this file
    missing_since INTEGER                     -- set when a scan stops seeing it (soft delete)
);
CREATE INDEX IF NOT EXISTS idx_items_hash    ON items(content_hash);
CREATE INDEX IF NOT EXISTS idx_items_type    ON items(type);
CREATE INDEX IF NOT EXISTS idx_items_missing ON items(missing_since);

-- Tag vocabulary. namespace is optional booru-style grouping ("" if none).
CREATE TABLE IF NOT EXISTS tags (
    id        INTEGER PRIMARY KEY,
    namespace TEXT NOT NULL DEFAULT '',
    name      TEXT NOT NULL,
    UNIQUE(namespace, name)
);

-- Which tags apply to which items. source lets a machine SUGGESTION and a
-- human CONFIRMATION of the same tag coexist as separate rows.
CREATE TABLE IF NOT EXISTS item_tags (
    item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    tag_id     INTEGER NOT NULL REFERENCES tags(id)  ON DELETE CASCADE,
    source     TEXT    NOT NULL,               -- manual | ai
    confidence REAL,                           -- ai only
    PRIMARY KEY(item_id, tag_id, source)
);
CREATE INDEX IF NOT EXISTS idx_item_tags_tag ON item_tags(tag_id);

-- A chain = an ordered "story". Fully derived from the filesystem each scan.
CREATE TABLE IF NOT EXISTS chains (
    id     INTEGER PRIMARY KEY,
    key    TEXT NOT NULL UNIQUE,               -- folder relpath (or id from chain.json)
    title  TEXT NOT NULL,
    kind   TEXT NOT NULL,                      -- folder | manifest
    source TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chain_items (
    chain_id INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
    item_id  INTEGER NOT NULL REFERENCES items(id)  ON DELETE CASCADE,
    position INTEGER NOT NULL,                 -- reading order within the chain
    PRIMARY KEY(chain_id, item_id)
);
CREATE INDEX IF NOT EXISTS idx_chain_items_order ON chain_items(chain_id, position);

-- Observability for the "Rescan now" button and periodic runs.
CREATE TABLE IF NOT EXISTS scan_runs (
    id         INTEGER PRIMARY KEY,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    seen       INTEGER NOT NULL DEFAULT 0,
    added      INTEGER NOT NULL DEFAULT 0,
    updated    INTEGER NOT NULL DEFAULT 0,
    missing    INTEGER NOT NULL DEFAULT 0,
    errors     INTEGER NOT NULL DEFAULT 0
);

-- Full-text search over path + tags. At a few thousand rows a LIKE scan is
-- also fine; FTS5 keeps search instant as the library grows. Maintained
-- explicitly by the app with rowid = items.id (see store.WriteBatch).
-- Perceptual signatures for visual similarity search (color + layout). BLOB is
-- little-endian float32s; brute-forced in memory, no external vector DB needed.
CREATE TABLE IF NOT EXISTS item_vectors (
    item_id INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    dim     INTEGER NOT NULL,
    vec     BLOB    NOT NULL
);

-- Caption text embeddings for semantic natural-language search (Ollama).
CREATE TABLE IF NOT EXISTS item_embeddings (
    item_id INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    dim     INTEGER NOT NULL,
    vec     BLOB    NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(relpath, tags);
