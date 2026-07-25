package media

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

var mimeByExt = map[string]string{
	// images
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif",
	".jxl": "image/jxl", ".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff",
	// video
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	".mkv": "video/x-matroska", ".mov": "video/quicktime", ".avi": "video/x-msvideo",
	".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".m2v": "video/mpeg", ".vob": "video/mpeg",
	".ts": "video/mp2t", ".m2ts": "video/mp2t", ".mts": "video/mp2t",
	".flv": "video/x-flv", ".f4v": "video/x-f4v",
	".wmv": "video/x-ms-wmv", ".asf": "video/x-ms-asf",
	".ogv": "video/ogg", ".3gp": "video/3gpp", ".3g2": "video/3gpp2",
	".divx": "video/divx",
}

// MimeFor returns a best-effort MIME type from the file extension.
func MimeFor(path string) string {
	if m, ok := mimeByExt[strings.ToLower(filepath.Ext(path))]; ok {
		return m
	}
	return "application/octet-stream"
}

// ffErr turns an exec failure into a useful message. When the child wrote to
// stderr (ffprobe with "-v error" prints the actual cause there), surface its
// first line instead of the useless bare "exit status 1".
func ffErr(tool string, err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%s: %s (%v)", tool, firstLine(ee.Stderr), err)
	}
	return fmt.Errorf("%s: %v", tool, err)
}

func firstLine(b []byte) string { return clipLine(string(b), true) }
func lastLine(b []byte) string  { return clipLine(string(b), false) }

func clipLine(s string, first bool) string {
	s = strings.TrimSpace(s)
	if first {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
	} else {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "\u2026"
	}
	return s
}
