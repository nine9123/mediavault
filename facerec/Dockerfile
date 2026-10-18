# syntax=docker/dockerfile:1

# The face-recognition sidecar. Separate image on purpose: dlib is a C++ build
# and the model weights are ~100 MB, none of which belongs in a static Go binary
# whose only other runtime dependency is ffmpeg.
#
# First build is slow (dlib compiles from source — 5-15 minutes on a NAS CPU).
# The compiler only exists in the build stage, so the runtime image stays lean.
FROM python:3.12-slim-bookworm AS build

# dlib needs cmake + a C++ toolchain; libopenblas speeds up its linear algebra.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential cmake libopenblas-dev \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt

# ---- runtime ----
FROM python:3.12-slim-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends libopenblas0 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /install /usr/local
COPY app.py shutdown.py /app/

# hog is CPU-friendly; set FACE_MODEL=cnn if you have cores to spare and want
# better recall on small or turned faces.
#
# Deliberately not setting FACE_UPSAMPLE, FACE_DETECT_PX or FACE_CONCURRENCY:
# their defaults depend on FACE_MODEL (cnn wants far less of all three), and a
# value baked in here would override that choice for everyone who only sets the
# model. See app.py.
ENV PORT=8000 \
    FACE_MODEL=hog \
    PYTHONUNBUFFERED=1

EXPOSE 8000
# No media mount: mediavault sends the frame in the request body, so this
# container never touches your library.

# Exec form, so Python is PID 1 and receives docker stop's SIGTERM itself;
# shutdown.py turns that into exit 0. A shell-form CMD would leave /bin/sh as
# PID 1, which does not forward it.
CMD ["python", "/app/app.py"]
