package media

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// FrameJPEG returns one representative frame as a JPEG (longest edge ~maxPx),
// for sending to a vision model. Video is sampled at its poster offset (see
// PosterSeek), with a fallback to the first frame for short clips.
func FrameJPEG(ctx context.Context, cfg config.Config, path string, typ model.MediaType, offset float64, maxPx int) ([]byte, error) {
	if seek := PosterSeek(typ, offset); seek != nil {
		if b, err := jpegFrame(ctx, cfg, path, maxPx, seek...); err == nil {
			return b, nil
		}
	}
	return jpegFrame(ctx, cfg, path, maxPx)
}

// FrameAtJPEG returns the frame at a given number of seconds into a video. No
// fallback to the first frame: the caller is sampling several points and wants
// to know which one it got, so silently returning frame 0 for a seek past the
// end would hand it the same picture several times over.
func FrameAtJPEG(ctx context.Context, cfg config.Config, path string, at float64, maxPx int) ([]byte, error) {
	if at <= 0 {
		return jpegFrame(ctx, cfg, path, maxPx)
	}
	return jpegFrame(ctx, cfg, path, maxPx, "-ss", strconv.FormatFloat(at, 'f', 3, 64))
}

// jpegFrame is one ffmpeg run: seek (optionally), take a frame, scale it down,
// write a JPEG to stdout.
func jpegFrame(ctx context.Context, cfg config.Config, path string, maxPx int, seek ...string) ([]byte, error) {
	vf := fmt.Sprintf("scale='min(%d,iw)':-1", maxPx)
	args := append([]string{"-nostdin", "-v", "error"}, seek...)
	args = append(args, "-i", path, "-frames:v", "1", "-vf", vf, "-c:v", "mjpeg", "-f", "image2pipe", "-")
	out, err := exec.CommandContext(ctx, cfg.FFmpeg, args...).Output()
	if err != nil {
		return nil, ffErr("ffmpeg", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty frame")
	}
	return out, nil
}
