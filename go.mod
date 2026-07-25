module mediavault

go 1.22

// The ONLY external dependency. modernc.org/sqlite is a pure-Go (cgo-free)
// SQLite with FTS5 built in, so the server cross-compiles to a single static
// binary with no libsqlite at runtime. ffmpeg/ffprobe are the only runtime deps.
//
// This repo ships without go.sum (it can't be generated offline). Run:
//     go mod tidy
// once, in an environment with network access, to pin indirect deps + sums.
require modernc.org/sqlite v1.34.4
