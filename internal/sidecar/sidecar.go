// Package sidecar reads and writes the small JSON files that hold
// human-authored data (tags, rating, notes) for each media file. Sidecars are
// the source of truth for that metadata, so dropping the SQLite cache loses
// nothing — a rescan reads it all back.
//
//	photo.jpg          <- media (the truth for pixels)
//	photo.jpg.json     <- sidecar (tags, rating, notes — the truth for metadata)
//	chain.json         <- optional per-folder manifest (title + explicit order)
//
// Where sidecars live is the caller's choice (see config.SidecarPath): beside
// the media file, or mirrored under a separate writable dir so the media mount
// can stay read-only. Either way this package only ever writes a sidecar.
package sidecar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediavault/internal/model"
)

const suffix = ".json"

// Sidecar is the per-file metadata document.
type Sidecar struct {
	TagList []string `json:"tags"`
	Rating  int      `json:"rating,omitempty"`
	Notes   string   `json:"notes,omitempty"`

	// Offset is how many seconds into a video to take the representative frame:
	// the grid thumbnail, the frame sent to the vision model, and the perceptual
	// signature. Zero/absent means "use the default" (media.DefaultPosterOffset).
	// It lives here rather than in the DB because it is a human judgement about
	// the file — the DB is a disposable cache, and this must survive a rebuild.
	Offset float64 `json:"offset,omitempty"`
}

// IsSidecar reports whether a filename is a sidecar/manifest (so the scanner
// skips it as media). Everything ending in .json qualifies.
func IsSidecar(name string) bool { return strings.HasSuffix(strings.ToLower(name), suffix) }

// Read loads the sidecar at the given path, if it exists. The caller decides
// where sidecars live (see config.SidecarPath).
func Read(path string) (Sidecar, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Sidecar{}, false, nil
	}
	if err != nil {
		return Sidecar{}, false, err
	}
	var s Sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return Sidecar{}, false, err
	}
	return s, true, nil
}

// Write persists a sidecar atomically (temp file + rename), creating parent
// directories as needed. This is the only place the app writes metadata.
func Write(path string, s Sidecar) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Tags parses the sidecar's tag strings into model.Tags, splitting an optional
// "namespace:name" prefix (e.g. "artist:foo").
func (s Sidecar) Tags() []model.Tag {
	out := make([]model.Tag, 0, len(s.TagList))
	for _, raw := range s.TagList {
		name := strings.TrimSpace(raw)
		ns := ""
		if i := strings.Index(name, ":"); i >= 0 {
			ns, name = strings.TrimSpace(name[:i]), strings.TrimSpace(name[i+1:])
		}
		if name == "" {
			continue
		}
		out = append(out, model.Tag{Namespace: ns, Name: name})
	}
	return out
}

// ChainManifest is an optional chain.json that overrides a folder-chain's
// title and gives an explicit reading order by filename.
type ChainManifest struct {
	Title string   `json:"title"`
	Order []string `json:"order"` // bare filenames, in reading order
}

// ReadChainManifest loads chain.json from a directory, if present.
func ReadChainManifest(dirAbs string) (ChainManifest, bool) {
	b, err := os.ReadFile(filepath.Join(dirAbs, "chain.json"))
	if err != nil {
		return ChainManifest{}, false
	}
	var m ChainManifest
	if json.Unmarshal(b, &m) != nil {
		return ChainManifest{}, false
	}
	return m, true
}

// ResolveOrder maps the manifest's filename order onto full relpaths. Files the
// manifest lists come first (in its order); any discovered file it didn't
// mention is appended afterward (kept in the caller's existing order).
func (m ChainManifest) ResolveOrder(dirRel string, discovered []string) []string {
	pos := make(map[string]int, len(m.Order))
	for i, fn := range m.Order {
		pos[filepath.ToSlash(filepath.Join(dirRel, fn))] = i
	}
	known := make([]string, 0, len(discovered))
	rest := make([]string, 0, len(discovered))
	for _, rp := range discovered {
		if _, ok := pos[rp]; ok {
			known = append(known, rp)
		} else {
			rest = append(rest, rp)
		}
	}
	sort.SliceStable(known, func(a, b int) bool { return pos[known[a]] < pos[known[b]] })
	return append(known, rest...)
}
