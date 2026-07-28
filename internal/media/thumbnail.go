package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// ThumbName maps a file's relpath to a stable thumbnail filename. It's a hash
// of the relpath, so naming needs no content read; a moved/renamed file simply
// gets re-thumbnailed under its new name (rare, and cheap enough to accept).
func ThumbName(relpath string) string {
	sum := sha256.Sum256([]byte(relpath))
	return hex.EncodeToString(sum[:8]) + ".webp"
}

// ThumbPath is the absolute thumbnail path within the thumbnail dir.
func ThumbPath(dir, relpath string) string {
	return filepath.Join(dir, ThumbName(relpath))
}

// Thumbnail writes a webp thumbnail for src at dst. Stills and animated formats
// use their first frame; video seeks to its poster offset (see PosterSeek) and
// falls back to the first frame for clips shorter than that. The longest edge is
// capped at cfg.ThumbPx.
func Thumbnail(ctx context.Context, cfg config.Config, src string, typ model.MediaType, offset float64, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// Downscale but never upscale; keep aspect ratio.
	scale := fmt.Sprintf("scale='min(%d,iw)':-1:flags=lanczos", cfg.ThumbPx)
	frame := func(seekArgs ...string) []string {
		a := append([]string{}, seekArgs...)
		return append(a, "-i", src, "-frames:v", "1", "-vf", scale, "-y", dst)
	}

	if seek := PosterSeek(typ, offset); seek != nil {
		// The input seek is fast and skips black intro frames. Clips shorter than
		// the seek point (common for webm loops) yield no frame there, so retry
		// from the very start before giving up.
		if err := runFFmpeg(ctx, cfg, frame(seek...)); err == nil {
			return nil
		}
	}
	return runFFmpeg(ctx, cfg, frame())
}

func runFFmpeg(ctx context.Context, cfg config.Config, args []string) error {
	if out, err := exec.CommandContext(ctx, cfg.FFmpeg, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %s", lastLine(out))
	}
	return nil
}
