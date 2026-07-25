# syntax=docker/dockerfile:1

# ---- build stage ----
# A newer toolchain than the module's `go 1.22` directive gives headroom in
# case `go mod tidy` resolves a dependency that wants a higher Go; newer Go
# builds older-declared modules fine.
FROM golang:1.24-bookworm AS build
WORKDIR /src

# This repo ships without go.sum. Copy the source and let `go mod tidy` resolve
# the one dependency (modernc.org/sqlite) + its indirects from the import graph,
# writing go.mod/go.sum inside the build. The cache mounts keep the module cache
# and build cache warm across rebuilds, so this is only slow the first time.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mediavault ./cmd/server

# ---- runtime stage ----
FROM debian:bookworm-slim
# ffmpeg (which includes ffprobe) is the only runtime dependency.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/mediavault /usr/local/bin/mediavault

ENV MEDIA_ROOT=/media \
    DATA_DIR=/data \
    ADDR=:8080

VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["mediavault"]
