# facerec — the recognition sidecar

An optional container that finds faces in a frame and returns an embedding for
each. mediavault points at it with `FACE_URL` and does the rest (grouping,
naming, the People page). Nothing here touches your media: mediavault extracts
the frame with ffmpeg and posts the JPEG, so this container needs no mounts.

```bash
docker compose up -d facerec          # first build compiles dlib: 5-15 min
curl localhost:8000/health
# {"status":"ok","model":"dlib-resnet-v1/hog","dim":128,"match":0.82}
```

Then set on the mediavault service:

```yaml
  - FACE_URL=http://facerec:8000
```

## The contract

Two endpoints. Reimplement them and you can swap the model out without touching
mediavault:

```
POST /detect     body: JPEG bytes, Content-Type: image/jpeg
  200 {"model":"...","dim":128,"match":0.82,
       "faces":[{"box":[x,y,w,h],"score":1.0,"embedding":[...]}]}
  4xx  this frame is unusable  -> mediavault skips the item
  5xx  the service is unwell   -> mediavault backs off and retries later

GET /health
  200 {"status":"ok","model":"...","dim":128,"match":0.82}
```

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
| `FACE_UPSAMPLE` | `1` | Detection upsampling; `2` finds smaller faces at ~4x the cost |
| `FACE_MIN_SIZE` | `0.02` | Ignore faces narrower than this fraction of the frame — their embeddings are noise, and noise is what merges two people |
| `FACE_MATCH` | computed | Override the advertised threshold |
| `PORT` | `8000` | Listen port |

Symptoms and which knob:

- **Two people end up in one group** — threshold too low. Raise `FACE_MATCH` on
  *mediavault* (it overrides what this service advertises), then use
  **Re-detect all faces** or just re-group by naming again.
- **One person appears as several groups** — threshold too high, or their photos
  genuinely vary a lot. Lower `FACE_MATCH`, or simply give every group the same
  name: naming with an existing name merges into that person.
- **Faces are missed** — raise `FACE_UPSAMPLE`, try `FACE_MODEL=cnn`, or raise
  `FACE_FRAME_PX` on mediavault so the frame carries more pixels.

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
