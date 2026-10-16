package server

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// gzipPool recycles gzip writers: each one carries a few hundred KB of
// compressor state, and every JSON response would otherwise allocate one.
var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// gzipJSON compresses JSON responses for clients that accept gzip. Item lists
// are long runs of near-identical keys and URLs, so they shrink several-fold.
//
// The decision is made per response, from its Content-Type, at the moment the
// handler commits its headers — not from the route. Media (thumbnails, previews,
// raw files, face crops) is already compressed and is served through
// http.ServeFile, whose Range handling depends on byte offsets into the
// uncompressed body; compressing those would break seeking. So anything that is
// not application/json passes through untouched, and so does any request
// carrying a Range header or any response that is partial or already encoded.
func gzipJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, accept: acceptsGzip(r)}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipWriter defers choosing between plain and gzipped output until the
// handler's headers are final.
type gzipWriter struct {
	http.ResponseWriter
	accept  bool // client's Accept-Encoding allows gzip
	decided bool
	zw      *gzip.Writer // nil unless this response is being compressed
}

func (g *gzipWriter) WriteHeader(code int) {
	g.decide(code)
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.zw != nil {
		return g.zw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) decide(code int) {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	if !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		return
	}
	// The body depends on Accept-Encoding whether or not this client takes gzip.
	h.Add("Vary", "Accept-Encoding")
	if !g.accept || h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return
	}
	if code < http.StatusOK || code == http.StatusNoContent || code == http.StatusNotModified || code == http.StatusPartialContent {
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length") // the handler's length, if any, is of the plain body
	g.zw = gzipPool.Get().(*gzip.Writer)
	g.zw.Reset(g.ResponseWriter)
}

// close flushes the gzip trailer and returns the writer to the pool.
func (g *gzipWriter) close() {
	if g.zw == nil {
		return
	}
	_ = g.zw.Close()
	gzipPool.Put(g.zw)
	g.zw = nil
}
