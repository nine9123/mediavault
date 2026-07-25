# syntax=docker/dockerfile:1

# ---- build stage ----
# modernc.org/sqlite tracks recent Go releases, so its latest version can need a
# newer Go than whatever base tag we pin. GOTOOLCHAIN=auto lets the build pull
# the exact toolchain the resolved dependency asks for, so this won't break the
# next time that dependency bumps its minimum Go. (golang:1.25 already satisfies
# the current requirement; auto only kicks in if a future version needs more.)
FROM golang:1.25-bookworm AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src

# Ships without go.sum: copy the source and let `go mod tidy` resolve the one
# dependency (modernc.org/sqlite) + indirects from the import graph, writing
# go.mod/go.sum inside the build. Cache mounts keep the module + build caches
# warm across rebuilds, so this is only slow the first time.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mediavault ./cmd/server

# ---- runtime stage ----
FROM debian:bookworm-slim
# ffmpeg (which includes ffprobe) is the only runtime dependency; bash runs the
# bundled transcode helper.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg bash ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/mediavault /usr/local/bin/mediavault
# Bundled so you can run `docker compose exec mediavault transcode` — it reads
# MEDIA_ROOT/DERIVED_DIR from the container env, so paths line up with the app.
COPY --chmod=0755 scripts/transcode.sh /usr/local/bin/transcode

ENV MEDIA_ROOT=/media \
    DATA_DIR=/data \
    ADDR=:8080

VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["mediavault"]
