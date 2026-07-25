package media

import (
	"context"
	"fmt"
	"math"
	"os/exec"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// Perceptual signature: a small, model-free descriptor for "looks alike"
// similarity. It blends two cheap parts extracted from a downscaled frame:
//
//   - structure: a per-channel mean-centered RGB grid, so cosine measures the
//     spatial pattern (layout/composition) rather than average brightness. A
//     solid color becomes a near-zero vector and won't match textured images.
//   - color: a coarse RGB histogram (palette), which is layout-invariant.
//
// Both are L2-normalized, weighted, concatenated, and normalized again. Good
// for near-duplicates, same-scene shots, and similar palette/composition. For
// semantic "same subject" similarity you'd want neural embeddings (CLIP) — a
// separate, heavier path.
const (
	sigGrid   = 12                        // RGB grid edge (structure)
	sigColorW = 0.4                       // color weight (structure gets 1-this)
	SigDim    = sigGrid*sigGrid*3 + 4*4*4 // 432 + 64 = 496
	sigPoster = "2"                       // seconds into a video to sample (skip fade-ins)
)

// Signature returns the perceptual descriptor for a file, or an error if a
// frame can't be decoded. Caller should gate on the item having a visual stream.
func Signature(ctx context.Context, cfg config.Config, path string, typ model.MediaType) ([]float32, error) {
	grid, err := rgbGrid(ctx, cfg, path, typ)
	if err != nil {
		return nil, err
	}
	if len(grid) < sigGrid*sigGrid*3 {
		return nil, fmt.Errorf("short frame data")
	}
	return build(grid), nil
}

// rgbGrid decodes one frame downscaled to sigGrid×sigGrid as raw RGB. Video is
// sampled a couple of seconds in (fade-ins are usually black); short clips fall
// back to the first frame.
func rgbGrid(ctx context.Context, cfg config.Config, path string, typ model.MediaType) ([]byte, error) {
	vf := fmt.Sprintf("scale=%d:%d,format=rgb24", sigGrid, sigGrid)
	run := func(seek ...string) ([]byte, error) {
		args := append([]string{"-nostdin", "-v", "error"}, seek...)
		args = append(args, "-i", path, "-frames:v", "1", "-vf", vf, "-f", "rawvideo", "-")
		return exec.CommandContext(ctx, cfg.FFmpeg, args...).Output()
	}
	if typ == model.MediaVideo {
		if out, err := run("-ss", sigPoster); err == nil && len(out) >= sigGrid*sigGrid*3 {
			return out, nil
		}
		return run()
	}
	return run()
}

func build(grid []byte) []float32 {
	n := sigGrid * sigGrid
	structure := make([]float64, n*3)
	for i := 0; i < n*3; i++ {
		structure[i] = float64(grid[i])
	}
	// per-channel mean-center
	for c := 0; c < 3; c++ {
		var m float64
		for i := 0; i < n; i++ {
			m += structure[i*3+c]
		}
		m /= float64(n)
		for i := 0; i < n; i++ {
			structure[i*3+c] -= m
		}
	}
	l2(structure)

	// coarse 4×4×4 RGB histogram
	color := make([]float64, 64)
	for i := 0; i < n*3; i += 3 {
		r, g, b := bin(grid[i]), bin(grid[i+1]), bin(grid[i+2])
		color[r*16+g*4+b]++
	}
	l2(color)

	out := make([]float32, 0, SigDim)
	for _, x := range structure {
		out = append(out, float32(x*(1-sigColorW)))
	}
	for _, x := range color {
		out = append(out, float32(x*sigColorW))
	}
	l2f(out)
	return out
}

func bin(v byte) int {
	b := int(v) / 64
	if b > 3 {
		b = 3
	}
	return b
}

func l2(v []float64) {
	var s float64
	for _, x := range v {
		s += x * x
	}
	if s = math.Sqrt(s); s > 0 {
		for i := range v {
			v[i] /= s
		}
	}
}

func l2f(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s = math.Sqrt(s); s > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / s)
		}
	}
}

// Cosine similarity of two equal-length unit vectors (dot product).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}
