# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.22-bookworm AS build
WORKDIR /src

# Requires go.sum, so run `go mod tidy` once (with network) before building.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0: modernc.org/sqlite is pure Go, so this links nothing native
# and produces a static, portable binary. -trimpath/-ldflags shrink it.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mediavault ./cmd/server

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
