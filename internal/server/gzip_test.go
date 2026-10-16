package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/media"
	"mediavault/internal/model"
)

// JSON is compressed for clients that take gzip, and only JSON: media, Range
// requests and error text go out exactly as the handler wrote them.
func TestGzipJSONOnlyCompressesJSON(t *testing.T) {
	payload := strings.Repeat(`{"relpath":"a/b.jpg"},`, 50)
	h := gzipJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
		case "/media":
			w.Header().Set("Content-Type", "video/mp4")
		case "/error":
			http.Error(w, payload, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, payload)
	}))

	for _, tc := range []struct {
		path, accept, rng string
		gzip              bool
	}{
		{"/json", "gzip", "", true},
		{"/json", "", "", false},
		{"/json", "gzip", "bytes=0-10", false},
		{"/media", "gzip", "", false},
		{"/media", "gzip", "bytes=0-10", false},
		{"/error", "gzip", "", false},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.accept != "" {
			req.Header.Set("Accept-Encoding", tc.accept)
		}
		if tc.rng != "" {
			req.Header.Set("Range", tc.rng)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		body := rec.Body.Bytes()
		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if gotGzip != tc.gzip {
			t.Fatalf("%s accept=%q range=%q: gzipped=%v, want %v", tc.path, tc.accept, tc.rng, gotGzip, tc.gzip)
		}
		if gotGzip {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if body, err = io.ReadAll(zr); err != nil {
				t.Fatal(err)
			}
		}
		if !strings.Contains(string(body), payload) {
			t.Errorf("%s accept=%q range=%q: body altered", tc.path, tc.accept, tc.rng)
		}
		if tc.path == "/json" && tc.rng == "" && rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s accept=%q: Vary %q, want Accept-Encoding", tc.path, tc.accept, rec.Header().Get("Vary"))
		}
	}
}

// A ServeFile Range request through the full router still gets its 206 with
// plain bytes, even from a gzip-accepting client.
func TestThumbRangeNotCompressed(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	p := media.ThumbPath(cfg.ThumbDir(), "a/b.jpg")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(cfg, nil, nil, nil, nil).Routes()

	req := httptest.NewRequest(http.MethodGet, "/api/thumb/a/b.jpg?v=1", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=2-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("status %d, body %q, encoding %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Encoding"))
	}
}

// Versioned thumbnail URLs are cached for good; unversioned ones revalidate; a
// missing thumbnail is never cached.
func TestThumbCacheHeaders(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	p := media.ThumbPath(cfg.ThumbDir(), "a/b.jpg")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(cfg, nil, nil, nil, nil).Routes()

	for _, tc := range []struct {
		url    string
		status int
		cc     string
	}{
		{"/api/thumb/a/b.jpg?v=abc", http.StatusOK, "private, max-age=31536000, immutable"},
		{"/api/thumb/a/b.jpg", http.StatusOK, "no-cache"},
		{"/api/thumb/a/missing.jpg?v=abc", http.StatusNotFound, ""},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.url, rec.Code, tc.status)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != tc.cc {
			t.Errorf("%s: Cache-Control %q, want %q", tc.url, cc, tc.cc)
		}
	}
}

// Anything that rebuilds the thumbnail must change its URL, or the long browser
// cache would keep showing the old frame.
func TestThumbURLChangesOnRebuild(t *testing.T) {
	it := model.Item{Relpath: "a b/c.mp4", IndexedAt: time.Unix(1700000000, 0)}
	base := thumbURL(it, encRel(it.Relpath))
	if !strings.HasPrefix(base, "/api/thumb/a%20b/c.mp4?v=") {
		t.Fatalf("unexpected URL %q", base)
	}

	offset := it
	offset.Offset = 12.5
	reindexed := it
	reindexed.IndexedAt = it.IndexedAt.Add(time.Second)
	seen := map[string]bool{base: true}
	for _, u := range []string{thumbURL(offset, encRel(it.Relpath)), thumbURL(reindexed, encRel(it.Relpath))} {
		if seen[u] {
			t.Errorf("URL %q did not change on rebuild", u)
		}
		seen[u] = true
	}
	if thumbURL(it, encRel(it.Relpath)) != base {
		t.Error("URL is not stable for an unchanged item")
	}
}
