module mediavault

go 1.22

// This module's one external dependency is modernc.org/sqlite — a pure-Go
// (cgo-free) SQLite with FTS5 built in, so the binary stays static with no
// libsqlite at runtime. It is intentionally NOT pinned here: run
//
//     go mod tidy
//
// once (with network) and Go adds it — at a current, compatible version —
// along with its indirect deps and go.sum. The Docker build runs tidy for you.
