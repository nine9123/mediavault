// Package server is a small HTTP layer over the store: it serves the embedded
// single-page UI at / and a JSON API under /api. Reads run concurrently with an
// in-progress scan (WAL + single-writer), so browsing works while indexing.
package server

import (
	"context"
	"embed"
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

// The UI is compiled into the binary, so there's still just one artifact to
// ship to TrueNAS — no static-files directory, no second container.
//
//go:embed web
var webFS embed.FS

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

// Routes builds the router (Go 1.22 method+pattern matching).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex) // exact "/" only
	mux.HandleFunc("GET /api/items", s.handleItems)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/item", s.handleItemDetail)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/thumb/{path...}", s.handleThumb)
	mux.HandleFunc("GET /api/raw/{path...}", s.handleRaw)
	return mux
}

// handleIndex serves the embedded single-page UI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "ui not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// itemDTO is the JSON shape returned to clients, with ready-to-use URLs.
type itemDTO struct {
	Relpath    string  `json:"relpath"`
	Type       string  `json:"type"`
	MIME       string  `json:"mime"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Duration   float64 `json:"duration,omitempty"`
	ThumbReady bool    `json:"thumb_ready"`
	ThumbURL   string  `json:"thumb_url"`
	RawURL     string  `json:"raw_url"`
}

func toDTO(it model.Item) itemDTO {
	enc := encRel(it.Relpath)
	return itemDTO{
		Relpath:    it.Relpath,
		Type:       string(it.Type),
		MIME:       it.MIME,
		Width:      it.Width,
		Height:     it.Height,
		Duration:   it.Duration,
		ThumbReady: it.ThumbReady,
		ThumbURL:   "/api/thumb/" + enc,
		RawURL:     "/api/raw/" + enc,
	}
}

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	limit := clampAtoi(r.URL.Query().Get("limit"), 120, 1, 500)
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
	limit := clampAtoi(r.URL.Query().Get("limit"), 200, 1, 500)
	items, err := s.store.SearchItems(r.Context(), q, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeItems(w, items)
}

// handleItemDetail returns one item plus its tags, for the viewer's catalog card.
func (s *Server) handleItemDetail(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	it, err := s.store.ItemByRelpath(r.Context(), rel)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	tags, err := s.store.TagsForItem(r.Context(), it.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type tagDTO struct {
		Namespace  string  `json:"namespace"`
		Name       string  `json:"name"`
		Source     string  `json:"source"`
		Confidence float64 `json:"confidence,omitempty"`
	}
	tds := make([]tagDTO, 0, len(tags))
	for _, t := range tags {
		tds = append(tds, tagDTO{Namespace: t.Namespace, Name: t.Name, Source: t.Source, Confidence: t.Confidence})
	}
	writeJSON(w, map[string]any{"item": toDTO(it), "tags": tds})
}

// handleStats reports collection size + the latest scan, so the UI can show
// progress while indexing runs.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{
		"items":    st.Items,
		"scanning": s.scanner.Running(),
	}
	if st.Last != nil {
		resp["last"] = map[string]any{
			"started": st.Last.Started, "ended": st.Last.Ended,
			"seen": st.Last.Seen, "added": st.Last.Added,
			"updated": st.Last.Updated, "missing": st.Last.Missing,
			"errors": st.Last.Errors,
		}
	}
	writeJSON(w, resp)
}

// handleScan kicks off a rescan in the background and returns immediately. The
// scanner's guard collapses overlapping requests, so spamming this is safe.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	go s.scanner.Scan(context.Background())
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "scan started"})
}

// handleThumb serves a cached thumbnail. The on-disk name is a hash of the
// relpath, so it isn't attacker-controlled.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, media.ThumbPath(s.cfg.ThumbDir(), r.PathValue("path")))
}

// handleRaw serves the original file. http.ServeFile handles Range requests, so
// video seeking works out of the box. The path is contained within MediaRoot.
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

// encRel percent-escapes each path segment while leaving separators intact.
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
