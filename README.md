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
| `SIDECAR_DIR`   | *(empty)*| Where tag sidecars live; empty = next to the media  |
| `FFMPEG`        | `ffmpeg` | ffmpeg binary                                       |
| `FFPROBE`       | `ffprobe`| ffprobe binary                                      |

## HTTP API

- `GET  /api/items?limit=&before=&after=` — list items newest-first; `before=<id>`
  pages older, `after=<id>` fetches newer (used to live-stream during a scan)
- `GET  /api/search?q=&limit=` — FTS5 search over path + tags
- `GET  /api/item?path=` — one item plus its tags (manual + ai)
- `PUT  /api/tags` — `{ "path": "...", "tags": ["a", "artist:b"] }`; writes the
  sidecar and syncs the DB immediately (no rescan needed)
- `GET  /api/stats` — collection size + latest scan (drives the live UI)
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

**Editing:** open any item and edit its tags in the catalog panel — type a tag
and press Enter (comma-separate to add several), click `×` to remove one. Each
change writes the sidecar and updates the DB immediately, so it's searchable
right away without waiting for a scan. AI tags (if you later add the tagger) are
shown but not editable here. Click any tag to search for it.

**Search:** the search box runs an FTS5 prefix match over both the file path and
the tags, so typing a tag name (including a `namespace:name` tag) finds it.

**Where sidecars live** is set by `SIDECAR_DIR`:

- **empty** — beside the media (`photo.jpg.json`). Portable and moves with the
  file, but requires the media mount to be **writable**.
- **set to a path** (the compose default, `/meta` on its own volume) — sidecars
  are mirrored there by relative path, so the **media mount stays read-only**.
  Back this volume up; it holds your only copy of manual tags/ratings.

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

## Making everything playable in the browser

The built-in `<video>`/`<img>` viewer can only show what the browser itself
decodes: **MP4/H.264, WebM (VP8/VP9/AV1), Ogg, and common still images
(JPEG/PNG/GIF/WebP/AVIF/SVG)**. Anything else — an **HEVC/H.265** `.mp4`, an
`.avi` (DivX/Xvid), `.mkv`, `.wmv`, `.flv`, or a still like **HEIC**, **TIFF**,
**JPEG-2000**, or camera **RAW** — is still indexed and thumbnailed, but the
viewer will show a fallback with an *"open / download original"* link instead of
playing it.

To make those play/show in-browser, generate browser-friendly copies into a
separate writable directory (`DERIVED_DIR`) and mediavault will serve the derived
file whenever one exists. Your original archive is never touched.

```bash
# from the repo, pointing at your real media + a writable output dir
MEDIA_ROOT=/mnt/nas/media DERIVED_DIR=/mnt/nas/derived ./scripts/transcode.sh
```

Or run it against the same volumes the container uses, e.g. a throwaway ffmpeg
container mounting media read-only and the derived volume read-write.

What the script does, cheaply:

- **Skips** files already playable (mp4/webm with a web codec) — no copy made.
- **Remuxes** (`-c copy`, fast + lossless) when the streams are web-safe but the
  container isn't — e.g. H.264 + AAC inside an `.mkv`/`.mov`/`.avi`.
- **Re-encodes** to H.264/AAC only when the codec itself isn't web-playable
  (HEVC, DivX, VC-1, ...).
- Writes a downscaled **JPEG preview** for non-web still formats (HEIC, TIFF,
  JP2, ...). Most camera **RAW** needs external tooling (ffmpeg can't decode it),
  so those may remain preview-less.

Re-running is safe — up-to-date outputs are skipped.

### Which format to keep?

Keep your **originals** as the archive (they're the source of truth; don't
re-encode them and lose quality). For the *derived* playback copies, **MP4 /
H.264 (High, 8-bit yuv420p) + AAC** is the best default: it plays in every
browser, seeks well, and encodes fast. WebM (VP9/AV1) compresses smaller but
encodes much slower; prefer it only if disk space matters more than CPU time.

### Config for playback

| Env | Default | Meaning |
|-----|---------|---------|
| `DERIVED_DIR` | `""` (off) | Directory of derived copies (`<relpath>.mp4` / `.jpg`). When set, the raw endpoint serves a derived file if present. |

Supported extensions were also widened to cover many more still-image formats
(HEIC/HEIF, JFIF, JPEG-2000, TGA/EXR/HDR/DDS/PNM, SVG, and common camera RAW).
If part of your collection wasn't showing up before, a rescan will now pick it
up. Note: HEIC decoding depends on your ffmpeg build, and most RAW won't
thumbnail without external tooling — such files are still indexed, searchable,
and downloadable.

## Audio

Audio files (`mp3`, `m4a`, `aac`, `flac`, `wav`, `ogg`/`oga`, `opus`, `wma`,
`aiff`) are indexed alongside everything else. The file's **embedded cover art**
is extracted (via ffmpeg) and used as its thumbnail, so a music folder shows
album covers in the grid; files with no embedded art get a ♪ placeholder.
Opening one shows the cover with an audio player beneath it.

Most audio plays in the browser directly. `wma` (and some `aiff`) don't, so
`scripts/transcode.sh` converts those to an mp3 in `DERIVED_DIR`, which
mediavault then serves — same mechanism as video. Cover art is unaffected (it
comes from the thumbnail, not the derived audio).

## Discovery: Random + recommendations

**Random** (header button) opens a random item in the viewer. If a search is
active, it picks randomly from the matches. From there the viewer's arrow keys
pull further random items (with back-history), so it doubles as a lean-back
shuffle. Endpoint: `GET /api/random?q=<optional search>`.

**More like this** appears in every item's viewer panel. Ranking blends two
signals: **tag overlap** (manual + AI tags both count) and **popularity** — how
often items get opened. Each shared tag is worth 1 point; an item's view count
adds up to ~3 more (capped), so a frequently-opened item can edge ahead of one
with a couple more tags, without letting popularity override a much stronger tag
match. Click a result to open that whole set as an arrow-navigable queue, so
following related items chains naturally.

Views are recorded when an item stays open in the viewer for ~1s (flipping past
with the arrows doesn't count), and the count shows in the metadata panel. If an
item has no tags (or nothing overlaps), the strip falls back to your
most-viewed items, so it's useful even before you've tagged much. Endpoints:
`GET /api/recommend?path=<relpath>&limit=<n>` and `POST /api/view?path=<relpath>`.

View counts live in the cache DB (added by a non-destructive migration, so your
existing thumbnails/index are preserved on upgrade). They persist across
restarts but, unlike tags, are not written to sidecars — so a deliberate full DB
wipe resets them. Tell me if you'd rather they be durable and I'll sidecar them.
