# facerec — the recognition sidecar

An optional container that finds faces in a frame and returns an embedding for
each. mediavault points at it with `FACE_URL` and does the rest (grouping,
naming, the People page). Nothing here touches your media: mediavault extracts
the frame with ffmpeg and posts the JPEG, so this container needs no mounts.

From a clone, where `docker compose` can see the `facerec/` directory:

```bash
docker compose up -d facerec          # first build compiles dlib: 5-15 min
curl localhost:8000/health
# {"status":"ok","model":"dlib-resnet-v1/hog","dim":128,"match":0.82}
```

Then set on the mediavault service:

```yaml
  - FACE_URL=http://facerec:8000
```

On TrueNAS there is no clone for compose to see — read the next section.

## Running it as a TrueNAS custom app

**`build: ./facerec` cannot work here**, and it fails in a way that points at the
wrong thing. A custom app's YAML is rendered into a directory the Apps UI
generates and `docker compose` runs *there*, not in a checkout of this repo, so
the relative context `./facerec` doesn't exist. Compose then falls back to
treating `facerec` as an image name and you get:

```
pull access denied for facerec, repository does not exist or may require 'docker login'
```

The Dockerfile is fine. A custom app can only *reference* images, so build the
image first and point `image:` at the tag.

### 1. Build the image

Simplest, if you don't run a registry — build it on the NAS over SSH:

```bash
git clone <this repo> /mnt/<pool>/src/mediavault
docker build -t mediavault-facerec:1 /mnt/<pool>/src/mediavault/facerec
```

Expect 5–15 minutes and a couple of GB of scratch space in the Docker root on
your apps pool: dlib compiles from source. If the build dies without a useful
message it is almost always the C++ compile running out of memory.

Or build it wherever you already build the mediavault image and push it:

```bash
docker build -t <registry>/<you>/mediavault-facerec:1 ./facerec
docker push  <registry>/<you>/mediavault-facerec:1
```

The registry route is the durable one, and the one iX supports. An image built
by hand on the NAS exists only on that host and is not something the Apps UI
knows about, so an image cleanup or a rebuilt pool takes it with it and the app
won't start until you build it again.

Tag a version rather than `latest`. Embeddings are only comparable within one
build of one model, so an image you can pin — and roll back to — is worth more
here than an image that quietly moves under you (see *Changing models*).

### 2. Reference the tag from the app YAML

Apps → Discover Apps → Custom App → **Install via YAML**. Put mediavault and
facerec in the **same** custom app: one app is one compose project on one
network, which is what makes the hostname `facerec` resolve.

```yaml
services:
  mediavault:
    image: <registry>/<you>/mediavault:latest
    ports:
      - "8080:8080"
    environment:
      FACE_URL: "http://facerec:8000"
      SIDECAR_DIR: "/meta"
      # ... the rest of your existing config
    volumes:
      - /mnt/<pool>/media:/media:ro
      - /mnt/<pool>/apps/mediavault/data:/data
      - /mnt/<pool>/apps/mediavault/meta:/meta

  facerec:
    image: mediavault-facerec:1
    # Locally built tag: tell compose not to go looking for it in a registry.
    # Drop this line if you pushed the image somewhere.
    pull_policy: never
    restart: unless-stopped
    environment:
      FACE_MODEL: "hog"       # or "cnn": better on small/turned faces, much slower
      # Everything else defaults per-model — see Tuning. If you set FACE_MODEL=cnn,
      # read that section before adding FACE_UPSAMPLE here: it is the difference
      # between a container that uses a gigabyte and one that uses ten.
    # No ports and no volumes on purpose: only mediavault needs to reach it, and
    # it never touches your library — mediavault posts the frame in the request.
```

Create the host directories (as datasets) before installing; the app won't start
against a path that isn't there.

