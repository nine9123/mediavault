#!/usr/bin/env python3
"""A reference face-recognition service for mediavault.

mediavault talks to this over two endpoints and holds no other assumptions, so
swapping in a different model (InsightFace, a GPU service, a hosted API) means
reimplementing this file and nothing else:

    POST /detect   body: a JPEG frame
      -> {"model": "...", "dim": 128, "match": 0.82, "faces": [
             {"box": [x, y, w, h], "score": 1.0, "embedding": [...]}]}

    GET /health
      -> {"status": "ok", "model": "...", "dim": 128, "match": 0.82}

Two things about the contract are load-bearing:

  * box is [x, y, w, h] as FRACTIONS of the frame, not pixels. mediavault sends a
    downscaled frame and crops faces out of the original file, so pixel
    coordinates would have to be rescaled by a factor this service knows nothing
    about.
  * match is the cosine similarity above which THIS model considers two
    embeddings the same person. mediavault uses it unless FACE_MATCH overrides
    it, because the right value is a property of the model: dlib and ArcFace sit
    a factor of two apart.

This implementation uses dlib via face_recognition: CPU-only, no model download
at runtime, and good enough for a personal library.

The one thing worth understanding before changing anything here is that both the
time and the memory a detection takes are set by the PIXEL COUNT the detector
looks at, and that number is not the size of the frame that arrived — it is that
size doubled per step of FACE_UPSAMPLE, so a 1280px frame at upsample 1 is a
2560px detection, four times the area. With FACE_MODEL=cnn that is measured in
gigabytes: dlib's CNN detector holds its intermediate tensors for the whole image
at once, and people who set cnn and left the rest alone have watched this
container reach 10 GB and take minutes a frame.

So FACE_DETECT_PX is a budget on that final number, applied by shrinking the
frame before the detector ever sees it — bounded memory whatever the frame size
and upsampling say. Faces are then described from the full-resolution frame,
which costs almost nothing (the descriptor works on a 150px chip of the face, not
on the image) and keeps the embeddings good.

Environment:
    PORT             8000        listen port
    FACE_MODEL       hog         hog (fast, CPU) or cnn (slower, more accurate)
    FACE_UPSAMPLE    1 / 0       detection upsampling; 2 finds smaller faces at
                                 ~4x the cost. Defaults to 0 for cnn, which finds
                                 small faces well enough without it.
    FACE_DETECT_PX   2560 / 960  longest edge the detector may process, AFTER
                                 upsampling. Lower it if this container is short
                                 of memory; raise it to find smaller faces. The
                                 default is per-model (cnn is the small one).
    FACE_CONCURRENCY 2 / 1       detections at a time. Each one costs another
                                 copy of the memory above, so cnn defaults to 1.
    FACE_MATCH       (computed)  override the advertised cosine threshold
    FACE_MIN_SIZE    0.02        ignore faces whose width is under this fraction
                                 of the frame; their embeddings are mostly noise
"""

import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO

import face_recognition
import numpy as np
from PIL import Image

PORT = int(os.environ.get("PORT", "8000"))
MODEL = os.environ.get("FACE_MODEL", "hog").strip().lower()
if MODEL not in ("hog", "cnn"):
    MODEL = "hog"
# cnn's detector is a CNN trained to find small faces; upsampling on top of it
# quadruples the cost of the most expensive model for the least benefit, so it
# is off by default there and on for hog.
UPSAMPLE = int(os.environ.get("FACE_UPSAMPLE", "0" if MODEL == "cnn" else "1"))
UPSAMPLE = max(0, min(UPSAMPLE, 3))
DETECT_PX = int(os.environ.get("FACE_DETECT_PX", "960" if MODEL == "cnn" else "2560"))
DETECT_PX = max(160, DETECT_PX)
CONCURRENCY = max(1, int(os.environ.get("FACE_CONCURRENCY", "1" if MODEL == "cnn" else "2")))
MIN_SIZE = float(os.environ.get("FACE_MIN_SIZE", "0.02"))
MAX_BYTES = 32 * 1024 * 1024

# dlib's documented operating point is a Euclidean distance of 0.6 between its
# 128-d descriptors. Those descriptors are (near enough) unit length, and for unit
# vectors d^2 = 2 - 2cos, so 0.6 becomes a cosine similarity of 1 - 0.6^2/2.
# Tune with FACE_MATCH: raise it if two people get merged, lower it if one person
# turns up as several groups.
DEFAULT_MATCH = round(1.0 - (0.6 ** 2) / 2.0, 3)  # 0.82
MATCH = float(os.environ.get("FACE_MATCH", DEFAULT_MATCH))
MODEL_NAME = "dlib-resnet-v1/" + MODEL
DIM = 128

