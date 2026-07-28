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
	".heic": "image/heic", ".heif": "image/heif", ".svg": "image/svg+xml",
	".jfif": "image/jpeg", ".jpe": "image/jpeg",
	".jp2": "image/jp2", ".j2k": "image/jp2", ".jpf": "image/jpx",
	".tga": "image/x-tga", ".exr": "image/x-exr", ".hdr": "image/vnd.radiance",
	".dds": "image/vnd.ms-dds", ".ppm": "image/x-portable-pixmap",
	".pgm": "image/x-portable-graymap", ".pbm": "image/x-portable-bitmap",
	".pnm": "image/x-portable-anymap",
	".cr2": "image/x-canon-cr2", ".cr3": "image/x-canon-cr3", ".nef": "image/x-nikon-nef",
	".arw": "image/x-sony-arw", ".dng": "image/x-adobe-dng", ".raf": "image/x-fuji-raf",
	".orf": "image/x-olympus-orf", ".rw2": "image/x-panasonic-rw2", ".pef": "image/x-pentax-pef",
	// video
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	".mkv": "video/x-matroska", ".mov": "video/quicktime", ".avi": "video/x-msvideo",
	".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".m2v": "video/mpeg", ".vob": "video/mpeg",
	".ts": "video/mp2t", ".m2ts": "video/mp2t", ".mts": "video/mp2t",
	".flv": "video/x-flv", ".f4v": "video/x-f4v",
	".wmv": "video/x-ms-wmv", ".asf": "video/x-ms-asf",
	".ogv": "video/ogg", ".3gp": "video/3gpp", ".3g2": "video/3gpp2",
	".divx": "video/divx",
	// audio
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".flac": "audio/flac", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".oga": "audio/ogg", ".opus": "audio/opus", ".wma": "audio/x-ms-wma",
	".aif": "audio/aiff", ".aiff": "audio/aiff",
}

// MimeFor returns a best-effort MIME type from the file extension.
func MimeFor(path string) string {
	if m, ok := mimeByExt[strings.ToLower(filepath.Ext(path))]; ok {
		return m
	}
	return "application/octet-stream"
}

// incompleteMarkers are the ffmpeg/ffprobe messages that mean "this file is not
// a whole file" rather than "this codec is unsupported". The common cause is an
// interrupted download: MP4 keeps its index (the `moov` atom) at the END of the
// file, so a copy that stopped early parses as valid right up to the point where
// the index should be and then fails. Note this also fires for a .webm whose
// bytes are really MP4 — ffprobe detects containers by content, not extension,
// which is why the error names the mov demuxer for a .webm path.
var incompleteMarkers = []string{
	"moov atom not found",
	"Invalid data found when processing input",
	"End of file",
	"Truncating packet",
	"could not find codec parameters",
}

// IsIncomplete reports whether err looks like a truncated or still-being-written
// file. Callers use it to aggregate these into one log line instead of one per
// file, since a stalled download folder can otherwise fill the log every scan.
func IsIncomplete(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range incompleteMarkers {
		if strings.Contains(msg, strings.ToLower(m)) {
			return true
		}
	}
	return false
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
