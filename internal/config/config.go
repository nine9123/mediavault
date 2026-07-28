// Package config loads runtime configuration from environment variables with
// sensible defaults, so the container needs no config file.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mediavault/internal/model"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	MediaRoot string // read-only source of truth (your NAS mount)
	DataDir   string // writable cache: SQLite DB + thumbnails
	Addr      string // HTTP listen address

	Workers   int           // scan worker-pool size (keep modest: SMB is the bottleneck)
	ScanEvery time.Duration // periodic rescan interval; 0 disables the ticker

	// ScanSettle is how recently a file may have been modified and still be
	// skipped as "probably still being written". An in-progress download has no
	// index yet, so probing it wastes work and logs a scary error; waiting one
	// scan interval costs nothing. 0 disables the check.
	ScanSettle time.Duration
	ThumbPx    int // thumbnail longest edge, px

	// Animated video previews: several short bursts of frames sampled across
	// the whole duration, baked into one looping file the browser plays in a
	// plain <img>. Generation costs PreviewSegments seeks per video on top of
	// the thumbnail, so on a large library the first scan is noticeably longer;
	// PreviewFormat "off" disables the feature entirely.
	PreviewFormat    string  // "webp" (default) | "gif" | "off"
	PreviewPx        int     // preview longest edge, px (smaller than ThumbPx is fine)
	PreviewFPS       int     // sampling + playback rate; equal values mean 1x speed
	PreviewSegments  int     // maximum number of bursts
	PreviewSegFrames int     // frames per burst
	PreviewSegEvery  float64 // seconds of video per burst, before the cap applies
	PreviewSpeed     float64 // playback speed of each burst; 1 = real time, 2 = 2x
	PreviewQuality   int     // libwebp -q:v, 0-100 (ignored for gif)
	PreviewMinDur    float64 // skip videos shorter than this; they're their own preview

	// HashMode controls content hashing of new files, used for dedup and
	// move-detection (so tags can follow a renamed file). Nothing reads it yet;
	// it just populates the field for those future features.
	//   "off"     no hashing — fastest.
	//   "partial" hash head+tail+size only — near-constant tiny cost regardless
	//             of file size, enough to fingerprint a moved/renamed file.
	//   "full"    hash every byte — strongest, but reads the whole file over the
	//             network (slow for large videos). Was the old default.
	HashMode string

	// SidecarDir is where per-file tag/rating sidecars are read and written.
	// Empty (default) = next to the media file (needs a writable media mount).
	// Set it to a writable path to keep the media mount read-only; sidecars are
	// then mirrored there by relative path.
	SidecarDir string

	// DerivedDir is an optional writable directory of browser-friendly copies
	// (transcoded video / image previews) mirroring the media tree by relpath,
	// e.g. "<rel>.mp4" or "<rel>.jpg". When set, the raw endpoint serves the
	// derived file if one exists, so otherwise-unplayable formats work in the
	// browser. Empty disables it. Populate it with scripts/transcode.sh.
	DerivedDir string

	// Optional natural-language search: point OllamaURL at an Ollama server with
	// a vision model to auto-caption media; captions feed full-text search.
	// Empty OllamaURL disables it.
	OllamaURL    string
	OllamaModel  string
	OllamaPrompt string
	// Optional embedding model (e.g. nomic-embed-text) enabling semantic search
	// over captions. Empty leaves search as keyword-only.
	OllamaEmbedModel string

	FFmpeg  string // ffmpeg binary (on PATH by default)
	FFprobe string // ffprobe binary

	Exts map[string]model.MediaType // allowed extensions -> media type
}

// DBPath is the SQLite file location inside the data dir.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "index.db") }

// ThumbDir is where generated thumbnails are cached.
func (c Config) ThumbDir() string { return filepath.Join(c.DataDir, "thumbs") }

// PreviewDir is where generated animated previews are cached. Kept separate
// from ThumbDir so `rm -rf` on either one rebuilds just that derivative.
func (c Config) PreviewDir() string { return filepath.Join(c.DataDir, "previews") }

// PreviewsEnabled reports whether animated previews should be generated.
func (c Config) PreviewsEnabled() bool { return c.PreviewFormat != "off" }

// PreviewMIME is the Content-Type for generated previews.
func (c Config) PreviewMIME() string {
	if c.PreviewFormat == "gif" {
		return "image/gif"
	}
	return "image/webp"
}

// SidecarPath returns the sidecar file path for a media relpath, honoring
// SidecarDir (empty = beside the media file).
func (c Config) SidecarPath(relpath string) string {
	rp := filepath.FromSlash(relpath) + ".json"
	if c.SidecarDir != "" {
		return filepath.Join(c.SidecarDir, rp)
	}
	return filepath.Join(c.MediaRoot, rp)
}

