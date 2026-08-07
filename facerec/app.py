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
at runtime, and good enough for a personal library. It is deliberately
single-threaded — mediavault sends one frame at a time, and dlib will happily
saturate every core on one frame anyway.

Environment:
    PORT           8000        listen port
    FACE_MODEL     hog         hog (fast, CPU) or cnn (slower, more accurate)
    FACE_UPSAMPLE  1           detection upsampling; 2 finds smaller faces, slower
    FACE_MATCH     (computed)  override the advertised cosine threshold
    FACE_MIN_SIZE  0.02        ignore faces whose width is under this fraction of
                               the frame; their embeddings are mostly noise
"""

import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from io import BytesIO

import face_recognition
import numpy as np
from PIL import Image

PORT = int(os.environ.get("PORT", "8000"))
MODEL = os.environ.get("FACE_MODEL", "hog")
UPSAMPLE = int(os.environ.get("FACE_UPSAMPLE", "1"))
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


def detect(jpeg: bytes):
    """Return mediavault's face list for one JPEG frame."""
    img = Image.open(BytesIO(jpeg))
    # dlib wants 8-bit RGB; a CMYK scan or a greyscale frame would otherwise blow
    # up inside the detector rather than here.
    img = img.convert("RGB")
    arr = np.array(img)
    height, width = arr.shape[0], arr.shape[1]
    if not width or not height:
        return []

    boxes = face_recognition.face_locations(
        arr, number_of_times_to_upsample=UPSAMPLE, model=MODEL
    )
    keep = []
    for top, right, bottom, left in boxes:
        if (right - left) / float(width) >= MIN_SIZE:
            keep.append((top, right, bottom, left))
    if not keep:
        return []

    # num_jitters=1 keeps this fast; raising it re-samples each face and averages,
    # which is a little more accurate and linearly slower.
    encodings = face_recognition.face_encodings(arr, known_face_locations=keep, num_jitters=1)

    out = []
    for (top, right, bottom, left), enc in zip(keep, encodings):
        vec = np.asarray(enc, dtype=np.float64)
        norm = float(np.linalg.norm(vec))
        if norm == 0.0:
            continue
        vec = vec / norm  # mediavault compares by dot product
        out.append(
            {
                "box": [
                    left / float(width),
                    top / float(height),
                    (right - left) / float(width),
                    (bottom - top) / float(height),
                ],
                # dlib's HOG/CNN detector gives no per-face confidence through
                # this API. 1.0 is honest about that: mediavault only uses score
                # to order faces, and falls back to box size on a tie.
                "score": 1.0,
                "embedding": [float(x) for x in vec],
            }
        )
    return out


class Handler(BaseHTTPRequestHandler):
    server_version = "mediavault-facerec/1.0"

    def _json(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
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
            self._json(400, {"error": "expected a JPEG body"})
            return
        data = self.rfile.read(length)
        try:
            faces = detect(data)
        except Exception as exc:  # unreadable frame, unsupported colour space, ...
            self._json(400, {"error": "could not read that frame: %s" % exc})
            return
        self._json(200, {"model": MODEL_NAME, "dim": DIM, "match": MATCH, "faces": faces})

    def log_message(self, fmt: str, *args) -> None:
        # One line per request, on stderr, so `docker compose logs` is readable.
        print("facerec: " + (fmt % args), flush=True)


def main() -> None:
    print(
        "facerec: listening on :%d — model %s, upsample %d, advertising match %.2f"
        % (PORT, MODEL_NAME, UPSAMPLE, MATCH),
        flush=True,
    )
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
