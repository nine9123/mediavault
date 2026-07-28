package media

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mediavault/internal/config"
)

func testCfg(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		DataDir:          t.TempDir(),
		FFmpeg:           "ffmpeg",
		FFprobe:          "ffprobe",
		PreviewFormat:    "webp",
		PreviewPx:        240,
		PreviewFPS:       8,
		PreviewSegments:  12,
		PreviewSegFrames: 6,
		PreviewSegEvery:  20,
		PreviewQuality:   55,
		PreviewMinDur:    5,
	}
}

func TestPreviewSegments(t *testing.T) {
	cfg := testCfg(t)
	// burst = 6 frames / 8 fps = 0.75s of source per burst.
	for _, tc := range []struct {
		name string
		dur  float64
		want int
	}{
		{"unknown duration", 0, 1},
		{"shorter than one burst", 0.5, 1},
		{"ten seconds", 10, 1},
		{"one minute", 60, 3},
		{"five minutes", 300, 12}, // 300/20 = 15, capped at 12
		{"ten minutes", 600, 12},  // cap holds, so size stays bounded
		{"three hours", 10800, 12},
	} {
		if got := PreviewSegments(cfg, tc.dur); got != tc.want {
			t.Errorf("%s: PreviewSegments(%.1f) = %d, want %d", tc.name, tc.dur, got, tc.want)
		}
	}
}

func TestSampleAtStaysInBounds(t *testing.T) {
	const dur, burst = 600.0, 0.75
	n := 12
	var prev float64 = -1
	for i := 0; i < n; i++ {
		got := sampleAt(dur, i, n, burst)
		if got < 0 {
			t.Fatalf("burst %d starts before zero: %.3f", i, got)
		}
		if got+burst > dur {
			t.Fatalf("burst %d runs past the end: %.3f + %.2f > %.1f", i, got, burst, dur)
		}
		if got <= prev {
			t.Fatalf("burst %d not after burst %d: %.3f <= %.3f", i, i-1, got, prev)
		}
		prev = got
	}
	// The bursts should actually span the video, not cluster at the start:
	// first in the opening twelfth, last in the closing twelfth.
	if first := sampleAt(dur, 0, n, burst); first > dur/float64(n) {
		t.Errorf("first burst too late: %.1f", first)
	}
	if last := sampleAt(dur, n-1, n, burst); last < dur*float64(n-1)/float64(n) {
		t.Errorf("last burst too early: %.1f", last)
	}
}

// A one-burst preview must not seek to t=0 on a normal video: the opening frame
// is often black or a fade, which is exactly what the poster frame already shows.
func TestSampleAtSingleBurstSkipsBlackOpening(t *testing.T) {
	if got := sampleAt(30, 0, 1, 0.75); got <= 0 {
		t.Errorf("single burst on a 30s video should skip the opening, got %.3f", got)
	}
}

func TestPreviewEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping integration test")
	}
	cfg := testCfg(t)
	ctx := context.Background()

	// A 60s fixture: 3 bursts x 6 frames = 18 frames expected.
	src := filepath.Join(cfg.DataDir, "fixture.mp4")
	mk := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24",
		"-t", "60", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "-y", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}

	dst := PreviewPath(cfg.DataDir, "clips/fixture.mp4", cfg.PreviewFormat)
	if err := Preview(ctx, cfg, src, 60, dst); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	frames, loop, err := webpAnimInfo(dst)
	if err != nil {
		t.Fatalf("reading preview: %v", err)
	}
	want := PreviewSegments(cfg, 60) * cfg.PreviewSegFrames
	if frames != want {
		t.Errorf("frame count = %d, want %d", frames, want)
	}
	if loop != 0 {
		t.Errorf("loop count = %d, want 0 (forever)", loop)
	}

	// No scratch directories left behind.
	ents, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			t.Errorf("leftover scratch dir %s", e.Name())
		}
	}
}

func TestPreviewGIF(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping integration test")
	}
	cfg := testCfg(t)
	cfg.PreviewFormat = "gif"
	ctx := context.Background()

	src := filepath.Join(cfg.DataDir, "fixture.mp4")
	mk := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24",
		"-t", "30", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "-y", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}

	dst := PreviewPath(cfg.DataDir, "clips/fixture.mp4", cfg.PreviewFormat)
	if err := Preview(ctx, cfg, src, 30, dst); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b[:6]); got != "GIF89a" {
		t.Errorf("magic = %q, want GIF89a", got)
	}
	// 0x2C is the image-descriptor byte introducing each frame; an animation
	// needs more than one.
	var n int
	for _, c := range b {
		if c == 0x2C {
			n++
		}
	}
	if n < 2 {
		t.Errorf("gif looks static: %d image descriptors", n)
	}
}

// webpAnimInfo counts ANMF (animation frame) chunks and reads the loop count
// from the ANIM chunk. ffprobe can't parse animated webp, so the container is
// walked directly: RIFF header, then a sequence of 8-byte-tagged chunks with
// even-padded payloads.
func webpAnimInfo(path string) (frames, loop int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, os.ErrInvalid
	}
	for p := 12; p+8 <= len(b); {
		tag := string(b[p : p+4])
		size := int(binary.LittleEndian.Uint32(b[p+4 : p+8]))
		if p+8+size > len(b) {
			break
		}
		switch tag {
		case "ANIM":
			loop = int(binary.LittleEndian.Uint16(b[p+8+4 : p+8+6]))
		case "ANMF":
			frames++
		}
		p += 8 + size + size%2
	}
	return frames, loop, nil
}