// MediaTypeFor classifies a filename by extension. ok is false for files we
// don't index (including sidecars, which end in .json).
func (c Config) MediaTypeFor(name string) (model.MediaType, bool) {
	t, ok := c.Exts[strings.ToLower(filepath.Ext(name))]
	return t, ok
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		MediaRoot:        env("MEDIA_ROOT", "/media"),
		DataDir:          env("DATA_DIR", "/data"),
		Addr:             env("ADDR", ":8080"),
		Workers:          envInt("SCAN_WORKERS", 6),
		ScanEvery:        envDur("SCAN_INTERVAL", 5*time.Minute),
		ScanSettle:       envDur("SCAN_SETTLE", 60*time.Second),
		ThumbPx:          envInt("THUMB_PX", 400),
		PreviewFormat:    previewFormat(env("PREVIEW_FORMAT", "webp")),
		PreviewPx:        envInt("PREVIEW_PX", 320),
		PreviewFPS:       envInt("PREVIEW_FPS", 8),
		PreviewSegments:  envInt("PREVIEW_SEGMENTS", 12),
		PreviewSegFrames: envInt("PREVIEW_SEG_FRAMES", 6),
		PreviewSegEvery:  envFloat("PREVIEW_SEG_EVERY", 20),
		PreviewSpeed:     envFloat("PREVIEW_SPEED", 1),
		PreviewQuality:   envInt("PREVIEW_QUALITY", 55),
		PreviewMinDur:    envFloat("PREVIEW_MIN_DUR", 5),
		HashMode:         hashMode(env("HASH_MODE", "partial")),
		SidecarDir:       env("SIDECAR_DIR", ""),
		DerivedDir:       env("DERIVED_DIR", ""),
		OllamaURL:        env("OLLAMA_URL", ""),
		OllamaModel:      env("OLLAMA_MODEL", "llava"),
		OllamaPrompt:     env("OLLAMA_PROMPT", "Describe this image in one or two sentences for search. Name the main subjects, the setting, notable objects, colors, and mood. Be concise, factual, and specific; do not start with \"The image\"."),
		OllamaEmbedModel: env("OLLAMA_EMBED_MODEL", ""),
		FFmpeg:           env("FFMPEG", "ffmpeg"),
		FFprobe:          env("FFPROBE", "ffprobe"),
		Exts:             defaultExts(),
	}
}

func defaultExts() map[string]model.MediaType {
	return map[string]model.MediaType{
		".jpg": model.MediaImage, ".jpeg": model.MediaImage, ".png": model.MediaImage,
		".bmp": model.MediaImage, ".tif": model.MediaImage, ".tiff": model.MediaImage,
		".avif": model.MediaImage, ".jxl": model.MediaImage,
		// less-common stills. ffmpeg thumbnails most of these; a browser can't
		// display them all, so the viewer falls back to a derived preview.
		".heic": model.MediaImage, ".heif": model.MediaImage,
		".jfif": model.MediaImage, ".jpe": model.MediaImage,
		".jp2": model.MediaImage, ".j2k": model.MediaImage, ".jpf": model.MediaImage,
		".tga": model.MediaImage, ".exr": model.MediaImage, ".hdr": model.MediaImage,
		".dds": model.MediaImage, ".ppm": model.MediaImage, ".pgm": model.MediaImage,
		".pbm": model.MediaImage, ".pnm": model.MediaImage, ".svg": model.MediaImage,
		// camera RAW: indexed + searchable, but ffmpeg can't decode most RAW, so
		// they often show no thumbnail until you generate previews externally.
		".cr2": model.MediaImage, ".cr3": model.MediaImage, ".nef": model.MediaImage,
		".arw": model.MediaImage, ".dng": model.MediaImage, ".raf": model.MediaImage,
		".orf": model.MediaImage, ".rw2": model.MediaImage, ".pef": model.MediaImage,
		".gif": model.MediaAnimated, ".webp": model.MediaAnimated, // webp may be still or animated; treat as animated
		".mp4": model.MediaVideo, ".m4v": model.MediaVideo, ".webm": model.MediaVideo,
		".mkv": model.MediaVideo, ".mov": model.MediaVideo, ".avi": model.MediaVideo,
		".mpg": model.MediaVideo, ".mpeg": model.MediaVideo, ".m2v": model.MediaVideo,
		".ts": model.MediaVideo, ".m2ts": model.MediaVideo, ".mts": model.MediaVideo,
		".flv": model.MediaVideo, ".f4v": model.MediaVideo, ".vob": model.MediaVideo,
		".wmv": model.MediaVideo, ".asf": model.MediaVideo, ".ogv": model.MediaVideo,
		".3gp": model.MediaVideo, ".3g2": model.MediaVideo, ".divx": model.MediaVideo,
		// audio — the embedded cover art (if any) becomes the thumbnail
		".mp3": model.MediaAudio, ".m4a": model.MediaAudio, ".aac": model.MediaAudio,
		".flac": model.MediaAudio, ".wav": model.MediaAudio, ".ogg": model.MediaAudio,
		".oga": model.MediaAudio, ".opus": model.MediaAudio, ".wma": model.MediaAudio,
		".aif": model.MediaAudio, ".aiff": model.MediaAudio,
	}
}

// previewFormat validates PREVIEW_FORMAT, falling back to webp on anything
// unrecognized rather than silently producing files nothing can read.
func previewFormat(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "gif":
		return "gif"
	case "off", "none", "false", "0":
		return "off"
	default:
		return "webp"
	}
}

func hashMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "partial", "full":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "partial"
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return unquote(v)
	}
	return def
}

// unquote strips one pair of matching surrounding quotes. Docker Compose's
// list form (`- KEY='value'`) keeps the quotes as part of the value, a common
// footgun for paths like SIDECAR_DIR/DERIVED_DIR; tolerate it.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
