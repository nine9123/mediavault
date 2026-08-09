// Package face is a tiny client for the OPTIONAL face-recognition service. That
// service runs in its own container (see facerec/ for a reference
// implementation): face models want Python, native wheels and often a GPU, none
// of which belong in a static Go binary that otherwise needs only ffmpeg.
//
// The contract is deliberately two endpoints and no state, so any detector can
// satisfy it:
//
//	POST /detect   body: a JPEG frame (Content-Type: image/jpeg)
//	  -> {"model":"buffalo_l","dim":512,"faces":[
//	        {"box":[0.31,0.12,0.18,0.24],"score":0.99,"embedding":[...]}]}
//
//	GET /health
//	  -> {"status":"ok","model":"buffalo_l","dim":512,"match":0.42}
//
// Two details carry their weight:
//
//   - box is [x,y,w,h] as FRACTIONS of the frame, not pixels. mediavault sends a
//     downscaled frame and crops faces out of the original, so pixel coordinates
//     would have to be rescaled by a factor the service doesn't know about.
//   - match is the cosine similarity above which the service's own model
//     considers two embeddings the same person. It is advisory (FACE_MATCH
//     overrides it), but a service knows its model's operating point and
//     mediavault does not — the right value differs by a factor of two between
//     common models.
//
// Embeddings are L2-normalized here, so cosine similarity is a dot product,
// matching how the rest of the app compares vectors.
package face

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"mediavault/internal/config"
)

// Detection is a face found in one frame.
type Detection struct {
	// Box is the face rectangle as fractions of the frame: x, y, w, h in [0,1].
	X, Y, W, H float64
	Score      float64
	Vec        []float32 // L2-normalized
}

// Info is what /health reports: enough to log a useful startup line and to pick
// a similarity threshold suited to the service's model.
type Info struct {
	Model string
	Dim   int
	Match float64 // recommended cosine threshold; 0 if the service doesn't say
}

// client has no timeout of its own: each call below sets a deadline suited to
// what it is doing, because they are not comparable. A health probe that takes
// more than a moment is a service that isn't there; one frame through a CNN
// detector on a busy CPU legitimately takes minutes, and cutting it off at some
// number chosen here would quietly cost the user those faces.
var client = &http.Client{}

// healthTimeout bounds the startup probe. It is only there to keep an
// unreachable service from wedging the worker before it starts.
const healthTimeout = 15 * time.Second

// StatusError is a non-200 reply from the recognition service.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("status %d: %s", e.Code, e.Body) }

// PermanentError reports whether err is the service rejecting this particular
// frame (4xx) rather than a transient problem with the service or the network.
// Callers use it to skip one item instead of backing off the whole queue — the
// same distinction internal/ollama draws, and for the same reason: a bad input
// at the head of a work queue must not starve everything behind it.
func PermanentError(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.Code >= 400 && se.Code < 500 && se.Code != http.StatusTooManyRequests
}

// TimedOut reports whether err is this call running past its deadline, as
// opposed to the service being unreachable or the app shutting down. Callers
// treat it as a property of the frame rather than of the service: a frame that
// takes longer than the budget will take longer than it again next time, and
// retrying it forever is how a work queue stops moving.
func TimedOut(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

type wireFace struct {
	Box       []float64 `json:"box"`
	Score     float64   `json:"score"`
	Embedding []float64 `json:"embedding"`
}

// Detect sends one JPEG frame and returns the faces found in it. An empty slice
// with no error means "looked, found nobody", which is a normal outcome.
func Detect(ctx context.Context, cfg config.Config, jpeg []byte) ([]Detection, error) {
	if cfg.FaceURL == "" {
		return nil, fmt.Errorf("FACE_URL not set")
	}
	if cfg.FaceTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.FaceTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.FaceURL, "/")+"/detect", bytes.NewReader(jpeg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "image/jpeg")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	var out struct {
		Faces []wireFace `json:"faces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	dets := make([]Detection, 0, len(out.Faces))
	for _, f := range out.Faces {
		// Drop rather than reject: one malformed entry in a crowd shot shouldn't
		// cost us the other faces, and the item is marked done either way.
		if len(f.Box) != 4 || len(f.Embedding) == 0 {
			continue
		}
		d := Detection{
			X: f.Box[0], Y: f.Box[1], W: f.Box[2], H: f.Box[3],
			Score: f.Score,
			Vec:   normalize(f.Embedding),
		}
		if d.W <= 0 || d.H <= 0 || d.Vec == nil {
			continue
		}
		dets = append(dets, clampBox(d))
	}
	return dets, nil
}

// Health probes the service. Used for the startup log line and to learn the
// model's recommended similarity threshold.
func Health(ctx context.Context, cfg config.Config) (Info, error) {
	if cfg.FaceURL == "" {
		return Info{}, fmt.Errorf("FACE_URL not set")
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(cfg.FaceURL, "/")+"/health", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Info{}, &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	var out struct {
		Model string  `json:"model"`
		Dim   int     `json:"dim"`
		Match float64 `json:"match"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Info{}, err
	}
	return Info{Model: out.Model, Dim: out.Dim, Match: out.Match}, nil
}

// clampBox keeps a box inside the frame. A detector that pads around a face can
// legitimately report a box hanging off the edge; the crop filter can't.
func clampBox(d Detection) Detection {
	if d.X < 0 {
		d.W += d.X
		d.X = 0
	}
	if d.Y < 0 {
		d.H += d.Y
		d.Y = 0
	}
	if d.X+d.W > 1 {
		d.W = 1 - d.X
	}
	if d.Y+d.H > 1 {
		d.H = 1 - d.Y
	}
	return d
}

// normalize returns a unit-length float32 copy, or nil for a zero vector (which
// carries no direction and so can never be compared meaningfully).
func normalize(in []float64) []float32 {
	var s float64
	for _, x := range in {
		s += x * x
	}
	if s = math.Sqrt(s); s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
		return nil
	}
	out := make([]float32, len(in))
	for i, x := range in {
		out[i] = float32(x / s)
	}
	return out
}
