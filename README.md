# mediavault

A bespoke, self-hosted media library for a personal collection of images,
gifs/webms, and video living on a NAS. Single static Go binary + ffmpeg. No
auth, single user, built to run as a TrueNAS custom app.

This repo is a **scaffold**: the indexing pipeline, storage layer, schema, and a
minimal HTTP API are real and wired together. The web UI and the optional AI
tagger are not included yet (see *What's stubbed* below).

## The one idea to keep in mind

There are three tiers of data, and they are strictly separated:

| Data                                   | Lives in                    | If you delete it        |
|----------------------------------------|-----------------------------|-------------------------|
| Pixels (the media itself)              | NAS, mounted **read-only**  | it's gone — back it up  |
| Human metadata (tags, rating, notes)   | **sidecar** `*.json` files  | it's gone — back it up  |
| Everything else (thumbs, dims, hashes, FTS) | **SQLite cache**       | rebuilt on next scan    |

The SQLite DB is a **disposable cache**. Every column in it is re-derivable from
(media files + sidecars), so you can delete `index.db` any time and a rescan
rebuilds it. The app enforces this itself: bump `store.schemaVersion` and it
drops the DB and re-applies `schema.sql` on next start. No migration framework.

The app **never writes to the media tree** except sidecars (and only ever a
sidecar, never the media file).

## Layout

```
mediavault/
├── cmd/server/main.go        # entrypoint: wire everything, run, shut down
├── internal/
│   ├── config/               # env-driven config + defaults
│   ├── model/                # domain types (no internal imports)
│   ├── store/                # the ONLY SQLite code
│   │   ├── schema.sql        # embedded; the whole DB
│   │   └── store.go          # open/WAL, auto-rebuild, batched writes, queries
│   ├── media/                # ffprobe (probe) + ffmpeg (thumbnail) + mime
│   ├── sidecar/              # read/write sidecars + chain.json
│   ├── scan/                 # THE worker pool (walk → workers → single writer)
│   └── server/               # minimal HTTP API
├── Dockerfile                # multi-stage, CGO-free, ffmpeg in runtime
├── docker-compose.yml        # media :ro + writable data volume
└── go.mod                    # only dep: modernc.org/sqlite (pure Go)
```

## How the scanner works

```
walk (1 goroutine)                  stat only; (size,mtime) change-detection
   → jobs channel
      → workers (N, bounded)         read file + ffmpeg; never touch the DB
         → results channel
            → writer (1 goroutine)   the only DB writer; batches into txns
```

- **Change-detection is a `stat`**, never a read: `(relpath, size, mtime)`.
  Unchanged files are skipped entirely, so a rescan reads ~0 bytes over SMB.
- **One writer goroutine** owns all DB writes → no `SQLITE_BUSY`, and writes
  batch into transactions. Workers only read/thumbnail.
- **Soft delete with a mount guard.** Vanished files get a `missing_since`
  stamp (tags/associations survive a NAS blip). If a scan sees *zero* files
  while the index has some, it assumes an unmount and skips pruning.
- **Content hashing is cheap by default** (`HASH_MODE=partial`): a
  head+tail+size fingerprint that reads at most 128 KiB per file regardless of
  size. `full` reads every byte (strong but slow over SMB); `off` skips it.
  It's for dedup/move-detection only — change-detection uses (size, mtime) —
  and nothing consumes it yet, so `off` is fine if you don't need it.

## Run

```bash
# 1. Pin dependencies (needs network once; generates go.sum).
go mod tidy

# 2a. Locally:
MEDIA_ROOT=/path/to/media DATA_DIR=./data go run ./cmd/server
#     → http://localhost:8080/api/items

# 2b. Or in Docker (edit the media mount in docker-compose.yml first):
docker compose up --build
```

## Config (env vars)

| Var             | Default  | Meaning                                             |
|-----------------|----------|-----------------------------------------------------|
| `MEDIA_ROOT`    | `/media` | Read-only media root                                |
| `DATA_DIR`      | `/data`  | Writable cache (DB + thumbnails)                    |
| `ADDR`          | `:8080`  | HTTP listen address                                 |
| `SCAN_WORKERS`  | `6`      | Worker-pool size (SMB is the bottleneck; keep 4–8)  |
| `SCAN_INTERVAL` | `5m`     | Periodic rescan; `0` disables the ticker            |
| `THUMB_PX`      | `400`    | Thumbnail longest edge (px)                         |
| `HASH_MODE`     | `partial`| Content fingerprint: `off` / `partial` / `full`     |
| `FFMPEG`        | `ffmpeg` | ffmpeg binary                                       |
| `FFPROBE`       | `ffprobe`| ffprobe binary                                      |

## HTTP API

- `GET  /api/items?limit=&offset=` — list items (JSON DTOs with thumb/raw URLs)
- `GET  /api/search?q=&limit=` — FTS5 search over path + tags
- `POST /api/scan` — trigger a rescan (returns 202; overlapping calls collapse)
- `GET  /api/thumb/{path...}` — cached thumbnail
- `GET  /api/raw/{path...}` — original file, **served with Range** (video seeks)

## Sidecar format

`photo.jpg.json`, next to `photo.jpg`:

```json
{
  "tags": ["landscape", "artist:jane_doe", "character:mika"],
  "rating": 4,
  "notes": "golden hour, shot on the K1000"
}
```

Tags may use a `namespace:name` prefix. This file is the source of truth for
manual tags; a rescan mirrors it into the DB for querying.

## Chains (your "story" reader)

Any folder with 2+ images is a chain, ordered by filename. Drop a `chain.json`
in the folder to override:

```json
{
  "title": "Chapter 3 — The Descent",
  "order": ["cover.png", "01.png", "02.png", "03.png"]
}
```

Files not listed in `order` are appended after the listed ones. Chains are
fully derived from the filesystem and rebuilt on every scan.

## What's stubbed / next steps

- **No web UI yet.** The API is the seam; a SPA (grid, viewer, tag editor, and
  the chain/comic reader) plugs in on top. Your existing Go comic reader's
  paging maps onto `GET /api/items` for a chain's ordered members.
- **No AI tagger.** By design it's a *separate, optional* service: a small
  Python container (e.g. WD14/CLIP via ONNX) that reads an image and writes
  `source='ai'` tags (with confidence) — the schema already distinguishes AI
  suggestions from manual confirmations. The core app works with zero AI.
- **Sidecar-only edits** (editing a `.json` without touching the media file)
  aren't yet picked up by the cheap change-detector. Options: include the
  sidecar's mtime in the change key, or add a lightweight tag-only sync pass.

## Notes / honesty

This scaffold was formatted with `gofmt`; the pure-Go packages type-check, but
it was **not** fully compiled in the environment it was written in (no Go
toolchain, and the module proxy was blocked, so the SQLite driver couldn't be
fetched). Run `go mod tidy && go build ./...` to finish wiring it up. The only
external dependency is `modernc.org/sqlite`.
