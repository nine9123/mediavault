package scan

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"mediavault/internal/config"
	"mediavault/internal/model"
	"mediavault/internal/sidecar"
	"mediavault/internal/store"
)

// setup indexes one small PNG, so the tests below start from a library where
// the media file is already known and unchanged.
func setup(t *testing.T) (config.Config, *store.Store, *Scanner) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Load()
	cfg.MediaRoot = filepath.Join(dir, "media")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.SidecarDir = filepath.Join(dir, "sidecars")
	cfg.ScanSettle = 0
	cfg.HashMode = "off"
	cfg.PreviewFormat = "off"
	cfg.Workers = 2
	for _, d := range []string{cfg.MediaRoot, cfg.DataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	f, err := os.Create(filepath.Join(cfg.MediaRoot, "a.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return cfg, st, New(cfg, st)
}

func scan(t *testing.T, s *Scanner) model.ScanStats {
	t.Helper()
	stats, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func writeSidecar(t *testing.T, cfg config.Config, sc sidecar.Sidecar) {
	t.Helper()
	if err := sidecar.Write(cfg.SidecarPath("a.png"), sc); err != nil {
		t.Fatal(err)
	}
}

func manualTags(t *testing.T, st *store.Store) []string {
	t.Helper()
	it, err := st.ItemByRelpath(context.Background(), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := st.TagsForItem(context.Background(), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tg := range tags {
		if tg.Source == string(model.TagManual) {
			out = append(out, tg.Name)
		}
	}
	sort.Strings(out)
	return out
}

// A sidecar edited by hand, with the media file untouched, reaches the index on
// the next scan without the media file being reprocessed.
func TestSidecarOnlyEditSyncsTags(t *testing.T) {
	cfg, st, s := setup(t)
	if got := scan(t, s); got.Added != 1 {
		t.Fatalf("first scan: %+v, want 1 added", got)
	}
	if got := manualTags(t, st); len(got) != 0 {
		t.Fatalf("tags before any sidecar = %v", got)
	}

	writeSidecar(t, cfg, sidecar.Sidecar{TagList: []string{"beach", "dog"}})
	if got := scan(t, s); got.Added != 0 || got.Updated != 0 || got.Errors != 0 {
		t.Fatalf("scan after sidecar edit: %+v, want no media reprocessing", got)
	}
	if got, want := manualTags(t, st), []string{"beach", "dog"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags after sidecar edit = %v, want %v", got, want)
	}
	if hits, err := st.SearchItems(context.Background(), "beach", "", 10); err != nil || len(hits) != 1 {
		t.Fatalf("search for the new tag: %d hit(s), err %v", len(hits), err)
	}

	// A second edit, possibly within the same second as the last one.
	writeSidecar(t, cfg, sidecar.Sidecar{TagList: []string{"cat"}})
	scan(t, s)
	if got, want := manualTags(t, st), []string{"cat"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags after second edit = %v, want %v", got, want)
	}

	// Deleting the sidecar clears the manual tags it held.
	if err := os.Remove(cfg.SidecarPath("a.png")); err != nil {
		t.Fatal(err)
	}
	scan(t, s)
	if got := manualTags(t, st); len(got) != 0 {
		t.Fatalf("tags after sidecar removed = %v, want none", got)
	}
}

// With nothing edited, a rescan reads no sidecar and writes nothing.
func TestUnchangedSidecarIsSkipped(t *testing.T) {
	cfg, st, s := setup(t)
	writeSidecar(t, cfg, sidecar.Sidecar{TagList: []string{"beach"}})
	scan(t, s)
	before, err := st.ItemByRelpath(context.Background(), "a.png")
	if err != nil {
		t.Fatal(err)
	}

	// Were the sidecar re-read, this unparseable content would surface as an error.
	path := cfg.SidecarPath("a.png")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got := scan(t, s); got.Added != 0 || got.Updated != 0 || got.Errors != 0 {
		t.Fatalf("rescan: %+v, want nothing touched", got)
	}
	after, _ := st.ItemByRelpath(context.Background(), "a.png")
	if !after.IndexedAt.Equal(before.IndexedAt) {
		t.Fatalf("item rewritten: indexed_at %v -> %v", before.IndexedAt, after.IndexedAt)
	}
}

// A broken sidecar keeps the tags the index already has, and is retried once fixed.
func TestBrokenSidecarKeepsTags(t *testing.T) {
	cfg, st, s := setup(t)
	writeSidecar(t, cfg, sidecar.Sidecar{TagList: []string{"beach"}})
	scan(t, s)

	if err := os.WriteFile(cfg.SidecarPath("a.png"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scan(t, s); got.Errors != 1 {
		t.Fatalf("scan with broken sidecar: %+v, want 1 error", got)
	}
	if got, want := manualTags(t, st), []string{"beach"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags after broken edit = %v, want %v", got, want)
	}

	writeSidecar(t, cfg, sidecar.Sidecar{TagList: []string{"dune"}})
	scan(t, s)
	if got, want := manualTags(t, st), []string{"dune"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags after fix = %v, want %v", got, want)
	}
}

// A sidecar edit that moves the poster offset changes which frame the
// thumbnail and signature come from, so it gets a full reprocess.
func TestSidecarOffsetEditReprocesses(t *testing.T) {
	cfg, st, s := setup(t)
	scan(t, s)

	writeSidecar(t, cfg, sidecar.Sidecar{TagList: []string{"beach"}, Offset: 2.5})
	if got := scan(t, s); got.Updated != 1 {
		t.Fatalf("scan after offset edit: %+v, want 1 updated", got)
	}
	it, err := st.ItemByRelpath(context.Background(), "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if it.Offset != 2.5 {
		t.Fatalf("offset = %v, want 2.5", it.Offset)
	}
	if got, want := manualTags(t, st), []string{"beach"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}

	// The reprocess recorded the sidecar, so the next scan leaves it alone.
	if got := scan(t, s); got.Updated != 0 || got.Errors != 0 {
		t.Fatalf("rescan: %+v, want nothing touched", got)
	}
}