If install still fails with a pull error for `mediavault-facerec`, the deploy
path is running a pull that ignores `pull_policy`. Push the image to a registry —
any registry, including one on your LAN — and reference it by its full name.
That route has no local-only tag to trip over.

If you would rather keep facerec as its own custom app, the two are separate
compose projects on separate networks and `http://facerec:8000` will not
resolve. Publish the port on the facerec app:

```yaml
    ports:
      - "8000:8000"
```

and set `FACE_URL: "http://<nas-lan-ip>:8000"` on mediavault. That puts an
unauthenticated service on your LAN — it takes a JPEG and returns boxes and
vectors, so the exposure is small, but it isn't nothing. One app is the better
default.

### 3. Check it came up

With the port published, from anywhere on the LAN:

```bash
curl http://<nas-lan-ip>:8000/health
# {"status":"ok","model":"dlib-resnet-v1/hog","dim":128,"match":0.82}
```

Without it, the mediavault log is the check — the app's **Logs** view should
show, a moment after start:

```
faces: on — recognition service at http://facerec:8000
faces: service reachable — model "dlib-resnet-v1/hog", 128-dim embeddings, matching at cosine >= 0.82
```

A few `faces: NOT reachable … retrying in 1s` lines right after a start are
normal: facerec takes a few seconds longer to load its model, and mediavault
keeps retrying (backing off to every 30 s) until it answers. If they never stop,
the two containers aren't on one network (see above) or the image never started;
everything else in mediavault carries on regardless, and the People page just
stays empty until it resolves.

### Updating

Same shape as the build: build and push (or rebuild on the NAS) a new tag, edit
the app's YAML to point at it, save. If you pin `latest` and use watchtower for
mediavault, note it will not update a locally built image — there is nothing to
pull.

## The contract

Two endpoints. Reimplement them and you can swap the model out without touching
mediavault:

```
POST /detect     body: JPEG bytes, Content-Type: image/jpeg
  200 {"model":"...","dim":128,"match":0.82,
       "faces":[{"box":[x,y,w,h],"score":1.0,"embedding":[...]}]}
  4xx  this frame is unusable  -> mediavault skips the item
  5xx  the service is unwell   -> mediavault backs off and retries later

GET /health      (or /healthz)
  200 {"status":"ok","model":"...","dim":128,"match":0.82}
```

mediavault retries `/health` with backoff (1 s up to 30 s) until it answers, so
a service that is still loading its model is waited for rather than given up on.
Bind the port only once the model has loaded, or answer 503 until then.

- **Stopping** must exit 0 within 5 s of SIGTERM (and SIGINT). idlegate stops
  this container whenever the library goes idle; a container killed after
  Docker's 10 s grace exits 137, and TrueNAS then shows the whole app as
  *Crashed*. Here `shutdown.py` handles it, and `test_shutdown.py` checks it:
  `python facerec/test_shutdown.py` locally (dlib is stubbed if missing), or
  `docker run --rm -i --entrypoint python <image> - < facerec/test_shutdown.py`
  against a built image, as CI does.

- `box` is `[x, y, w, h]` as **fractions of the frame**, not pixels. mediavault
  downscales the frame it sends (`FACE_FRAME_PX`) but crops faces from the
  original file, so pixel coordinates would need a scale factor this service
  doesn't know.
- `embedding` should be comparable by cosine similarity. Length is up to the
  model; mediavault normalizes and only ever compares vectors of equal length,
  so changing models mid-library is safe — but see *Changing models* below.
- `match` is the cosine similarity at which **this model** considers two faces
  the same person. mediavault uses it unless `FACE_MATCH` is set, because the
  correct value is a property of the model and not something the app can guess:
  dlib wants ≈0.82 and ArcFace ≈0.4 for the same photographs.
- `score` is per-face confidence, used only to order faces and pick a group's
  cover image. Return `1.0` if your detector doesn't provide one.

## Tuning

