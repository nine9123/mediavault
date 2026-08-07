package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// faceCropPx is the longest edge of a cached face crop. The People page shows
// them at ~96px, so this leaves room for a retina display without storing a
// second full-size derivative per face.
const faceCropPx = 192

// faceMargin pads the detector's box outward as a fraction of its size. A tight
// box around eyes-to-chin is unrecognisable as a portrait; hair and jawline are
// what make a thumbnail identifiable at 96px.
const faceMargin = 0.35

// FaceCropName maps a face (its item's relpath + its box) to a stable filename.
// Keyed on the box rather than the face's row id because ids are not stable
// across a cache rebuild, and re-cropping every face after every rebuild would
// mean thousands of ffmpeg runs to produce byte-identical files.
func FaceCropName(relpath string, x, y, w, h float64) string {
	key := fmt.Sprintf("%s|%.4f,%.4f,%.4f,%.4f", relpath, x, y, w, h)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]) + ".jpg"
}

// FaceCropPath is the absolute path of a cached face crop.
func FaceCropPath(dir, relpath string, x, y, w, h float64) string {
	return filepath.Join(dir, FaceCropName(relpath, x, y, w, h))
}

// FaceCrop writes a cropped, downscaled JPEG of one face box to dst. The box is
// in frame fractions (see internal/face), so the same numbers work whatever
// resolution the detector was handed. Video seeks to the item's poster offset —
// the frame faces were detected in — so the crop shows the face that was found.
func FaceCrop(ctx context.Context, cfg config.Config, src string, typ model.MediaType, offset float64,
	x, y, w, h float64, dst string) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("empty face box")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	x, y, w, h = pad(x, y, w, h, faceMargin)
	// Expressions in terms of iw/ih keep this independent of the source's actual
	// pixel dimensions, which we don't need to know here. crop must come before
	// scale, or we would be cropping the already-shrunk frame.
	vf := "crop=" + expr(w) + "*iw:" + expr(h) + "*ih:" + expr(x) + "*iw:" + expr(y) + "*ih" +
		fmt.Sprintf(",scale='min(%d,iw)':-1:flags=lanczos", faceCropPx)

	frame := func(seekArgs ...string) []string {
		a := append([]string{"-nostdin", "-v", "error"}, seekArgs...)
		return append(a, "-i", src, "-frames:v", "1", "-vf", vf, "-y", dst)
	}
	if seek := PosterSeek(typ, offset); seek != nil {
		if err := runFFmpeg(ctx, cfg, frame(seek...)); err == nil {
			return nil
		}
	}
	return runFFmpeg(ctx, cfg, frame())
}

// pad grows a box by frac of its own size on every side, clamped to the frame.
func pad(x, y, w, h, frac float64) (float64, float64, float64, float64) {
	x -= w * frac / 2
	y -= h * frac / 2
	w *= 1 + frac
	h *= 1 + frac
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x+w > 1 {
		w = 1 - x
	}
	if y+h > 1 {
		h = 1 - y
	}
	return x, y, w, h
}

func expr(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
