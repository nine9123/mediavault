package media

import (
	"math"
	"testing"
)

// The crop cache is keyed on (relpath, box) rather than on the face's database id
// because ids are not stable across a cache rebuild: keying on the id would mean
// re-cropping every face in the library after every rebuild to produce
// byte-identical files.
func TestFaceCropNameIsStableAndBoxSpecific(t *testing.T) {
	a := FaceCropName("holiday/img_01.jpg", 0.1, 0.2, 0.3, 0.4)
	if a != FaceCropName("holiday/img_01.jpg", 0.1, 0.2, 0.3, 0.4) {
		t.Error("same face produced two different names")
	}
	if a == FaceCropName("holiday/img_01.jpg", 0.5, 0.2, 0.3, 0.4) {
		t.Error("two faces in one frame collided on the same name")
	}
	if a == FaceCropName("holiday/img_02.jpg", 0.1, 0.2, 0.3, 0.4) {
		t.Error("the same box in two files collided on the same name")
	}
}

func TestPadStaysInsideTheFrame(t *testing.T) {
	for _, tc := range []struct {
		name       string
		x, y, w, h float64
	}{
		{"middle of the frame", 0.4, 0.4, 0.2, 0.2},
		{"hard against the top-left", 0, 0, 0.2, 0.2},
		{"hard against the bottom-right", 0.8, 0.8, 0.2, 0.2},
		{"almost the whole frame", 0.01, 0.01, 0.98, 0.98},
	} {
		x, y, w, h := pad(tc.x, tc.y, tc.w, tc.h, faceMargin)
		// ffmpeg's crop filter cannot take a rectangle that leaves the frame, so
		// this is a hard requirement rather than a nicety.
		if x < 0 || y < 0 || x+w > 1+1e-9 || y+h > 1+1e-9 {
			t.Errorf("%s: padded box (%.3f,%.3f,%.3f,%.3f) leaves the frame", tc.name, x, y, w, h)
		}
		if w < tc.w-1e-9 || h < tc.h-1e-9 {
			t.Errorf("%s: padding shrank the box to %.3fx%.3f from %.3fx%.3f", tc.name, w, h, tc.w, tc.h)
		}
	}
	// A face in open frame should actually gain the margin — a crop tight to the
	// eyes and chin is unrecognisable as a portrait at thumbnail size.
	_, _, w, _ := pad(0.4, 0.4, 0.2, 0.2, faceMargin)
	if math.Abs(w-0.2*(1+faceMargin)) > 1e-9 {
		t.Errorf("padded width %.4f, want %.4f", w, 0.2*(1+faceMargin))
	}
}
