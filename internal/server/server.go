// Package server is a small HTTP layer over the store: it serves the embedded
// single-page UI at / and a JSON API under /api. Reads run concurrently with an
// in-progress scan (WAL + single-writer), so browsing works while indexing.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/media"
	"mediavault/internal/model"
	"mediavault/internal/ollama"
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
	mux.HandleFunc("GET /api/similar", s.handleSimilar)
	mux.HandleFunc("POST /api/view", s.handleView)
	mux.HandleFunc("POST /api/transcode", s.handleTranscode)
	mux.HandleFunc("GET /api/transcode", s.handleTranscodeStatus)
	mux.HandleFunc("GET /api/item", s.handleItemDetail)
	mux.HandleFunc("PUT /api/tags", s.handleSetTags)
	mux.HandleFunc("POST /api/caption", s.handleRecaption)
	mux.HandleFunc("PUT /api/offset", s.handleSetOffset)
	mux.HandleFunc("GET /api/tags/top", s.handleTopTags)
	mux.HandleFunc("POST /api/captions/reset", s.handleResetCaptions)
	mux.HandleFunc("POST /api/embeddings/reset", s.handleResetEmbeddings)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/thumb/{path...}", s.handleThumb)
	mux.HandleFunc("GET /api/preview/{path...}", s.handlePreview)
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

// captionFramePx matches the background describer's frame size, so an on-demand
// caption sees exactly the same input the automatic one would have.
const captionFramePx = 512

