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
    offset_s      REAL,                       -- poster-frame offset (s); mirrors the sidecar
    thumb_ready   INTEGER NOT NULL DEFAULT 0, -- 0/1
    preview_ready INTEGER NOT NULL DEFAULT 0, -- 0/1; animated preview exists
    views         INTEGER NOT NULL DEFAULT 0, -- times opened in the viewer (popularity signal)
    description   TEXT,                        -- AI caption (optional); indexed into items_fts
    faces_done    INTEGER NOT NULL DEFAULT 0, -- 0/1; face detection has run (0 faces is a valid result)
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

-- A named person. DERIVED, like everything else here: the durable record of who
-- is who is people.json next to the tag sidecars (see internal/sidecar). These
-- rows are re-created from that file, and the name is only mirrored here so
-- queries can join on it.
CREATE TABLE IF NOT EXISTS people (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE
);

-- One row per face found in one item, from the optional recognition service.
-- person_id is the clustering/labelling result, NULL while a face is unnamed.
CREATE TABLE IF NOT EXISTS faces (
    id        INTEGER PRIMARY KEY,
    item_id   INTEGER NOT NULL REFERENCES items(id)  ON DELETE CASCADE,
    person_id INTEGER          REFERENCES people(id) ON DELETE SET NULL,
    x         REAL NOT NULL,               -- box as fractions of the frame, so it
    y         REAL NOT NULL,               -- stays valid if FACE_FRAME_PX changes
    w         REAL NOT NULL,
    h         REAL NOT NULL,
    score     REAL,                        -- detector confidence
    dim       INTEGER NOT NULL,
    vec       BLOB    NOT NULL             -- little-endian float32s, L2-normalized
);
CREATE INDEX IF NOT EXISTS idx_faces_item   ON faces(item_id);
CREATE INDEX IF NOT EXISTS idx_faces_person ON faces(person_id);

-- Per-group similarity overrides: "this group was split from another; group it
-- at a lower threshold than the rest of the library". face_id is the group's key
-- face — groups have no id, so that is the only handle they have. Cache, like
-- everything else here: a widening is a step towards naming the group, and the
-- name is what gets written to people.json.
CREATE TABLE IF NOT EXISTS face_group_match (
    face_id INTEGER PRIMARY KEY REFERENCES faces(id) ON DELETE CASCADE,
    match   REAL NOT NULL              -- cosine threshold, below the global one
);

CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(relpath, tags);
