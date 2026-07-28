package media

import (
	"strconv"

	"mediavault/internal/model"
)

// DefaultPosterOffset is how far into a video the representative frame is taken
// when the item has no explicit offset: far enough to clear a fade-in from
// black, close enough to still be on the opening shot.
const DefaultPosterOffset = 2.0

// PosterSeek returns the ffmpeg input-seek arguments for a file's representative
// frame — the one frame that stands in for the whole item. Three consumers need
// to agree on it: the grid thumbnail, the frame sent to the vision model, and
// the perceptual signature used by "looks similar". They previously each
// hardcoded 2 seconds, which meant a per-item offset would have silently applied
// to some and not others.
//
// offset <= 0 means "unset", which is why the field can be omitted from a
// sidecar entirely. Stills get no seek at all.
func PosterSeek(typ model.MediaType, offset float64) []string {
	if typ != model.MediaVideo {
		return nil
	}
	if offset <= 0 {
		offset = DefaultPosterOffset
	}
	return []string{"-ss", strconv.FormatFloat(offset, 'f', -1, 64)}
}