// itemDTO is the JSON shape returned to clients, with ready-to-use URLs.
type itemDTO struct {
	ID            int64   `json:"id"`
	Relpath       string  `json:"relpath"`
	Type          string  `json:"type"`
	MIME          string  `json:"mime"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Duration      float64 `json:"duration,omitempty"`
	Offset        float64 `json:"offset,omitempty"`
	OffsetDefault float64 `json:"offset_default"`
	ThumbReady    bool    `json:"thumb_ready"`
	PreviewReady  bool    `json:"preview_ready"`
	Views         int     `json:"views"`
	Description   string  `json:"description,omitempty"`
	ThumbURL      string  `json:"thumb_url"`
	PreviewURL    string  `json:"preview_url,omitempty"`
	RawURL        string  `json:"raw_url"`
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
		ID:            it.ID,
		Relpath:       it.Relpath,
		Type:          string(it.Type),
		MIME:          it.MIME,
		Width:         it.Width,
		Height:        it.Height,
		Duration:      it.Duration,
		Offset:        it.Offset,
		OffsetDefault: media.DefaultPosterOffset,
		ThumbReady:    it.ThumbReady,
		PreviewReady:  it.PreviewReady,
		Views:         it.Views,
		Description:   it.Description,
		ThumbURL:      "/api/thumb/" + enc,
		PreviewURL:    previewURL(it, enc),
		RawURL:        "/api/raw/" + enc,
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
	// Hybrid: keep keyword/tag/type hits first, then fill with semantically-close
	// captions (so "puppy" surfaces a "dog on the beach"). Best-effort — if the
	// embedding server is slow or down, we just return the keyword results.
	if s.semanticEnabled() && len(text) >= 3 {
		if sem := s.semanticSearch(r.Context(), text, typ, limit); len(sem) > 0 {
			items = mergeItems(items, sem, limit)
		}
	}
	writeItems(w, items)
}

func (s *Server) semanticEnabled() bool {
	return s.cfg.OllamaURL != "" && s.cfg.OllamaEmbedModel != ""
}

func (s *Server) semanticSearch(ctx context.Context, text, typ string, limit int) []model.Item {
	ectx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	vec, err := ollama.Embed(ectx, s.cfg, text)
	if err != nil {
		log.Printf("search: embed query: %v", err)
		return nil
	}
	items, err := s.store.SemanticSearch(ctx, vec, typ, limit)
	if err != nil {
		log.Printf("search: semantic: %v", err)
		return nil
	}
	return items
}

// mergeItems concatenates a then b, de-duplicated by relpath, capped at limit.
func mergeItems(a, b []model.Item, limit int) []model.Item {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]model.Item, 0, limit)
	add := func(list []model.Item) {
		for _, it := range list {
			if len(out) >= limit {
				return
			}
			if !seen[it.Relpath] {
				seen[it.Relpath] = true
				out = append(out, it)
			}
		}
	}
	add(a)
	add(b)
	return out
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

// handleSimilar returns items that look perceptually similar to ?path=.
func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	limit := clampAtoi(r.URL.Query().Get("limit"), 24, 1, 100)
	items, err := s.store.SimilarItems(r.Context(), rel, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeItems(w, items)
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
// handleResetCaptions clears all captions + embeddings so they get regenerated.
func (s *Server) handleResetCaptions(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ResetCaptions(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleResetEmbeddings clears only caption embeddings (re-embed existing captions).
func (s *Server) handleResetEmbeddings(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ResetEmbeddings(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleTopTags returns the most-used manual tags for the quick-add menu.
func (s *Server) handleTopTags(w http.ResponseWriter, r *http.Request) {
	limit := clampAtoi(r.URL.Query().Get("limit"), 12, 1, 100)
	tags, err := s.store.TopTags(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]tagDTO, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagDTO{Namespace: t.Namespace, Name: t.Name, Source: "manual"})
	}
	writeJSON(w, map[string]any{"tags": out})
}

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

// handleRecaption regenerates one item's caption on demand, so a bad caption can
// be fixed without resetting the whole library. Synchronous: the caller is a
// human waiting on a button, and inference takes seconds, not minutes.
func (s *Server) handleRecaption(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OllamaURL == "" {
		http.Error(w, "captioning is not configured (set OLLAMA_URL)", http.StatusServiceUnavailable)
		return
	}
	it, ok := s.itemFromBody(w, r)
	if !ok {
		return
	}
	if it.Width == 0 {
		http.Error(w, "no visual frame to describe", http.StatusUnprocessableEntity)
		return
	}

	abs := filepath.Join(s.cfg.MediaRoot, filepath.FromSlash(it.Relpath))
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()

	jpeg, err := media.FrameJPEG(ctx, s.cfg, abs, it.Type, it.Offset, captionFramePx)
	if err != nil {
		http.Error(w, "could not extract a frame: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	desc, err := ollama.Caption(ctx, s.cfg, jpeg)
	if err != nil {
		http.Error(w, "caption failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if desc == "" {
		http.Error(w, "the model returned an empty caption", http.StatusBadGateway)
		return
	}
	// SetDescription also drops the stale embedding, so semantic search stops
	// matching the caption we just replaced.
	if err := s.store.SetDescription(ctx, it.Relpath, desc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-embed inline when configured, so the new caption is searchable at once
	// rather than whenever the background pass next runs. Failure is not fatal:
	// the caption is already saved and the backfill will retry.
	if s.cfg.OllamaEmbedModel != "" {
		if vec, err := ollama.Embed(ctx, s.cfg, desc); err != nil {
			log.Printf("caption: re-embed %s: %v", it.Relpath, err)
		} else if err := s.store.SetEmbedding(ctx, it.Relpath, vec); err != nil {
			log.Printf("caption: save embedding %s: %v", it.Relpath, err)
		}
	}
	writeJSON(w, map[string]any{"description": desc})
}

// handleSetOffset sets how far into a video its representative frame is taken,
// then rebuilds the derivatives that depend on that choice. The sidecar is
// written first: it is the source of truth, and the DB is a cache of it.
func (s *Server) handleSetOffset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string   `json:"path"`
		Offset *float64 `json:"offset"` // pointer so null/absent means "clear"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	it, err := s.store.ItemByRelpath(r.Context(), strings.TrimSpace(req.Path))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var off float64
	if req.Offset != nil {
		off = *req.Offset
	}
	if off < 0 || math.IsNaN(off) || math.IsInf(off, 0) {
		http.Error(w, "offset must be a non-negative number of seconds", http.StatusBadRequest)
		return
	}
	// Clamp into the video: an offset past the end yields no frame, and the
	// thumbnailer would silently fall back to frame 0, which looks like the
	// setting was ignored.
	if it.Duration > 0 && off > it.Duration {
		off = math.Max(0, it.Duration-0.1)
	}

	scPath := s.cfg.SidecarPath(it.Relpath)
	sc, _, _ := sidecar.Read(scPath) // preserve tags/rating/notes
	sc.Offset = off
	if err := sidecar.Write(scPath, sc); err != nil {
		http.Error(w, "write sidecar: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.SetOffset(r.Context(), it.Relpath, off); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Rebuild what the offset decides. The thumbnail is what the user is looking
	// at, so it is worth waiting for; the animated preview is unaffected (it
	// samples the whole duration) and the signature is refreshed on the next scan.
	abs := filepath.Join(s.cfg.MediaRoot, filepath.FromSlash(it.Relpath))
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	thumbOK := true
	if err := media.Thumbnail(ctx, s.cfg, abs, it.Type, off, media.ThumbPath(s.cfg.ThumbDir(), it.Relpath)); err != nil {
		log.Printf("offset: rethumbnail %s: %v", it.Relpath, err)
		thumbOK = false
	}
	writeJSON(w, map[string]any{"offset": off, "thumb_rebuilt": thumbOK})
}

// itemFromBody decodes a {"path": ...} body and resolves it to a known item.
// Only indexed relpaths are accepted, which 404s bad paths and blocks traversal.
func (s *Server) itemFromBody(w http.ResponseWriter, r *http.Request) (model.Item, bool) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return model.Item{}, false
	}
	rel := strings.TrimSpace(req.Path)
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return model.Item{}, false
	}
	it, err := s.store.ItemByRelpath(r.Context(), rel)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return model.Item{}, false
	}
	return it, true
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
	if s.cfg.OllamaURL != "" {
		if visual, captioned, embedded, err := s.store.CaptionCounts(r.Context()); err == nil {
			resp["captions"] = map[string]any{
				"visual": visual, "captioned": captioned,
				"embeddings": s.cfg.OllamaEmbedModel != "", "embedded": embedded,
			}
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

// previewURL is empty unless a preview exists, so the client can treat a
// present URL as "safe to load" without a second readiness check.
func previewURL(it model.Item, enc string) string {
	if !it.PreviewReady {
		return ""
	}
	return "/api/preview/" + enc
}

// handlePreview serves a cached animated preview. Same shape as handleThumb: the
// on-disk name is a hash of the relpath, so the path segment is the relpath.
// Content-Type is set explicitly because the ".preview.webp" double extension
// defeats Go's sniffing on some builds, and a mislabelled webp won't animate.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PreviewsEnabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", s.cfg.PreviewMIME())
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, media.PreviewPath(s.cfg.PreviewDir(), r.PathValue("path"), s.cfg.PreviewFormat))
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
