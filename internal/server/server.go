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
	"mediavault/internal/sidecar"
	"mediavault/internal/store"
	"mediavault/internal/transcode"
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
	tx      *transcode.Transcoder
}

// New creates a Server.
func New(cfg config.Config, st *store.Store, sc *scan.Scanner, tx *transcode.Transcoder) *Server {
	return &Server{cfg: cfg, store: st, scanner: sc, tx: tx}
}

// Routes builds the router (Go 1.22 method+pattern matching).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex) // exact "/" only
	mux.HandleFunc("GET /api/items", s.handleItems)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/random", s.handleRandom)
	mux.HandleFunc("GET /api/recommend", s.handleRecommend)
	mux.HandleFunc("POST /api/view", s.handleView)
	mux.HandleFunc("POST /api/transcode", s.handleTranscode)
	mux.HandleFunc("GET /api/transcode", s.handleTranscodeStatus)
	mux.HandleFunc("GET /api/item", s.handleItemDetail)
	mux.HandleFunc("PUT /api/tags", s.handleSetTags)
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
	ID         int64   `json:"id"`
	Relpath    string  `json:"relpath"`
	Type       string  `json:"type"`
	MIME       string  `json:"mime"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Duration   float64 `json:"duration,omitempty"`
	ThumbReady bool    `json:"thumb_ready"`
	Views      int     `json:"views"`
	ThumbURL   string  `json:"thumb_url"`
	RawURL     string  `json:"raw_url"`
}

// tagDTO is the JSON shape for a tag in the item-detail and tag-edit responses.
type tagDTO struct {
	Namespace  string  `json:"namespace"`
	Name       string  `json:"name"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence,omitempty"`
}

func toTagDTOs(tags []store.ItemTag) []tagDTO {
	out := make([]tagDTO, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagDTO{Namespace: t.Namespace, Name: t.Name, Source: t.Source, Confidence: t.Confidence})
	}
	return out
}

func toDTO(it model.Item) itemDTO {
	enc := encRel(it.Relpath)
	return itemDTO{
		ID:         it.ID,
		Relpath:    it.Relpath,
		Type:       string(it.Type),
		MIME:       it.MIME,
		Width:      it.Width,
		Height:     it.Height,
		Duration:   it.Duration,
		ThumbReady: it.ThumbReady,
		Views:      it.Views,
		ThumbURL:   "/api/thumb/" + enc,
		RawURL:     "/api/raw/" + enc,
	}
}

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := clampAtoi(q.Get("limit"), 120, 1, 500)

	// after=<id>: newer items (for live-streaming during a scan), oldest-first.
	if a := q.Get("after"); a != "" {
		after, _ := strconv.ParseInt(a, 10, 64)
		items, err := s.store.ItemsNewer(r.Context(), after, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeItems(w, items)
		return
	}

	// before=<id> (or absent): newest-first page, older than the cursor.
	var before int64
	if b := q.Get("before"); b != "" {
		before, _ = strconv.ParseInt(b, 10, 64)
	}
	items, err := s.store.ItemsOlder(r.Context(), before, limit)
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
	text, typ := splitTypeFilter(q)
	limit := clampAtoi(r.URL.Query().Get("limit"), 200, 1, 500)
	items, err := s.store.SearchItems(r.Context(), text, typ, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeItems(w, items)
}

// splitTypeFilter pulls "type:<kind>" tokens out of a search query, returning
// the remaining text and a canonical media type ("" if none). Unrecognized
// type words are left in the text.
func splitTypeFilter(q string) (text, typ string) {
	fields := strings.Fields(q)
	kept := fields[:0]
	for _, f := range fields {
		low := strings.ToLower(f)
		if strings.HasPrefix(low, "type:") {
			if t, ok := canonType(low[len("type:"):]); ok {
				typ = t
				continue
			}
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " "), typ
}

func canonType(s string) (string, bool) {
	switch s {
	case "image", "img", "images", "photo", "photos", "pic", "pics", "still", "stills":
		return string(model.MediaImage), true
	case "video", "videos", "vid", "vids", "movie", "movies":
		return string(model.MediaVideo), true
	case "audio", "music", "sound", "song", "songs":
		return string(model.MediaAudio), true
	case "gif", "gifs", "animated", "animation", "anim":
		return string(model.MediaAnimated), true
	}
	return "", false
}

// handleRandom returns a single random item (respecting the ?q= filter, which
// may include a type: token), wrapped like /api/items so the client reads items[0].
func (s *Server) handleRandom(w http.ResponseWriter, r *http.Request) {
	text, typ := splitTypeFilter(strings.TrimSpace(r.URL.Query().Get("q")))
	it, ok, err := s.store.RandomItem(r.Context(), text, typ)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		writeItems(w, nil)
		return
	}
	writeItems(w, []model.Item{it})
}

// handleRecommend returns items sharing the most tags with ?path=, best first.
func (s *Server) handleRecommend(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	limit := clampAtoi(r.URL.Query().Get("limit"), 24, 1, 100)
	items, err := s.store.RecommendByTags(r.Context(), rel, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(items) == 0 {
		// No tag-based matches (untagged seed, or no overlap): fall back to the
		// most-opened items so the strip is still useful before things are tagged.
		if items, err = s.store.MostViewed(r.Context(), rel, limit); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeItems(w, items)
}

// handleView records that an item was opened in the viewer (a popularity signal
// for recommendations). Fire-and-forget from the client.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if err := s.store.IncrementViews(r.Context(), rel); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTranscode starts an on-demand transcode (if needed) and returns status.
func (s *Server) handleTranscode(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"status": s.tx.Request(rel)})
}

// handleTranscodeStatus reports transcode progress without starting work (poll).
func (s *Server) handleTranscodeStatus(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"status": s.tx.Status(rel)})
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
	writeJSON(w, map[string]any{"item": toDTO(it), "tags": toTagDTOs(tags)})
}

