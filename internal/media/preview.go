package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"mediavault/internal/config"
)

// Animated previews for video.
//
// A poster frame tells you a video exists; it doesn't tell you what's in it.
// The preview here is a pre-baked looping animation assembled from N short
// bursts of frames sampled at evenly spaced points across the whole duration —
// so a 10-minute video yields ~0.75s from 25s in, then from 75s in, and so on.
// Scrubbing through the result gives you the shape of the video in a couple of
// seconds.
//
// Why a burst per sample point rather than one frame per point: a single frame
// every 50 seconds reads as a slideshow of unrelated stills. A short run of
// consecutive frames reads as motion, which is what makes a preview legible at
// a glance — you can tell a talking head from a chase scene.
//
// Why one ffmpeg invocation per burst rather than one pass with a select
// filter: a select filter has to decode the entire file to evaluate the
// predicate, which over a network mount means pulling every byte of a 4 GB
// video across the wire. An input seek (-ss before -i) jumps to the nearest
// keyframe and reads only from there, so N bursts cost N small reads no matter
// how long the video is. The extra process spawns are noise next to that.

// PreviewName maps a relpath to a stable preview filename, mirroring ThumbName:
// a hash of the relpath, so no content read is needed to name the file.
func PreviewName(relpath, format string) string {
	sum := sha256.Sum256([]byte(relpath))
	return hex.EncodeToString(sum[:8]) + ".preview." + format
}

// PreviewPath is the absolute preview path within the preview dir.
func PreviewPath(dir, relpath, format string) string {
	return filepath.Join(dir, PreviewName(relpath, format))
}

// PreviewSegments reports how many bursts a video of this duration gets: one
// per cfg.PreviewSegEvery seconds, at least one, capped at cfg.PreviewSegments.
// Short clips get few bursts because there's little to summarize; long ones hit
// the cap, so preview size stays bounded whether the video is 5 minutes or 3
// hours (a longer video just samples more sparsely).
func PreviewSegments(cfg config.Config, duration float64) int {
	burst := burstSeconds(cfg)
	if duration <= burst {
		return 1
	}
	every := math.Max(cfg.PreviewSegEvery, burst)
	n := int(duration / every)
	if n < 1 {
		n = 1
	}
	if n > cfg.PreviewSegments {
		n = cfg.PreviewSegments
	}
	return n
}

// sampleFPS is the rate frames are pulled from the source. Playback always
// happens at PreviewFPS, so sampling more slowly than that is what produces a
// sped-up burst: 4 fps in, 8 fps out, 2x motion. Speed 1 keeps real time, which
// is the most legible default — you can read a face or a caption.
func sampleFPS(cfg config.Config) float64 {
	speed := cfg.PreviewSpeed
	if speed <= 0 {
		speed = 1
	}
	fps := float64(cfg.PreviewFPS)
	if fps <= 0 {
		fps = 8
	}
	return fps / speed
}

// burstSeconds is how much source time one burst covers.
func burstSeconds(cfg config.Config) float64 {
	return float64(cfg.PreviewSegFrames) / sampleFPS(cfg)
}

