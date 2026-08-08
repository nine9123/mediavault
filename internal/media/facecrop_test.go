package media

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mediavault/internal/model"
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

// The People page's covers are cut to a temp name and renamed into place, so dst
// carries no image extension. ffmpeg guesses its output format from that name
// unless told otherwise, and guessing on ".tmp" fails outright — which showed up
// as every face card on the page falling back to its placeholder.
func TestFaceCropIgnoresTheDestinationExtension(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping integration test")
	}
	cfg := testCfg(t)
	ctx := context.Background()

	src := filepath.Join(cfg.DataDir, "fixture.png")
	mk := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x480", "-frames:v", "1", "-y", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}

	dst := filepath.Join(cfg.DataDir, "faces", "abc123.jpg.7.tmp")
	if err := FaceCrop(ctx, cfg, src, model.MediaImage, 0, 0.3, 0.3, 0.2, 0.2, dst); err != nil {
		t.Fatalf("FaceCrop: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Errorf("crop is not a JPEG (%d bytes)", len(b))
	}
}

// A clip shorter than its poster offset yields no frame at the seek point, and
// ffmpeg reports that by writing nothing and exiting 0 — so the retry from the
// start has to be triggered by the missing file, not by the exit status.
func TestFaceCropFallsBackOnAClipShorterThanTheSeek(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping integration test")
	}
	cfg := testCfg(t)
	ctx := context.Background()

	src := filepath.Join(cfg.DataDir, "short.mp4")
	mk := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-t", "1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-y", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}

	dst := filepath.Join(cfg.DataDir, "faces", "short.jpg.1.tmp")
	if err := FaceCrop(ctx, cfg, src, model.MediaVideo, 0, 0.3, 0.3, 0.2, 0.2, dst); err != nil {
		t.Fatalf("FaceCrop: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("no crop written: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("crop is empty")
	}
}
