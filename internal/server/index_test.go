package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The UI shell is served gzipped to clients that take it, as-is to the rest,
// and is the same page either way.
func TestIndexServedGzippedWhenAccepted(t *testing.T) {
	want, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := newTestServer("").Routes()

	for _, tc := range []struct {
		accept string
		gzip   bool
	}{
		{"", false},
		{"gzip, deflate, br", true},
		{"br;q=1.0, GZIP;q=0.5", true},
		{"gzip;q=0", false},
		{"deflate", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.accept != "" {
			req.Header.Set("Accept-Encoding", tc.accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Accept-Encoding %q: status %d", tc.accept, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("Accept-Encoding %q: Content-Type %q", tc.accept, ct)
		}
		if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary %q, want Accept-Encoding", tc.accept, v)
		}
		body := rec.Body.Bytes()
		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if gotGzip != tc.gzip {
			t.Fatalf("Accept-Encoding %q: gzipped=%v, want %v", tc.accept, gotGzip, tc.gzip)
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
		if !bytes.Equal(body, want) {
			t.Errorf("Accept-Encoding %q: body differs from the embedded page", tc.accept)
		}
	}
}
