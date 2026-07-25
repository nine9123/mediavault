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
	ThumbPx   int           // thumbnail longest edge, px

	// HashOnIndex computes a content hash the first time a file is indexed.
	// NOTE: because ffmpeg (not us) reads the file for the thumbnail, this is a
	// *second* full read over the network per new file. Turn it off if that
	// cost is too high — you lose only dedup + move-detection, not correctness.
	HashOnIndex bool

	FFmpeg  string // ffmpeg binary (on PATH by default)
	FFprobe string // ffprobe binary

	Exts map[string]model.MediaType // allowed extensions -> media type
}

// DBPath is the SQLite file location inside the data dir.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "index.db") }

// ThumbDir is where generated thumbnails are cached.
func (c Config) ThumbDir() string { return filepath.Join(c.DataDir, "thumbs") }

// MediaTypeFor classifies a filename by extension. ok is false for files we
// don't index (including sidecars, which end in .json).
func (c Config) MediaTypeFor(name string) (model.MediaType, bool) {
	t, ok := c.Exts[strings.ToLower(filepath.Ext(name))]
	return t, ok
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		MediaRoot:   env("MEDIA_ROOT", "/media"),
		DataDir:     env("DATA_DIR", "/data"),
		Addr:        env("ADDR", ":8080"),
		Workers:     envInt("SCAN_WORKERS", 6),
		ScanEvery:   envDur("SCAN_INTERVAL", 5*time.Minute),
		ThumbPx:     envInt("THUMB_PX", 400),
		HashOnIndex: envBool("HASH_ON_INDEX", true),
		FFmpeg:      env("FFMPEG", "ffmpeg"),
		FFprobe:     env("FFPROBE", "ffprobe"),
		Exts:        defaultExts(),
	}
}

func defaultExts() map[string]model.MediaType {
	return map[string]model.MediaType{
		".jpg": model.MediaImage, ".jpeg": model.MediaImage, ".png": model.MediaImage,
		".bmp": model.MediaImage, ".tif": model.MediaImage, ".tiff": model.MediaImage,
		".avif": model.MediaImage, ".jxl": model.MediaImage,
		".gif": model.MediaAnimated, ".webp": model.MediaAnimated, // webp may be still or animated; treat as animated
		".mp4": model.MediaVideo, ".m4v": model.MediaVideo, ".webm": model.MediaVideo,
		".mkv": model.MediaVideo, ".mov": model.MediaVideo, ".avi": model.MediaVideo,
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
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

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
