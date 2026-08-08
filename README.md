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
│   ├── face/                 # client for the optional recognition service
│   ├── faces/                # face worker: detect, cluster, name (People page)
│   └── server/               # minimal HTTP API
├── facerec/                  # the recognition service itself (separate container)
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
| `SCAN_SETTLE`   | `60s`    | Ignore new files modified this recently (still being written); `0` disables |
| `THUMB_PX`      | `400`    | Thumbnail longest edge (px)                         |
| `HASH_MODE`     | `partial`| Content fingerprint: `off` / `partial` / `full`     |
| `SIDECAR_DIR`   | *(empty)*| Where tag sidecars live; empty = next to the media  |
| `API_KEY`       | *(empty)*| Shared key gating every `/api/` route; empty = no auth |
| `AUTH_TTL`      | `720h`   | How long a browser session lasts (30 days)          |
| `PREVIEW_FORMAT`| `webp`   | Animated video previews: `webp` / `gif` / `off`      |
| `PREVIEW_PX`    | `320`    | Preview longest edge (px)                            |
| `PREVIEW_SEGMENTS`| `12`   | Max sampled bursts per video                         |
| `PREVIEW_SEG_FRAMES`| `6`  | Frames per burst                                     |
| `PREVIEW_SEG_EVERY`| `20`  | Seconds of video per burst, before the cap applies   |
| `PREVIEW_FPS`   | `8`      | Preview playback rate                                |
| `PREVIEW_SPEED` | `1`      | Burst speed; `1` = real time, `2` = 2x (time-lapse)  |
| `PREVIEW_QUALITY`| `55`    | WebP quality 0–100 (ignored for gif)                 |
| `PREVIEW_MIN_DUR`| `5`     | Skip videos shorter than this (seconds)              |
| `FACE_URL`      | *(empty)*| Face-recognition service (own container); empty disables the People page |
| `FACE_MATCH`    | *(empty)*| Cosine threshold for "same person"; empty = use what the service advertises |
| `FACE_FRAME_PX` | `1280`   | Frame size sent to the detector (bigger finds smaller faces) |
| `FACE_MAX_PER_ITEM` | `20` | Keep at most this many faces per item (highest-scoring) |
| `FFMPEG`        | `ffmpeg` | ffmpeg binary                                       |
| `FFPROBE`       | `ffprobe`| ffprobe binary                                      |

### Animated video previews

A poster frame tells you a video exists; it doesn't tell you what's in it. So
each video also gets a small looping animation built from several short bursts of
frames sampled at evenly spaced points across the *whole* duration — a 10-minute
video yields ~0.75s from 25s in, then from 75s in, and so on, twelve times. The
result plays in a plain `<img>`, so the grid needs no JS to animate it and no
hover to trigger it.

Notes:

- **WebP, not GIF, by default.** Same autoplay-in-an-`<img>` behaviour, but full
  colour instead of a 256-entry palette and 2–5x smaller for identical frames
  (measured: ~120–280 KB per preview vs ~620–730 KB as GIF). `PREVIEW_FORMAT=gif`
  is there if you need it.
- **Cost.** Generation adds `PREVIEW_SEGMENTS` seeks per video on top of the
  thumbnail, so the first scan over a large video library takes noticeably
  longer. It's resumable — progress is recorded per file — and
  `PREVIEW_FORMAT=off` skips it entirely. Each burst is a separate input seek
  rather than one decode pass with a `select` filter, specifically so a 4 GB
  video isn't pulled across the network mount in full.
- **Playback is a client choice.** The grid has a `Previews` toggle: `auto` plays
  only the cells currently on screen (capped at 16 simultaneous animations),
  `hover` plays just the one under the cursor, `off` disables it. The setting
  persists per browser and defaults to `hover` when the OS requests reduced
  motion. Because the file is pre-baked, switching modes costs nothing.
- Previews live in `DATA_DIR/previews`; deleting that directory rebuilds them on
  the next scan. Changing `PREVIEW_FORMAT` orphans the old files, so clear the
  directory when you switch.

### Poster-frame offset

By default a video's representative frame is taken 2 seconds in — far enough to
clear a fade from black. When that lands on something useless (a title card, a
dark shot), set a per-video offset in the viewer: type a number of seconds, or
scrub to the frame you want and press **Use current frame**.

The offset drives three things that must agree: the grid thumbnail, the frame
sent to the vision model for captioning, and the perceptual signature behind
"looks similar". It is stored in the file's sidecar, not the database, so it
survives a cache rebuild like tags do. Applying it rebuilds the thumbnail
immediately; the signature refreshes on the next scan.

