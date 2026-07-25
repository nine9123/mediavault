// Package server is a deliberately small HTTP layer over the store: list,
// search, thumbnails, and range-served originals. It's the seam the future SPA
// (and your comic-reader chain view) will sit on. Reads run concurrently with
// an in-progress scan.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mediavault/internal/config"
	"mediavault/internal/media"
	"mediavault/internal/model"
	"mediavault/internal/scan"
	"mediavault/internal/store"
)

// Server wires the store + scanner into HTTP handlers.
type Server struct {
	cfg     config.Config
	store   *store.Store
	scanner *scan.Scanner
}

// New creates a Server.
func New(cfg config.Config, st *store.Store, sc *scan.Scanner) *Server {
	return &Server{cfg: cfg, store: st, scanner: sc}
}

// Routes builds the router. Uses Go 1.22 method+pattern matching, including the
// {path...} trailing wildcard for file-serving endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", s.handleItems)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/thumb/{path...}", s.handleThumb)
	mux.HandleFunc("GET /api/raw/{path...}", s.handleRaw)
	return mux
}

// itemDTO is the JSON shape returned to clients, with ready-to-use URLs.
type itemDTO struct {
	Relpath  string  `json:"relpath"`
	Type     string  `json:"type"`
	MIME     string  `json:"mime"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Duration float64 `json:"duration,omitempty"`
	ThumbURL string  `json:"thumb_url"`
	RawURL   string  `json:"raw_url"`
}

func toDTO(it model.Item) itemDTO {
	enc := encRel(it.Relpath)
	return itemDTO{
		Relpath:  it.Relpath,
		Type:     string(it.Type),
		MIME:     it.MIME,
		Width:    it.Width,
		Height:   it.Height,
		Duration: it.Duration,
		ThumbURL: "/api/thumb/" + enc,
		RawURL:   "/api/raw/" + enc,
	}
}

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	limit := clampAtoi(r.URL.Query().Get("limit"), 100, 1, 500)
	offset := clampAtoi(r.URL.Query().Get("offset"), 0, 0, 1<<30)

	items, err := s.store.ListItems(r.Context(), limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeItems(w, items)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeItems(w, nil)
		return
	}
	limit := clampAtoi(r.URL.Query().Get("limit"), 100, 1, 500)
	items, err := s.store.SearchItems(r.Context(), q, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeItems(w, items)
}

// handleScan kicks off a rescan in the background and returns immediately. The
// scanner's own guard collapses overlapping requests, so spamming this is safe.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	go s.scanner.Scan(context.Background())
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "scan started"})
}

// handleThumb serves a cached thumbnail. The thumb filename is a hash of the
// relpath, so the on-disk name is not attacker-controlled.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	http.ServeFile(w, r, media.ThumbPath(s.cfg.ThumbDir(), rel))
}

// handleRaw serves the original file. http.ServeFile handles Range requests, so
// video seeking works out of the box. We contain the path within MediaRoot to
// prevent traversal.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.resolve(r.PathValue("path"))
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, abs)
}

// resolve maps a request path to an absolute path and verifies it stays inside
// MediaRoot (defence against ../ traversal).
func (s *Server) resolve(rel string) (string, bool) {
	// Clean against a rooted path so any leading ".." is neutralised.
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(s.cfg.MediaRoot, clean)
	root := filepath.Clean(s.cfg.MediaRoot)
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// ---- helpers ----

func writeItems(w http.ResponseWriter, items []model.Item) {
	dtos := make([]itemDTO, 0, len(items))
	for _, it := range items {
		dtos = append(dtos, toDTO(it))
	}
	writeJSON(w, map[string]any{"items": dtos, "count": len(dtos)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// encRel percent-escapes each path segment while leaving separators intact, so
// the relpath round-trips through a URL.
func encRel(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func clampAtoi(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
