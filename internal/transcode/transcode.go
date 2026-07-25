// Package transcode produces browser-friendly copies of otherwise-unplayable
// media (WMV/AVI/HEVC video, ALAC/WMA audio, ...) into the configured
// DerivedDir, on demand. It mirrors scripts/transcode.sh's remux-or-encode
// logic: rewrap when the streams are already web-safe (fast, lossless), and
// re-encode only when the codec itself isn't web-playable.
//
// One job runs at a time (transcoding is CPU-heavy); duplicate requests for the
// same file collapse into one. Output is written atomically (.part then rename)
// so a crash or shutdown never leaves a half-file that looks done.
package transcode

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// Codecs a browser can decode (mirrors the shell script).
var (
	webVCodec = map[string]bool{"h264": true, "vp8": true, "vp9": true, "av1": true}
	webACodec = map[string]bool{"aac": true, "mp3": true, "opus": true, "vorbis": true, "flac": true}
	mp4ACodec = map[string]bool{"aac": true, "mp3": true, "opus": true} // valid inside mp4
)

// Status values returned to the client.
const (
	StatusReady       = "ready"       // a derived file exists; serve it
	StatusWorking     = "working"     // queued or in progress
	StatusError       = "error"       // last attempt failed
	StatusNone        = "none"        // nothing known and nothing running
	StatusUnavailable = "unavailable" // DerivedDir not configured
)

type Transcoder struct {
	ctx context.Context
	cfg config.Config

	mu    sync.Mutex
	state map[string]string // relpath -> "working" | "failed"; success is inferred from disk
	sem   chan struct{}     // capacity 1: serialize ffmpeg
}

func New(ctx context.Context, cfg config.Config) *Transcoder {
	return &Transcoder{ctx: ctx, cfg: cfg, state: map[string]string{}, sem: make(chan struct{}, 1)}
}

// DerivedPath returns an existing derived file for relpath, if any. Shared with
// the raw handler so serving and transcoding agree on naming.
func DerivedPath(cfg config.Config, relpath string) (string, bool) {
	if cfg.DerivedDir == "" {
		return "", false
	}
	base, ok := safeJoin(cfg.DerivedDir, relpath)
	if !ok {
		return "", false
	}
	for _, suf := range []string{".mp4", ".webm", ".m4v", ".jpg", ".jpeg", ".webp", ".png", ".mp3", ".m4a", ".ogg", ".oga"} {
		if fi, err := os.Stat(base + suf); err == nil && !fi.IsDir() {
			return base + suf, true
		}
	}
	return "", false
}

