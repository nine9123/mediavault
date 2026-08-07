package face

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mediavault/internal/config"
)

func serve(t *testing.T, handler http.HandlerFunc) config.Config {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return config.Config{FaceURL: srv.URL}
}

func TestDetectNormalizesEmbeddings(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/detect" {
			t.Errorf("posted to %s, want /detect", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("Content-Type %q, want image/jpeg", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "jpegbytes" {
			t.Errorf("body %q, want the frame we sent", body)
		}
		_, _ = w.Write([]byte(`{"dim":3,"faces":[
			{"box":[0.1,0.2,0.3,0.4],"score":0.9,"embedding":[3,0,4]}]}`))
	})

	dets, err := Detect(context.Background(), cfg, []byte("jpegbytes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dets) != 1 {
		t.Fatalf("got %d faces, want 1", len(dets))
	}
	d := dets[0]
	if d.X != 0.1 || d.Y != 0.2 || d.W != 0.3 || d.H != 0.4 || d.Score != 0.9 {
		t.Errorf("box/score decoded as %+v", d)
	}
	// Cosine similarity is a dot product everywhere else in the app, which only
	// holds if what we store is unit length. [3,0,4] has length 5.
	var norm float64
	for _, x := range d.Vec {
		norm += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-6 {
		t.Errorf("embedding norm %v, want 1", math.Sqrt(norm))
	}
	if math.Abs(float64(d.Vec[0])-0.6) > 1e-6 || math.Abs(float64(d.Vec[2])-0.8) > 1e-6 {
		t.Errorf("embedding = %v, want [0.6 0 0.8]", d.Vec)
	}
}

// One unusable entry in a crowd shot must not cost us the others: the item is
// marked as looked-at either way, so dropping the whole response would silently
// lose every face in it.
func TestDetectDropsUnusableFacesAndKeepsTheRest(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"faces":[
			{"box":[0,0,0.2,0.2],"embedding":[]},
			{"box":[0,0,0.2],"embedding":[1,0]},
			{"box":[0,0,0,0.2],"embedding":[1,0]},
			{"box":[0,0,0.2,0.2],"embedding":[0,0]},
			{"box":[0.5,0.5,0.2,0.2],"score":0.5,"embedding":[0,1]}]}`))
	})
	dets, err := Detect(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dets) != 1 {
		t.Fatalf("got %d faces, want only the well-formed one: %+v", len(dets), dets)
	}
	if dets[0].X != 0.5 {
		t.Errorf("kept the wrong face: %+v", dets[0])
	}
}

// A detector that pads around a face can report a box hanging off the frame.
// ffmpeg's crop filter cannot, so it is clamped on the way in.
func TestDetectClampsBoxesToTheFrame(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"faces":[
			{"box":[-0.1,-0.2,0.5,0.6],"embedding":[1,0]},
			{"box":[0.8,0.9,0.5,0.4],"embedding":[0,1]}]}`))
	})
	dets, err := Detect(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dets) != 2 {
		t.Fatalf("got %d faces, want 2", len(dets))
	}
	for _, d := range dets {
		if d.X < 0 || d.Y < 0 || d.X+d.W > 1+1e-9 || d.Y+d.H > 1+1e-9 {
			t.Errorf("box escapes the frame: %+v", d)
		}
	}
	if math.Abs(dets[0].W-0.4) > 1e-9 || math.Abs(dets[0].H-0.4) > 1e-9 {
		t.Errorf("clamping should shrink the box, not move it: %+v", dets[0])
	}
}

func TestDetectSurfacesStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		permanent bool
	}{
		{"bad frame", http.StatusBadRequest, true},
		{"rate limited", http.StatusTooManyRequests, false},
		{"service broken", http.StatusInternalServerError, false},
	} {
		cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", tc.code)
		})
		_, err := Detect(context.Background(), cfg, nil)
		if err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("%s: error should carry the service's message, got %v", tc.name, err)
		}
		// The distinction decides whether one item is skipped or the whole queue
		// backs off, so it is worth pinning down.
		if got := PermanentError(err); got != tc.permanent {
			t.Errorf("%s: PermanentError = %v, want %v", tc.name, got, tc.permanent)
		}
	}
}

func TestHealthReportsTheModelsThreshold(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("asked %s, want /health", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"ok","model":"buffalo_l","dim":512,"match":0.42}`))
	})
	info, err := Health(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "buffalo_l" || info.Dim != 512 || info.Match != 0.42 {
		t.Errorf("got %+v", info)
	}
}

func TestUnconfiguredServiceIsAnError(t *testing.T) {
	var empty config.Config
	if _, err := Detect(context.Background(), empty, nil); err == nil {
		t.Error("Detect with no FACE_URL should fail rather than dial nothing")
	}
	if _, err := Health(context.Background(), empty); err == nil {
		t.Error("Health with no FACE_URL should fail")
	}
}