// Preview writes an animated preview for src at dst. duration comes from the
// probe; zero (unknown) degrades to a single burst from the start, which is
// still better than a still.
//
// Individual bursts are allowed to fail — a seek landing in a corrupt region or
// past a truncated tail yields no frames, and dropping that burst is better
// than failing the whole preview. Only a total absence of frames is an error.
func Preview(ctx context.Context, cfg config.Config, src string, duration float64, dst string) error {
	segs := PreviewSegments(cfg, duration)
	burst := burstSeconds(cfg)

	// Scratch space lives under DataDir rather than /tmp: DataDir is the
	// writable volume we already own, and a container's /tmp is often a small
	// tmpfs that a few hundred PNGs can fill.
	tmp, err := os.MkdirTemp(cfg.DataDir, "preview-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	vf := fmt.Sprintf("fps=%s,scale='min(%d,iw)':-1:flags=lanczos",
		strconv.FormatFloat(sampleFPS(cfg), 'f', -1, 64), cfg.PreviewPx)
	for i := 0; i < segs; i++ {
		// -start_number keeps each burst's frames in globally increasing order
		// without a shared counter; gaps from failed bursts are closed below.
		args := []string{
			"-nostdin", "-v", "error",
			"-ss", strconv.FormatFloat(sampleAt(duration, i, segs, burst), 'f', 3, 64),
			"-i", src,
			"-an", "-sn", "-dn",
			"-vf", vf,
			"-frames:v", strconv.Itoa(cfg.PreviewSegFrames),
			"-start_number", strconv.Itoa(i * cfg.PreviewSegFrames),
			"-f", "image2", filepath.Join(tmp, "s_%05d.png"),
		}
		_ = runFFmpeg(ctx, cfg, args) // best-effort per burst
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// Renumber to a gap-free sequence. The image2 demuxer stops at the first
	// missing index, so a dropped burst would silently truncate the preview to
	// whatever preceded it.
	frames, err := filepath.Glob(filepath.Join(tmp, "s_*.png"))
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("preview: no frames sampled from %d burst(s)", segs)
	}
	sort.Strings(frames)
	for i, f := range frames {
		if err := os.Rename(f, filepath.Join(tmp, fmt.Sprintf("f_%05d.png", i))); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// Encode to a sibling temp file and rename into place. The HTTP layer
	// serves these straight off disk, so a half-written file would otherwise be
	// served as a broken image.
	//
	// -f is mandatory here, not belt-and-braces: ffmpeg picks the muxer from the
	// output extension, and that extension is ".part".
	part := dst + ".part"
	args := append([]string{
		"-nostdin", "-v", "error",
		"-framerate", strconv.Itoa(cfg.PreviewFPS),
		"-i", filepath.Join(tmp, "f_%05d.png"),
	}, encodeArgs(cfg)...)
	args = append(args, "-f", muxer(cfg), "-y", part)
	if err := runFFmpeg(ctx, cfg, args); err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return err
	}
	return nil
}

// sampleAt returns the start timestamp of burst i of n: the midpoint of the
// i-th equal slice of the video. Sampling at midpoints rather than boundaries
// skips the fade-in at t=0 and the credits at the end without special-casing
// either. The clamp keeps the final burst inside the file.
func sampleAt(duration float64, i, n int, burst float64) float64 {
	if duration <= 0 || n <= 1 {
		return math.Max(0, math.Min(1, duration/4))
	}
	t := duration * (float64(i) + 0.5) / float64(n)
	if t+burst > duration {
		t = duration - burst
	}
	return math.Max(0, t)
}

// muxer is the ffmpeg output format name for the configured preview format.
func muxer(cfg config.Config) string {
	if cfg.PreviewFormat == "gif" {
		return "gif"
	}
	return "webp"
}

// encodeArgs builds the encoder half of the assembly command.
//
// WebP is the default because it is strictly better here: full colour instead
// of GIF's 256-entry palette, and 2-5x smaller for the same frames. Both
// autoplay in a plain <img> with no JS, which is the property that matters.
// GIF stays available for clients that need it.
func encodeArgs(cfg config.Config) []string {
	switch cfg.PreviewFormat {
	case "gif":
		// One pass, two filter branches: build a palette optimised for the
		// frames that actually differ, then apply it. Bayer dithering costs
		// fewer bytes than error-diffusion because it doesn't add per-frame
		// noise for the encoder to store.
		return []string{
			"-vf", "split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5",
			"-loop", "0",
		}
	default:
		return []string{
			"-c:v", "libwebp_anim",
			"-lossless", "0",
			"-q:v", strconv.Itoa(cfg.PreviewQuality),
			"-compression_level", "5",
			"-loop", "0", // 0 = loop forever
		}
	}
}
