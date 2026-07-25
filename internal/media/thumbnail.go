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

// Thumbnail writes a webp thumbnail for src at dst. Stills and animated
// formats use their first frame; video seeks ~1s in for a representative
// poster frame. The longest edge is capped at cfg.ThumbPx.
func Thumbnail(ctx context.Context, cfg config.Config, src string, typ model.MediaType, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// Downscale but never upscale; keep aspect ratio.
	scale := fmt.Sprintf("scale='min(%d,iw)':-1:flags=lanczos", cfg.ThumbPx)

	var args []string
	if typ == model.MediaVideo {
		// -ss before -i = fast input seek. (For clips shorter than the seek
		// point ffmpeg falls back to the first frame; good enough here.)
		args = []string{"-ss", "1", "-i", src, "-frames:v", "1", "-vf", scale, "-y", dst}
	} else {
		args = []string{"-i", src, "-frames:v", "1", "-vf", scale, "-y", dst}
	}

	if out, err := exec.CommandContext(ctx, cfg.FFmpeg, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, out)
	}
	return nil
}
