#!/usr/bin/env bash
# gocheck.sh — build/vet/test this repo without dirtying it.
#
# Why this exists: mediavault ships WITHOUT go.sum on purpose (see go.mod), so
# `go build` in the repo fails until something resolves modernc.org/sqlite, and
# `go mod tidy` would write go.mod/go.sum into the working tree — diff noise that
# does not belong in a feature commit. So: copy the tree to a scratch dir, tidy
# there once, and run everything from there.
#
# Usage:
#   .claude-task/tools/gocheck.sh              # build + vet + test ./...
#   .claude-task/tools/gocheck.sh test ./internal/store/...
#
# Needs network the first time (module download); after that the scratch dir and
# Go's module cache make it fast. Delete /tmp/mediavault-check to start clean.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="${GOCHECK_DIR:-/tmp/mediavault-check}"

mkdir -p "$scratch"
# Copy sources, but never the resolved go.mod/go.sum back over the scratch pair.
tar -C "$repo" -cf - --exclude=.git --exclude=go.mod --exclude=go.sum . | tar -C "$scratch" -xf -
if [ ! -f "$scratch/go.sum" ]; then
  cp "$repo/go.mod" "$scratch/go.mod"
  (cd "$scratch" && GOTOOLCHAIN=auto go mod tidy)
fi

cd "$scratch"
if [ "$#" -gt 0 ]; then
  exec go "$@"
fi
go build ./...
go vet ./...
go test ./...
