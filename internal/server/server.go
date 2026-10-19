// Package server is a small HTTP layer over the store: it serves the embedded
// single-page UI at / and a JSON API under /api. Reads run concurrently with an
// in-progress scan (WAL + single-writer), so browsing works while indexing.
package server

import (
	"bytes"
	"compress/gzip"
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
	"sync"
	"sync/atomic"
	"time"

	"mediavault/internal/auth"
	"mediavault/internal/config"
	"mediavault/internal/faces"
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
	auth    *auth.Auth
	// rec is the face recognizer, shared with its background worker so both use
	// the same similarity threshold. nil when FACE_URL is unset.
	rec *faces.Recognizer
	// cropSem bounds concurrent face-crop ffmpeg runs. The People page asks for
	// every cover at once, and each miss is a process; a browser's own connection
	// limit happens to bound this today, which is not something to rely on.
	cropSem chan struct{}
	// cropSeq makes each crop's temp filename unique, so two requests for the
	// same face can't write the same partial file.
	cropSeq atomic.Uint64
	// groups caches the last face grouping; see clusterCache.
	groups clusterCache
}

// maxCrops is how many face crops may be cut at once. Small: each one is an
// ffmpeg process reading a frame over the network mount.
const maxCrops = 4

// New creates a Server.
func New(cfg config.Config, st *store.Store, sc *scan.Scanner, tx *transcode.Transcoder, rec *faces.Recognizer) *Server {
	return &Server{cfg: cfg, store: st, scanner: sc, tx: tx, rec: rec,
		auth:    auth.New(cfg.APIKey, cfg.AuthTTL),
		cropSem: make(chan struct{}, maxCrops)}
}

// Auth exposes the authenticator so main can log whether the API is protected.
func (s *Server) Auth() *auth.Auth { return s.auth }

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
	// People (face recognition; all of these 503 unless FACE_URL is set).
	mux.HandleFunc("GET /api/people", s.handlePeople)
	mux.HandleFunc("GET /api/people/items", s.handlePeopleItems)
	mux.HandleFunc("PUT /api/people", s.handleNamePerson)
	mux.HandleFunc("DELETE /api/people", s.handleForgetPerson)
	mux.HandleFunc("POST /api/people/regroup", s.handleWidenGroup)
	mux.HandleFunc("DELETE /api/people/regroup", s.handleResetGroup)
	mux.HandleFunc("POST /api/people/merge", s.handleMergeGroups)
	mux.HandleFunc("DELETE /api/people/merge", s.handleUnmergeGroup)
	mux.HandleFunc("POST /api/faces/reset", s.handleResetFaces)
	mux.HandleFunc("GET /api/face/{id}", s.handleFaceCrop)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/thumb/{path...}", s.handleThumb)
	mux.HandleFunc("GET /api/preview/{path...}", s.handlePreview)
	mux.HandleFunc("GET /api/raw/{path...}", s.handleRaw)

	// Auth endpoints are reachable without a session, for obvious reasons.
	mux.HandleFunc("GET /api/auth", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth", s.handleLogin)
	mux.HandleFunc("DELETE /api/auth", s.handleLogout)

	// Readiness for idlegate's health= check; see handleHealthz.
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Icons for tabs, bookmarks and home-screen shortcuts; see handleIcon.
	for name := range icons {
		mux.HandleFunc("GET /"+name, handleIcon)
	}

	// Wrapping the finished mux — rather than checking inside each handler — is
	// what makes this fail closed: a route added later is protected by default,
	// and forgetting to add a check is not a possible mistake. gzipJSON sits
	// outside it and only ever touches application/json bodies; see gzipJSON.
	return gzipJSON(s.requireAuth(mux))
}

// publicPaths are served without a session. The UI shell has to be, or there
// would be nowhere to type the key; it contains no library data, just markup.
// /healthz is too: idlegate sends no key, and it says nothing about the library.
// So are the icons: browsers fetch them for bookmarks without the session cookie.
func publicPaths(p string) bool {
	_, icon := icons[strings.TrimPrefix(p, "/")]
	return p == "/" || p == "/api/auth" || p == "/healthz" || icon
}

// requireAuth rejects unauthenticated requests to everything except publicPaths.
// It accepts either the session cookie (which is what browsers send on <img> and
// <video> loads) or an X-API-Key / Bearer header (for curl and scripts).
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() || publicPaths(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.authed(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Deliberately no WWW-Authenticate header: it would make the browser pop
		// its own native credential dialog over the app's own login screen.
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// authed reports whether a request carries a valid session or key.
func (s *Server) authed(r *http.Request) bool {
	if k := auth.KeyFromRequest(r); k != "" && s.auth.VerifyKey(k) {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return s.auth.VerifyToken(c.Value, time.Now())
	}
	return false
}

const sessionCookie = "mv_session"

// handleAuthStatus lets the UI decide, on load, whether to show the key prompt or
// go straight to the library. Unauthenticated by design — it reveals only whether
// a key is required, never anything about the library.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"enabled": s.auth.Enabled(),
		"authed":  !s.auth.Enabled() || s.authed(r),
	})
}

