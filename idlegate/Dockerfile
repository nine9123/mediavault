# syntax=docker/dockerfile:1

# Standard library only, so there's nothing to download: no go.sum, no tidy.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/idlegate .

# Nothing but the static binary: no shell, no libc, no package manager. It holds
# the Docker socket, so the less there is in here the better.
FROM scratch
COPY --from=build /out/idlegate /idlegate
ENTRYPOINT ["/idlegate"]