**Regenerate caption** in the same panel re-runs the vision model for that one
item, for when a single caption is wrong and resetting the whole library is not a
proportionate response. It overwrites the caption and refreshes its embedding, so
semantic search stops matching the text you just replaced.

### Truncated and incomplete files

`moov atom not found` in the scan log means a file has no container index. MP4
writes that index at the *end* of the file, so a copy that stopped early parses
fine right up to where the index should be. It is almost always an interrupted
download or a file copied while still being written — not a codec problem. Note
the error names the MP4 demuxer even for a `.webm` path, because ffprobe detects
containers by content rather than extension; such a file contains MP4 bytes.

Two things reduce the noise: files modified within `SCAN_SETTLE` are skipped until
they stop changing, and genuinely truncated files are summarised once per scan
instead of logged individually. They stay listed and downloadable but have no
dimensions or thumbnail. To find them, look for items with no preview; re-copying
the file fixes it.

## Access control

Set `API_KEY` and every `/api/` route requires it. There are no accounts — one
shared key, entered once per browser:

```yaml
environment:
  - API_KEY=paste-a-long-random-string-here   # e.g. `openssl rand -base64 24`
```

Leaving it empty keeps the old behaviour (open to anyone who can reach the port),
and the startup log says so in as many words, so a typo in the variable name
doesn't read as protection.

How it works, and why it isn't header-only: the browser loads thumbnails,
previews and video through `<img src>` and `<video src>`. Those requests come from
the browser's own loader, and JavaScript cannot attach a header to them — a
header-only key would authenticate the JSON endpoints and 401 every image and
video in the app. So entering the key exchanges it for a session **cookie**, which
the browser does attach to those loads. (Routing media through `fetch()` into blob
URLs would avoid the cookie but break video seeking, since it defeats HTTP Range
requests.)

- The UI shell at `/` is served without a key, because there has to be somewhere
  to type it. It contains no library data.
- The cookie holds a signed, expiring token — not the key itself — so the cookie
  jar never stores the master credential and the expiry can't be extended by
  editing it. It is `HttpOnly`, and `Secure` is set only when the request arrived
  over HTTPS (an unconditional `Secure` flag would make a plain-HTTP LAN server
  unusable).
- **Revoking access:** change `API_KEY` and restart. Every existing session dies,
  because tokens are signed with the key. **Lock** in the header only clears the
  current device.
- Scripts can skip the cookie and send `X-API-Key: <key>` or
  `Authorization: Bearer <key>` instead:
  ```bash
  curl -H "X-API-Key: $API_KEY" http://nas:8080/api/stats
  ```
- Ten wrong keys per minute per client gets a `429`, so the key can't be quietly
  brute-forced over the network.

This is a lock on the front door, not transport security: over plain HTTP the key
and your media still cross the network in the clear. On a home LAN that is usually
the accepted trade-off; if you expose this beyond it, put it behind a reverse proxy
with TLS.

## HTTP API

- `GET  /api/items?limit=&before=&after=` — list items newest-first; `before=<id>`
  pages older, `after=<id>` fetches newer (used to live-stream during a scan)
- `GET  /api/search?q=&limit=` — FTS5 search over path + tags
- `GET  /api/item?path=` — one item plus its tags (manual + ai)
- `PUT  /api/tags` — `{ "path": "...", "tags": ["a", "artist:b"] }`; writes the
  sidecar and syncs the DB immediately (no rescan needed)
- `GET  /api/preview/<relpath>` — cached animated preview (video only; the
  `preview_url` field in an item is empty until one exists)
- `POST /api/caption` — `{ "path": "..." }`; re-runs the vision model for that one
  item and overwrites its caption (also refreshes the caption embedding)
- `PUT  /api/offset` — `{ "path": "...", "offset": 12.5 }`; sets the poster-frame
  offset for a video and rebuilds its thumbnail. `null` restores the default
- `GET  /api/auth` — whether a key is required and whether this client has a
  session (unauthenticated; reveals nothing about the library)
- `POST /api/auth` — `{ "key": "..." }`; sets the session cookie
- `DELETE /api/auth` — clears the session cookie on this device
- `GET  /api/people` — named people + unnamed face groups (People page)
- `GET  /api/people/items?ref=` — the items one person / group appears in
- `PUT  /api/people` — `{ "ref": "f42", "name": "Jane" }` names a group (or renames
  `"ref": "p3"`); an existing name merges
- `DELETE /api/people?ref=p3` — forget a name (the faces stay)
- `POST /api/people/regroup` — `{ "ref": "f42" }` lowers *that one group's* match by
  0.1 (`"step"` overrides) and re-groups just there, pulling in a group that was
  the same person. Nothing is re-detected