# The longest edge a frame is shrunk to before the detector gets it, so that what
# the detector ends up processing is DETECT_PX.
INPUT_PX = max(64, DETECT_PX // (2 ** UPSAMPLE))

# Detections in flight. dlib holds the whole image while it works, so this is
# also a memory multiplier — hence a small default rather than "one per core".
_slots = threading.BoundedSemaphore(CONCURRENCY)


def shrink(img):
    """Return (image the detector should see, its scale relative to img)."""
    w, h = img.size
    longest = max(w, h)
    if longest <= INPUT_PX:
        return img, 1.0
    s = INPUT_PX / float(longest)
    return img.resize((max(1, int(w * s)), max(1, int(h * s))), Image.BILINEAR), s


def upscale_box(box, factor, width, height):
    """Move a (top, right, bottom, left) box back onto the full-size frame."""
    top, right, bottom, left = box
    return (
        max(0, min(height, int(top * factor))),
        max(0, min(width, int(right * factor))),
        max(0, min(height, int(bottom * factor))),
        max(0, min(width, int(left * factor))),
    )


def detect(jpeg: bytes):
    """Return mediavault's face list for one JPEG frame."""
    img = Image.open(BytesIO(jpeg))
    # dlib wants 8-bit RGB; a CMYK scan or a greyscale frame would otherwise blow
    # up inside the detector rather than here.
    img = img.convert("RGB")
    width, height = img.size
    if not width or not height:
        return [], (0, 0)

    small, scale = shrink(img)
    sw, sh = small.size
    boxes = face_recognition.face_locations(
        np.asarray(small), number_of_times_to_upsample=UPSAMPLE, model=MODEL
    )
    keep = []
    for top, right, bottom, left in boxes:
        if (right - left) / float(sw) >= MIN_SIZE:
            keep.append((top, right, bottom, left))
    if not keep:
        return [], (sw, sh)

    # Describe the faces from the full-resolution frame rather than the shrunken
    # copy the detector used: the descriptor is computed from a small chip of the
    # face, so its cost doesn't depend on the size of the image around it, but its
    # quality very much depends on how many pixels that face had.
    if scale < 1.0:
        full = np.asarray(img)
        at_full = [upscale_box(b, 1.0 / scale, width, height) for b in keep]
    else:
        full, at_full = np.asarray(img), keep
    # num_jitters=1 keeps this fast; raising it re-samples each face and averages,
    # which is a little more accurate and linearly slower.
    encodings = face_recognition.face_encodings(full, known_face_locations=at_full, num_jitters=1)

    out = []
    for (top, right, bottom, left), enc in zip(keep, encodings):
        vec = np.asarray(enc, dtype=np.float64)
        norm = float(np.linalg.norm(vec))
        if norm == 0.0:
            continue
        vec = vec / norm  # mediavault compares by dot product
        out.append(
            {
                # Fractions of the detector's frame, which are the same fractions
                # of the original: mediavault crops from the file itself.
                "box": [
                    left / float(sw),
                    top / float(sh),
                    (right - left) / float(sw),
                    (bottom - top) / float(sh),
                ],
                # dlib's HOG/CNN detector gives no per-face confidence through
                # this API. 1.0 is honest about that: mediavault only uses score
                # to order faces, and falls back to box size on a tie.
                "score": 1.0,
                "embedding": [float(x) for x in vec],
            }
        )
    return out, (sw, sh)


class Handler(BaseHTTPRequestHandler):
    server_version = "mediavault-facerec/1.1"
    # Keep-alive: mediavault sends one frame per request and a whole library's
    # worth of them, and a TCP handshake per frame is pure overhead.
    protocol_version = "HTTP/1.1"

    def _json(self, code: int, payload: dict, close: bool = False) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if close:
            # Said explicitly, because under HTTP/1.1 the client is entitled to
            # assume otherwise — and this is the path where we are hanging up
            # with a request body still unread and the stream out of step.
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 (http.server's naming)
        if self.path.rstrip("/") in ("/health", ""):
            self._json(200, {"status": "ok", "model": MODEL_NAME, "dim": DIM, "match": MATCH})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path.rstrip("/") != "/detect":
            self._json(404, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = 0
        if length <= 0 or length > MAX_BYTES:
            # 4xx on purpose: mediavault treats a 4xx as "this frame is bad, skip
            # it" and anything else as "the service is unwell, back off".
            self._json(400, {"error": "expected a JPEG body"}, close=True)
            return
        data = self.rfile.read(length)
        started = time.monotonic()
        waited = 0.0
        try:
            with _slots:
                waited = time.monotonic() - started
                faces, size = detect(data)
        except Exception as exc:  # unreadable frame, unsupported colour space, ...
            self.log_message("detect: rejected a %d-byte frame: %s", length, exc)
            self._json(400, {"error": "could not read that frame: %s" % exc})
            return
        # One line per frame, with the two numbers that explain a slow service:
        # what the detector actually looked at, and how long it queued to do it.
        self.log_message(
            "detect: %d face(s) in %.1fs%s (%dx%d x%d)",
            len(faces),
            time.monotonic() - started - waited,
            "" if waited < 0.05 else " after %.1fs queued" % waited,
            size[0],
            size[1],
            2 ** UPSAMPLE,
        )
        self._json(200, {"model": MODEL_NAME, "dim": DIM, "match": MATCH, "faces": faces})

    def log_request(self, code="-", size="-") -> None:
        pass  # do_POST logs something more useful; do_GET is a health check

    def log_message(self, fmt: str, *args) -> None:
        # One line per request, on stderr, so `docker compose logs` is readable.
        print("facerec: " + (fmt % args), flush=True)


def main() -> None:
    print(
        "facerec: listening on :%d — model %s, upsample %d, up to %d px of detector input "
        "(frames shrunk to %d px first), %d at a time, advertising match %.2f"
        % (PORT, MODEL_NAME, UPSAMPLE, DETECT_PX, INPUT_PX, CONCURRENCY, MATCH),
        flush=True,
    )
    if MODEL == "cnn" and UPSAMPLE > 0:
        print(
            "facerec: FACE_UPSAMPLE=%d with the cnn model means frames are shrunk to %d px "
            "to stay inside FACE_DETECT_PX=%d — with cnn you almost certainly want "
            "FACE_UPSAMPLE=0 and a larger FACE_DETECT_PX instead" % (UPSAMPLE, INPUT_PX, DETECT_PX),
            flush=True,
        )
    # Threaded so a health check answers while a frame is being worked on, and so
    # the next frame's JPEG can be read off the socket during it. How many are
    # actually detected at once is FACE_CONCURRENCY, not this.
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