// handleSetTags writes an item's manual tags to its sidecar (the durable source
// of truth) and syncs the DB immediately, so the change is visible and
// searchable without waiting for a rescan. AI tags are left alone.
func (s *Server) handleSetTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	// Only accept known items: this 404s bad paths and guards traversal, since
	// a valid relpath is one our own scan produced.
	it, err := s.store.ItemByRelpath(r.Context(), rel)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Preserve any existing rating/notes; replace only the tag list.
	scPath := s.cfg.SidecarPath(rel)
	sc, _, _ := sidecar.Read(scPath)
	sc.TagList = normalizeTags(req.Tags)
	if err := sidecar.Write(scPath, sc); err != nil {
		http.Error(w, "write sidecar: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.SetManualTags(r.Context(), rel, sc.Tags()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	all, err := s.store.TagsForItem(r.Context(), it.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"tags": toTagDTOs(all)})
}

// normalizeTags trims, drops blanks, and de-duplicates (case-insensitively on
// the whole "namespace:name" string) while preserving order.
func normalizeTags(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
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
	rel := r.PathValue("path")
	// Prefer a browser-friendly derived copy when one exists (transcoded video
	// or image preview written by scripts/transcode.sh).
	if p, ok := s.derivedFor(rel); ok {
		setContentType(w, p)
		http.ServeFile(w, r, p)
		return
	}
	abs, ok := s.resolve(rel)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	setContentType(w, abs)
	http.ServeFile(w, r, abs)
}

// setContentType sets an explicit type for known extensions, since the slim
// runtime image ships no /etc/mime.types; unknown types fall back to
// ServeFile's content sniffing. Range/seek handling is unaffected.
func setContentType(w http.ResponseWriter, path string) {
	if ct := media.MimeFor(path); ct != "application/octet-stream" {
		w.Header().Set("Content-Type", ct)
	}
}

// resolve maps a request path to an absolute path and verifies it stays inside
// MediaRoot (defence against ../ traversal).
func (s *Server) resolve(rel string) (string, bool) { return contain(s.cfg.MediaRoot, rel) }

// contain joins rel under root, refusing any path that escapes root.
func contain(root, rel string) (string, bool) {
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(root, clean)
	r := filepath.Clean(root)
	if abs != r && !strings.HasPrefix(abs, r+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// derivedFor returns a browser-friendly derived file for rel if one exists.
func (s *Server) derivedFor(rel string) (string, bool) {
	return transcode.DerivedPath(s.cfg, rel)
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