- `DELETE /api/people/regroup?ref=f42` — put that group back under the global match
- `GET  /api/face/{id}` — cropped face thumbnail (cut on first request, then cached)
- `POST /api/faces/reset` — clear detections and re-run the recognition service
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

**On-demand (automatic):** when the browser fails to play a video or audio file
and `DERIVED_DIR` is set, mediavault transcodes it in the background (one job at
a time) and swaps it in when ready — the viewer shows a "Converting…" spinner
meanwhile. So opening an unplayable file just works after a short wait
(remuxable files are near-instant; re-encodes take longer). The result is cached
in `DERIVED_DIR`, so it's instant next time.

**Batch (optional):** to convert everything up front instead of waiting on first
open, run the bundled script (below).

Note: `DERIVED_DIR` is a **named Docker volume** (`mediavault-derived`) by
default, so derived files live in Docker's storage, not a host folder — inspect
them with `docker compose exec mediavault ls -R /derived`. If you'd rather manage
them on the host, change that volume to a bind mount in docker-compose.yml.

```bash
# from the repo, pointing at your real media + a writable output dir
MEDIA_ROOT=/mnt/nas/media DERIVED_DIR=/mnt/nas/derived ./scripts/transcode.sh
```

The script is also bundled in the image, so the easiest way is to run it inside
the running container — it reads MEDIA_ROOT/DERIVED_DIR from the environment, so
paths always match what the app indexes:

```bash
docker compose exec mediavault transcode
```

It runs in the foreground (progress per file) and is safe to re-run — up-to-date
outputs are skipped. Run it again after importing new media. Once a file has a
derived copy, it plays in the browser immediately; no re-scan needed.

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

## Search filters & playback controls

**Filter by type** from the search box with a `type:` token: `type:video`,
`type:image`, `type:audio`, or `type:gif` (a few synonyms like `photo`/`music`
work too). Use it alone (`type:video` → all videos) or with terms
(`type:video beach`). The **?** button next to the search box opens a short
syntax cheatsheet with clickable type filters.

**Random** also lives inside the viewer, not just the header — and it respects
the active search/type filter, so `type:video` + Random gives random videos.

**Autoplay next** (toggle in the viewer) advances to the next item
automatically. Videos and tracks advance when they finish; images and gifs (no
"ended" event) advance after a configurable **every N s** interval next to the
toggle — i.e. a slideshow. Both the toggle and the interval are remembered in
your browser. So a mixed sequence plays each video to its end and lingers N
seconds on each still, and `type:video` + Random + Autoplay is continuous random
video. Note: browsers may require a video be muted to auto-start without a fresh
click.

## Troubleshooting playback

- **Animated WebP shows no cover.** ffmpeg can't decode animated WebP, so no
  thumbnail is generated — the grid instead renders the WebP itself as its cover
  (browsers handle animated WebP natively). Nothing to do.
- **`.m4a` won't play.** It's probably Apple Lossless (ALAC), which browsers
  can't decode (plain AAC `.m4a` plays fine). `transcode` now checks the audio
  *codec*, not the extension, and converts ALAC/WMA/PCM audio to mp3 in
  `DERIVED_DIR`. Run `docker compose exec mediavault transcode`.
- **`.avi` / `.wmv` won't play.** They need a derived MP4. Check the startup log
  (`docker compose logs mediavault | grep "derived dir"`): if it says *(unset)*,
  your compose is missing `DERIVED_DIR` and the `mediavault-derived` volume — add
  them (see docker-compose.yml). Then run `docker compose exec mediavault
  transcode` and confirm the file appears, e.g. `docker compose exec mediavault
  ls /derived/<path>.mp4`.

## URL & history

The viewer is deep-linkable. Opening an item sets the URL to `#v=<relpath>`, so
browser Back closes it (and Forward reopens), and you can bookmark or share a
link straight to a specific item. Searches are reflected as `#q=<query>`, so a
search is bookmarkable too and Back steps search → browse. Paging within the
viewer updates the URL in place (one Back exits the viewer rather than stepping
through every item).

## A note on env vars in docker-compose

Use the map form or unquoted list form — do **not** wrap values in quotes in the
list form, or the quotes become part of the value:

```yaml
    environment:      # correct (list form, no quotes)
      - DERIVED_DIR=/derived
      - SIDECAR_DIR=/meta
      - HASH_MODE=partial
```

