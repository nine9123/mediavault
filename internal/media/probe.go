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

// Probe reads width/height (and duration for video) with ffprobe. On failure it
// returns an error carrying ffprobe's own stderr, so the caller can log why a
// particular file was unreadable rather than a bare "exit status 1".
func Probe(ctx context.Context, cfg config.Config, path string, typ model.MediaType) (Meta, error) {
	out, err := exec.CommandContext(ctx, cfg.FFprobe,
		"-v", "error", "-hide_banner",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration:format=duration",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		return Meta{}, ffErr("ffprobe", err)
	}

	var probe struct {
		Streams []struct {
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return Meta{}, err
	}

	m := Meta{MIME: MimeFor(path)}
	if len(probe.Streams) > 0 {
		m.Width = probe.Streams[0].Width
		m.Height = probe.Streams[0].Height
	}
	if typ == model.MediaVideo {
		// Prefer the container duration; fall back to the video stream's, since
		// many webms carry no duration in the format header.
		if d, ok := parseDur(probe.Format.Duration); ok {
			m.Duration = d
		} else if len(probe.Streams) > 0 {
			if d, ok := parseDur(probe.Streams[0].Duration); ok {
				m.Duration = d
			}
		}
	}
	return m, nil
}

func parseDur(s string) (float64, bool) {
	if s == "" || s == "N/A" {
		return 0, false
	}
	d, err := strconv.ParseFloat(s, 64)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
