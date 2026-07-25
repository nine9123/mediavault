// Package media shells out to ffmpeg/ffprobe for probing and thumbnailing.
// Going through ffmpeg (rather than a cgo image library) keeps the binary
// static and handles the whole format zoo — stills, gif, animated webp, avif,
// and every video container — with one runtime dependency.
package media

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"

	"mediavault/internal/config"
	"mediavault/internal/model"
)

// Meta is the cheap metadata we extract from a file.
type Meta struct {
	Width    int
	Height   int
	Duration float64 // seconds; video only
	MIME     string
}

// Probe reads width/height (and duration for video) with ffprobe.
func Probe(ctx context.Context, cfg config.Config, path string, typ model.MediaType) (Meta, error) {
	out, err := exec.CommandContext(ctx, cfg.FFprobe,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		return Meta{}, err
	}

	var probe struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return Meta{}, err
	}

	m := Meta{MIME: mimeFor(path)}
	if len(probe.Streams) > 0 {
		m.Width = probe.Streams[0].Width
		m.Height = probe.Streams[0].Height
	}
	if typ == model.MediaVideo {
		if d, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
			m.Duration = d
		}
	}
	return m, nil
}