// handleHealthz is what idlegate polls after waking this container, before it
// forwards the request that woke it. main only starts listening once config is
// loaded and the database is open and migrated, so a server that can answer at
// all is ready to serve the UI. It deliberately doesn't wait for the catch-up
// scan or look at facerec: the UI works while those are busy or down, and this
// has to stay cheap.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleLogin exchanges the API key for a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.auth.Enabled() {
		writeJSON(w, map[string]any{"ok": true, "enabled": false})
		return
	}
	client := auth.Client(r)
	now := time.Now()
	if !s.auth.AllowAttempt(client, now) {
		http.Error(w, "too many attempts — wait a minute and try again", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.auth.VerifyKey(strings.TrimSpace(req.Key)) {
		s.auth.RecordFailure(client, now)
		log.Printf("auth: failed login from %s", client)
		http.Error(w, "wrong key", http.StatusUnauthorized)
		return
	}
	s.auth.RecordSuccess(client)
	token, expires := s.auth.Issue(now)
	http.SetCookie(w, s.sessionCookieFor(r, token, expires))
	writeJSON(w, map[string]any{"ok": true, "enabled": true, "expires": expires.Unix()})
}

// handleLogout clears the session cookie on this device. Other devices keep
// theirs; to revoke everything, change API_KEY.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c := s.sessionCookieFor(r, "", time.Unix(0, 0))
	c.MaxAge = -1
	http.SetCookie(w, c)
	writeJSON(w, map[string]any{"ok": true})
}

// sessionCookieFor builds the session cookie.
//
// HttpOnly because JavaScript never needs to read it — only the browser's own
// loaders do, and keeping it out of reach of script limits what an injected
// script could steal. Secure is set only when the request actually arrived over
// TLS: this is typically a plain-HTTP box on a LAN, and an unconditional Secure
// flag would make the browser discard the cookie and lock everyone out.
func (s *Server) sessionCookieFor(r *http.Request, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	}
}

// isTLS reports whether the request reached us over HTTPS. X-Forwarded-Proto is
// caller-controlled, but trusting it here can only ADD the Secure flag — a
// spoofed value restricts the attacker's own cookie and nothing else.
func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// uiPage is the embedded UI shell, plain and gzipped.
type uiPage struct{ plain, gz []byte }

// indexPage builds the UI shell once. embed.FS.ReadFile returns a fresh copy on
// every call, and the page is the size of the whole app, so reading it per
// request was an allocation of that size per page load; compressing it once
// also cuts what goes over the wire to a fraction.
var indexPage = sync.OnceValues(func() (uiPage, error) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		return uiPage{}, err
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return uiPage{}, err
	}
	if _, err := zw.Write(b); err != nil {
		return uiPage{}, err
	}
	if err := zw.Close(); err != nil {
		return uiPage{}, err
	}
	return uiPage{plain: b, gz: buf.Bytes()}, nil
})

// handleIndex serves the embedded single-page UI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := indexPage()
	if err != nil {
		http.Error(w, "ui not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept-Encoding")
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(page.gz)
		return
	}
	_, _ = w.Write(page.plain)
}

// icons are the embedded favicon files, by name, with their content types.
// favicon.svg is the source; the .ico and .png are rasterised from it for
// browsers that don't take SVG icons, and for iOS home-screen bookmarks.
var icons = map[string]string{
	"favicon.svg":          "image/svg+xml",
	"favicon.ico":          "image/x-icon",
	"apple-touch-icon.png": "image/png",
}

// handleIcon serves one of the icons. They only change with a deploy, so let
// browsers keep them for a day.
func handleIcon(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	b, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", icons[name])
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// acceptsGzip reports whether the client's Accept-Encoding allows gzip (and
// doesn't explicitly refuse it with q=0).
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
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
		ThumbURL:      thumbURL(it, enc),
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
	resp := map[string]any{"item": toDTO(it), "tags": toTagDTOs(tags)}
	// Who is in it. Omitted entirely when recognition is off, so the card doesn't
	// grow an empty section on a library that never asked for faces.
	if f := s.facesForItemDTO(r, it.ID); len(f) > 0 {
		resp["faces"] = f
	}
	writeJSON(w, resp)
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
	it.Offset = off
	writeJSON(w, map[string]any{"offset": off, "thumb_rebuilt": thumbOK, "thumb_url": thumbURL(it, encRel(it.Relpath))})
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
	// The presence of this block is what tells the UI to show the People page at
	// all, so it doubles as feature detection.
	if s.facesOn() {
		if visual, done, nfaces, named, err := s.store.FaceCounts(r.Context()); err == nil {
			resp["faces"] = map[string]any{
				"visual": visual, "done": done, "faces": nfaces, "named": named,
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

// thumbURL carries a version derived from everything that decides the
// thumbnail's pixels: the file's last (re)index, which is when the scanner
// rewrites it, and the poster offset, which handleSetOffset rewrites it for.
// The on-disk name is a hash of the relpath alone, so without this the URL
// would stay the same across a rebuild and a long browser cache would keep
// showing the old frame. With it, handleThumb can let browsers cache for good.
func thumbURL(it model.Item, enc string) string {
	v := strconv.FormatInt(it.IndexedAt.Unix(), 36)
	if it.Offset != 0 {
		v += "-" + strconv.FormatFloat(it.Offset, 'f', -1, 64)
	}
	return "/api/thumb/" + enc + "?v=" + v
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
//
// A versioned URL (see thumbURL) names one rendition for good, so it is cached
// without revalidation; the API hands out a new URL whenever the thumbnail is
// rebuilt. An unversioned request may be for a file that has since changed, so
// it is revalidated every time, as before. A missing thumbnail (not generated
// yet) is answered before any caching header is set, so a 404 is never cached.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	p := media.ThumbPath(s.cfg.ThumbDir(), r.PathValue("path"))
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, p)
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