| Env | Default | Meaning |
|-----|---------|---------|
| `FACE_MODEL` | `hog` | `hog` (fast, CPU) or `cnn` (better on small/turned faces, much slower) |
| `FACE_UPSAMPLE` | `1`, or `0` for cnn | Detection upsampling; each step finds smaller faces at ~4x the cost |
| `FACE_DETECT_PX` | `2560`, or `960` for cnn | Longest edge the detector may process, **after** upsampling. The frame is shrunk to fit before it is handed over |
| `FACE_CONCURRENCY` | `2`, or `1` for cnn | Detections at a time. Each one holds its own copy of the memory below |
| `FACE_MIN_SIZE` | `0.02` | Ignore faces narrower than this fraction of the frame — their embeddings are noise, and noise is what merges two people |
| `FACE_MATCH` | computed | Override the advertised threshold |
| `PORT` | `8000` | Listen port |

### The one that decides how much this costs

Detection time and memory go with the **pixel count the detector processes**,
which is not the size of the frame that arrived: `FACE_UPSAMPLE` doubles each
edge per step, so upsample 1 on a 1280px frame is a 2560px detection — four
times the area. With `hog` that is seconds. With `cnn` it is minutes and *many*
gigabytes, because dlib's CNN detector holds intermediate tensors for the whole
image at once. Setting `FACE_MODEL=cnn` and leaving everything else alone is
what puts this container at 10 GB and a few frames a minute.

`FACE_DETECT_PX` is the ceiling on that number. The frame is shrunk to
`FACE_DETECT_PX / 2^FACE_UPSAMPLE` before the detector sees it, so memory is
bounded no matter what `FACE_FRAME_PX` and `FACE_UPSAMPLE` say. Faces are still
*described* from the full-resolution frame — the descriptor works on a 150px chip
of the face, so that costs almost nothing and keeps the embeddings good.

The startup line tells you where you landed, and every frame logs what the
detector actually looked at and how long it took:

```
facerec: listening on :8000 — model dlib-resnet-v1/cnn, upsample 0, up to 960 px of detector input (frames shrunk to 960 px first), 1 at a time, advertising match 0.82
facerec: detect: 2 face(s) in 1.4s (960x540 x1)
```

If those lines say seconds, detection of a whole library is hours. If they say
minutes, lower `FACE_DETECT_PX` (or use `hog`) — mediavault gives up on a frame
after `FACE_TIMEOUT` and skips it, so a service this slow silently loses faces.

Symptoms and which knob:

- **Two people end up in one group** — threshold too low. Raise `FACE_MATCH` on
  *mediavault* (it overrides what this service advertises), then use
  **Re-detect all faces** or just re-group by naming again.
- **One person appears as several groups** — threshold too high, or their photos
  genuinely vary a lot. Lower `FACE_MATCH`, or simply give every group the same
  name: naming with an existing name merges into that person.
- **Faces are missed** — raise `FACE_DETECT_PX`, then `FACE_UPSAMPLE`, then try
  `FACE_MODEL=cnn`; and raise `FACE_FRAME_PX` on mediavault if the frame it sends
  is now the smaller of the two.
- **The container is enormous, or detection crawls** — lower `FACE_DETECT_PX`,
  and check `FACE_UPSAMPLE` is 0 if you are on `cnn`. Halving the first roughly
  quarters both.

## Changing models

Embeddings from different models are not comparable. After switching, run
**Re-detect all faces** (search help panel, or `POST /api/faces/reset`).
mediavault keeps `people.json`, so names come back — each person's example faces
are re-detected under the new model and everyone matching them is relabelled.

## Alternatives

This is a reference, not a recommendation. InsightFace (`buffalo_l`) is
noticeably more accurate and has ONNX runtimes including GPU; a service wrapping
it needs to implement the same two endpoints, return `dim: 512` and advertise a
`match` around 0.4. `deepface` and `dlib`-via-`cnn` are other options. The
mediavault side is model-agnostic.
