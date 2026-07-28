package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mediavault/internal/model"
)

func TestPosterSeek(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    model.MediaType
		offset float64
		want   []string
	}{
		{"video, no offset set", model.MediaVideo, 0, []string{"-ss", "2"}},
		{"video, negative treated as unset", model.MediaVideo, -5, []string{"-ss", "2"}},
		{"video, explicit offset", model.MediaVideo, 47.5, []string{"-ss", "47.5"}},
		{"still image never seeks", model.MediaImage, 30, nil},
		{"animated never seeks", model.MediaAnimated, 30, nil},
		{"audio never seeks", model.MediaAudio, 30, nil},
	} {
		got := PosterSeek(tc.typ, tc.offset)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: PosterSeek(%s, %v) = %v, want %v", tc.name, tc.typ, tc.offset, got, tc.want)
		}
	}
}

func TestIsIncomplete(t *testing.T) {
	incomplete := []string{
		"ffprobe: [mov,mp4,m4a,3gp,3g2,mj2 @ 0x563eab1a6780] moov atom not found (exit status 1)",
		"ffprobe: clip.webm: Invalid data found when processing input (exit status 1)",
		"ffmpeg: End of file",
	}
	for _, msg := range incomplete {
		if !IsIncomplete(errors.New(msg)) {
			t.Errorf("should be classed incomplete: %q", msg)
		}
	}
	// Real codec/permission problems must stay loud — they are actionable, and
	// folding them into the truncated-file summary would hide them.
	other := []string{
		"ffprobe: clip.mkv: Permission denied (exit status 1)",
		"ffmpeg: Unknown encoder 'libwebp_anim'",
		"ffprobe: No such file or directory",
	}
	for _, msg := range other {
		if IsIncomplete(errors.New(msg)) {
			t.Errorf("should NOT be classed incomplete: %q", msg)
		}
	}
	if IsIncomplete(nil) {
		t.Error("nil error is not incomplete")
	}
}

// A truncated MP4 is the actual failure the user reported. MP4 stores its index
// (moov) at the end of the file, so a copy that stopped early parses fine until
// the index should appear. This asserts Probe surfaces it and IsIncomplete
// recognises it, end to end.
func TestProbeTruncatedFileIsClassifiedIncomplete(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping integration test")
	}
	cfg := testCfg(t)
	ctx := context.Background()

	full := filepath.Join(cfg.DataDir, "full.mp4")
	mk := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-t", "10",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-y", full)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}
	// Sanity check: the intact file must probe cleanly, or the test proves nothing.
	if _, err := Probe(ctx, cfg, full, model.MediaVideo); err != nil {
		t.Fatalf("intact file should probe: %v", err)
	}

	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(cfg.DataDir, "cut.mp4")
	if err := os.WriteFile(cut, b[:len(b)*60/100], 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Probe(ctx, cfg, cut, model.MediaVideo)
	if err == nil {
		t.Fatal("truncated file probed without error")
	}
	if !IsIncomplete(err) {
		t.Errorf("truncated file not classified incomplete; error was: %v", err)
	}
}