`- DERIVED_DIR='/derived'` makes the path literally `'/derived'`, so derived
files/sidecars miss their mounts (and re-transcode every restart). mediavault
now strips stray surrounding quotes defensively, but fixing the compose is best.


## More controls

- **Shuffle** (viewer toolbar, next to Autoplay) makes autoplay jump to a random
  item instead of the next one — a continuous shuffle across videos, images, and
  audio. Respects the active search/type filter, and is remembered in your browser.
- **Quick-add tags:** the tag editor shows your most-used tags as one-tap `+`
  chips (already-applied ones are hidden), so re-tagging is a click. Backed by
  `GET /api/tags/top`.

## Visual similarity ("Looks similar")

Each visual item gets a small **perceptual signature** computed with ffmpeg (no
ML model): a per-channel mean-centered color grid (layout/composition) blended
with a coarse color histogram (palette), L2-normalized. Videos are sampled a
couple of seconds in to skip black fade-ins. The viewer shows a **Looks similar**
strip — nearest neighbours by cosine similarity — great for near-duplicates,
burst shots, and same-scene frames. Click one to browse the set.

Signatures are computed during scanning and stored in SQLite; existing items are
backfilled on the next scan (a "Looks similar" strip may say it's still computing
until then). Search is a brute-force cosine scan in memory — fast for a personal
library, so **no external vector database is needed**. For *semantic* similarity
("find other photos of a dog" regardless of colour), you'd want neural (CLIP)
embeddings, which would justify a Python embedding sidecar + a vector DB like
Qdrant — a separate, heavier path I can add if you want that quality.

`GET /api/similar?path=<relpath>&limit=<n>`

## Natural-language search (optional, via Ollama)

Point `OLLAMA_URL` at an Ollama server running a **vision** model and mediavault
will caption your media and make it searchable in plain words. A background
worker (one image at a time — LLM inference is the bottleneck) captions
undescribed items over time: it grabs a frame (a couple of seconds in for video,
to skip fade-ins), sends it to Ollama, and stores the caption. Captions are
folded into the existing full-text search, so a query like `dog beach sunset`
matches what's *in* the media, and each caption shows in the viewer panel.

```yaml
    environment:
      - OLLAMA_URL=http://your-truenas-ip:11434
      - OLLAMA_MODEL=llava          # any vision model you've pulled (llama3.2-vision, moondream, qwen2.5-vl, ...)
      # - OLLAMA_PROMPT=...          # override the captioning prompt to tune results
```

| Env | Default | Meaning |
|-----|---------|---------|
| `OLLAMA_URL` | `""` (off) | Ollama base URL, e.g. `http://host:11434`. Empty disables captioning. |
| `OLLAMA_MODEL` | `llava` | Vision model (must be pulled on the Ollama host). |
| `OLLAMA_PROMPT` | (built-in) | Captioning instruction. |

This is **keyword** matching over rich captions. For *semantic* matching (query
"puppy by the ocean" finding a caption that says "dog on the beach"), the next
step is to also embed captions + the query with an Ollama embedding model
(e.g. `nomic-embed-text`) and rank by cosine — reusing the vector search already
in place. Ask and I'll add it.


## Semantic search (optional, adds to captions)

With captions enabled, also set `OLLAMA_EMBED_MODEL` to a text-embedding model
(e.g. `nomic-embed-text`) and search becomes **semantic**: each caption is
embedded, your query is embedded, and results are ranked by meaning — so
"puppy by the ocean" finds a caption that says "a dog on the beach", even with
no shared words.

```yaml
    environment:
      - OLLAMA_URL=http://your-truenas-ip:11434
      - OLLAMA_MODEL=llava                 # captions (vision)
      - OLLAMA_EMBED_MODEL=nomic-embed-text # semantic search (embeddings)
```

It's a **hybrid**: the search box still returns exact keyword/tag/`type:` matches
first (fast FTS), then fills in semantically-close captions behind them — so
precise searches keep working while vague ones find the right media anyway.
Query embedding is best-effort with a short timeout, so if the Ollama server is
slow or down you simply fall back to keyword results. The background worker
embeds captions as it goes (and backfills existing captions if you turn the
embedding model on later); matching is a brute-force cosine scan in memory,
reusing the vector search already in place — still no external vector DB.

## People (optional, via a recognition container)

Set `FACE_URL` and the header grows a **People** button: a page of everyone in
your library, each with a face thumbnail and a count, clicking through to
everything they appear in. The viewer's card also gains a **People** row — the
faces found in the item on screen, each linking to that person.

```yaml
    environment:
      - FACE_URL=http://facerec:8000
```

