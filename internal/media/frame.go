package media

import (
	"context"
	"fmt"
	"os/exec"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// FrameJPEG returns one representative frame as a JPEG (longest edge ~maxPx),
// for sending to a vision model. Video is sampled at its poster offset (see
// PosterSeek), with a fallback to the first frame for short clips.
func FrameJPEG(ctx context.Context, cfg config.Config, path string, typ model.MediaType, offset float64, maxPx int) ([]byte, error) {
	vf := fmt.Sprintf("scale='min(%d,iw)':-1", maxPx)
	run := func(seek ...string) ([]byte, error) {
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
	if seek := PosterSeek(typ, offset); seek != nil {
		if b, err := run(seek...); err == nil {
			return b, nil
		}
	}
	return run()
}