// Status reports the current state without starting work.
func (t *Transcoder) Status(relpath string) string {
	if t.cfg.DerivedDir == "" {
		return StatusUnavailable
	}
	if _, ok := DerivedPath(t.cfg, relpath); ok {
		return StatusReady
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state[relpath] {
	case "working":
		return StatusWorking
	case "failed":
		return StatusError
	}
	return StatusNone
}

// Request starts a transcode if one isn't already done/running, and returns the
// resulting status. A previously-failed file is retried.
func (t *Transcoder) Request(relpath string) string {
	if t.cfg.DerivedDir == "" {
		return StatusUnavailable
	}
	if _, ok := DerivedPath(t.cfg, relpath); ok {
		return StatusReady
	}
	t.mu.Lock()
	if t.state[relpath] == "working" {
		t.mu.Unlock()
		return StatusWorking
	}
	t.state[relpath] = "working" // covers "none" and retry-after-failure
	t.mu.Unlock()
	go t.run(relpath)
	return StatusWorking
}

func (t *Transcoder) run(relpath string) {
	select { // one ffmpeg at a time
	case t.sem <- struct{}{}:
	case <-t.ctx.Done():
		t.set(relpath, "failed")
		return
	}
	defer func() { <-t.sem }()

	if t.ctx.Err() != nil {
		t.set(relpath, "failed")
		return
	}
	if err := transcodeOne(t.ctx, t.cfg, relpath); err != nil {
		log.Printf("transcode %s: %v", relpath, err)
		t.set(relpath, "failed")
		return
	}
	// success: drop the entry so Status reports ready from disk
	t.mu.Lock()
	delete(t.state, relpath)
	t.mu.Unlock()
}

func (t *Transcoder) set(relpath, s string) {
	t.mu.Lock()
	t.state[relpath] = s
	t.mu.Unlock()
}

func transcodeOne(ctx context.Context, cfg config.Config, relpath string) error {
	src, ok := safeJoin(cfg.MediaRoot, relpath)
	if !ok {
		return fmt.Errorf("path escapes media root")
	}
	typ, known := cfg.MediaTypeFor(filepath.Base(relpath))
	if !known {
		return fmt.Errorf("unsupported file type")
	}
	switch typ {
	case model.MediaVideo:
		return transcodeVideo(ctx, cfg, src, relpath)
	case model.MediaAudio:
		return transcodeAudio(ctx, cfg, src, relpath)
	default:
		return fmt.Errorf("no on-demand transcode for %s", typ)
	}
}

func transcodeVideo(ctx context.Context, cfg config.Config, src, relpath string) error {
	v := probeCodec(ctx, cfg, src, "v:0")
	a := probeCodec(ctx, cfg, src, "a:0")
	out, err := ensureOut(cfg, relpath, ".mp4")
	if err != nil {
		return err
	}
	tmp := out + ".part.mp4"

	var args []string
	if webVCodec[v] && (a == "" || webACodec[a]) {
		// Streams are web-safe: rewrap into mp4 (no video re-encode).
		if a != "" && !mp4ACodec[a] {
			args = []string{"-nostdin", "-v", "error", "-y", "-i", src,
				"-c:v", "copy", "-c:a", "aac", "-b:a", "160k", "-movflags", "+faststart", tmp}
		} else {
			args = []string{"-nostdin", "-v", "error", "-y", "-i", src,
				"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-movflags", "+faststart", tmp}
		}
	} else {
		args = []string{"-nostdin", "-v", "error", "-y", "-i", src,
			"-map", "0:v:0", "-map", "0:a:0?",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "160k", "-movflags", "+faststart", tmp}
	}
	return runFF(ctx, cfg, args, tmp, out)
}

func transcodeAudio(ctx context.Context, cfg config.Config, src, relpath string) error {
	out, err := ensureOut(cfg, relpath, ".mp3")
	if err != nil {
		return err
	}
	tmp := out + ".part.mp3"
	args := []string{"-nostdin", "-v", "error", "-y", "-i", src,
		"-vn", "-map", "0:a:0", "-c:a", "libmp3lame", "-q:a", "2", tmp}
	return runFF(ctx, cfg, args, tmp, out)
}

func ensureOut(cfg config.Config, relpath, suffix string) (string, error) {
	base, ok := safeJoin(cfg.DerivedDir, relpath)
	if !ok {
		return "", fmt.Errorf("path escapes derived dir")
	}
	out := base + suffix
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	return out, nil
}

func runFF(ctx context.Context, cfg config.Config, args []string, tmp, out string) error {
	if outb, err := exec.CommandContext(ctx, cfg.FFmpeg, args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %s", lastLine(outb))
	}
	return os.Rename(tmp, out)
}

func probeCodec(ctx context.Context, cfg config.Config, src, stream string) string {
	out, err := exec.CommandContext(ctx, cfg.FFprobe, "-v", "error",
		"-select_streams", stream, "-show_entries", "stream=codec_name",
		"-of", "default=nw=1:nk=1", src).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func safeJoin(root, rel string) (string, bool) {
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(root, clean)
	r := filepath.Clean(root)
	if abs != r && !strings.HasPrefix(abs, r+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300] + "\u2026"
	}
	return strings.TrimSpace(s)
}