Recognition itself runs in a **separate container**, because face models mean
Python, native wheels and often a GPU — none of which belongs in a static Go
binary whose only other dependency is ffmpeg. `facerec/` is a working one (dlib,
CPU-only); `docker compose up -d --build facerec` after uncommenting its block —
or, on TrueNAS, build the image once and reference the tag, because a custom app
has no checkout to build from (`facerec/README.md` has the walkthrough).
It needs no media mount: mediavault extracts a frame with ffmpeg and posts the
JPEG. The HTTP contract is two endpoints and is documented in
`facerec/README.md` — implement it and you can swap in InsightFace, a GPU
service, or a hosted API without touching mediavault.

How it works:

- A background worker sends one frame per item to the service (the same
  poster-frame offset the thumbnail uses, so a video is recognised on the frame
  you see in the grid) and stores a box plus an embedding per face. It is the
  same shape as captioning: slow, resumable, and harmless when the service is
  down — the startup log says so and the page just stays empty.
- **Grouping** is greedy clustering by cosine similarity over those embeddings.
  It errs toward splitting one person into two groups rather than merging two
  people, because splitting is repairable in one click and merging isn't: naming
  two groups with the same name joins them.
- **The threshold is the whole behaviour**, and the right value belongs to the
  model — dlib wants ≈0.82 where ArcFace wants ≈0.4 for the same photographs. So
  the service advertises its own via `/health`, and `FACE_MATCH` overrides it if
  you disagree. Two people merged into one group means raise it; one person in
  four groups means lower it.
- **Widen** repairs a split without touching that global number. When one person
  is in two groups but everyone else is grouped correctly, lowering `FACE_MATCH`
  trades this split for merged strangers elsewhere — so **Widen** on the card
  drops *that group's* match by 0.1 and re-groups only there, pulling in any
  group now close enough. Nothing is re-detected: the embeddings are already
  stored and only the number they are compared at changes, so it is instant.
  Click it again to go looser (down to 0.1), or **Reset match** to undo. Either
  half of the split works — you don't have to guess which one is "the" group.
  A widened group is compared to another group by the average similarity of
  their faces, so the number on the card means what `FACE_MATCH` means, and only
  the groups it is actually that alike to join it.
  The widening is cache like the grouping it produces; naming the joined group is
  what makes the judgement durable.
- **Names are durable, groups are not.** A name is a human judgement, so it goes
  in `people.json` next to the tag sidecars — the same volume, the same backup
  story. Faces, groups and labels are cache: drop `index.db` and re-detection
  plus that file put every name back (each person's example faces are matched
  again, and everyone who looks like them is relabelled). Back up `people.json`
  along with your sidecars; the database you don't need to.
- **Forget** removes a name but keeps the faces, so the group is offered again.
- Face crops are cut on first view into `DATA_DIR/faces`; delete that directory
  and they are re-cut on demand.

`people.json` is a plain file you can read and repair by hand:

```json
{
  "people": [
    { "name": "Jane",
      "faces": [ { "path": "2019/beach/img_0042.jpg", "box": [0.31, 0.12, 0.18, 0.24] } ] }
  ]
}
```

`box` is `[x, y, w, h]` as fractions of the frame, so it survives a change of
`FACE_FRAME_PX`. Removing a person from this file removes them from the app on
the next pass; up to 32 example faces are kept per person (64 after a merge),
which is what re-attaches a name after a cache rebuild — anyone beyond that is
re-labelled by resemblance to the examples instead.

Changed the recognition model? Embeddings from different models aren't
comparable, so use **Re-detect all faces** in the search-help panel (or
`POST /api/faces/reset`). Names are kept and re-attached as detection catches up.

Not built, and worth knowing before you rely on this: there is no way to say
"this face is not that person" — a wrong grouping can only be dissolved with
**Forget** and renamed. Person names are also not folded into the search box; use
the People page (or add a `person:jane` tag yourself if you want that).

## Regenerating captions

Changed the vision model or `OLLAMA_PROMPT` and want fresh captions? Open the
search help (the **?** by the search box) and use **Regenerate all captions**
(clears every caption + embedding and re-captions from scratch) or **Re-embed
only** (keeps captions, recomputes embeddings — for when you change
`OLLAMA_EMBED_MODEL`). Both run in the background; the header's "captioning X/Y"
resets and climbs again. Equivalent API calls:

```bash
curl -X POST http://your-host:8090/api/captions/reset     # captions + embeddings
curl -X POST http://your-host:8090/api/embeddings/reset   # embeddings only
```

This is far lighter than deleting the database, which would also wipe
thumbnails, dimensions, similarity vectors, and view counts.
