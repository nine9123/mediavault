package faces

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/model"
	"mediavault/internal/sidecar"
	"mediavault/internal/store"
)

// These tests never reach the recognition service: detection is stubbed by
// writing faces straight into the store, which is exactly what the worker does
// with what the service returns. What is under test is everything after that —
// grouping, naming, and whether a name survives losing the cache.

func setup(t *testing.T) (config.Config, *store.Store, *Recognizer) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		MediaRoot: dir,
		DataDir:   dir,
		// people.json lands here; in production this is the same volume as the tag
		// sidecars, for the same backup reasons.
		SidecarDir: dir,
		FaceURL:    "http://stub.invalid",
		FaceMatch:  0.8, // fixed, since no service is around to advertise one
	}
	st, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return cfg, st, New(cfg, st)
}

func addItem(t *testing.T, st *store.Store, rel string) {
	t.Helper()
	now := time.Now()
	err := st.WriteBatch(context.Background(), []store.ItemWrite{{
		Item: model.Item{
			Relpath: rel, Size: 1024, MTime: now, Type: model.MediaImage,
			Width: 800, Height: 600, IndexedAt: now, SeenAt: now,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func vec(a, b, c, d float64) []float32 {
	n := math.Sqrt(a*a + b*b + c*c + d*d)
	return []float32{float32(a / n), float32(b / n), float32(c / n), float32(d / n)}
}

// detected stands in for one pass of the worker: N items, one face each.
//
// In path order rather than map order: SetFaces mints face ids as it goes, and a
// group is identified by the lowest id in it — so iterating the map directly made
// "the group containing the first face" a different group from run to run, and
// the tests below fail about one time in six.
func detected(t *testing.T, st *store.Store, faces map[string][]float32) {
	t.Helper()
	rels := make([]string, 0, len(faces))
	for rel := range faces {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := st.SetFaces(context.Background(), rel, []store.FaceWrite{
			{X: 0.25, Y: 0.2, W: 0.2, H: 0.3, Score: 1, Vec: faces[rel]},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func namesOf(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	all, err := st.AllFaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range all {
		out[f.Relpath] = f.PersonName
	}
	return out
}

func TestNamingAGroupLabelsItAndRecordsItOnDisk(t *testing.T) {
	ctx := context.Background()
	cfg, st, rec := setup(t)

	for _, rel := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		addItem(t, st, rel)
	}
	detected(t, st, map[string][]float32{
		"a.jpg": vec(1, 0, 0, 0),
		"b.jpg": vec(0.98, 0.02, 0, 0), // same person
		"c.jpg": vec(0, 1, 0, 0),       // someone else
	})

	all, err := st.AllFaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	group := pick(t, all, store.ClusterContaining(all, rec.Match(), store.Grouping{}, all[0].ID))
	if len(group) != 2 {
		t.Fatalf("the group has %d faces, want the two alike ones", len(group))
	}
	if _, err := rec.Name(ctx, "Jane", group); err != nil {
		t.Fatal(err)
	}

	got := namesOf(t, st)
	if got["a.jpg"] != "Jane" || got["b.jpg"] != "Jane" {
		t.Errorf("labels = %v, want Jane on a.jpg and b.jpg", got)
	}
	if got["c.jpg"] != "" {
		t.Errorf("c.jpg was labelled %q; a different face must stay unnamed", got["c.jpg"])
	}

	// The durable record, not the cache, is where the name actually lives.
	doc, err := sidecar.ReadPeople(cfg.PeoplePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.People) != 1 || doc.People[0].Name != "Jane" {
		t.Fatalf("people.json = %+v, want one Jane", doc.People)
	}
	if len(doc.People[0].Faces) != 2 {
		t.Errorf("Jane has %d example faces on disk, want 2", len(doc.People[0].Faces))
	}

	items, err := st.ItemsForPerson(ctx, mustPerson(t, st, "Jane"), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("Jane appears in %d items, want 2", len(items))
	}
}

// The point of keeping names in people.json: the database is a disposable cache,
// and dropping it must not lose who anyone is. ResetFaces clears both the
// detections and the people rows, which is what a rebuild does.
func TestNamesSurviveLosingTheCache(t *testing.T) {
	ctx := context.Background()
	_, st, rec := setup(t)

	for _, rel := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		addItem(t, st, rel)
	}
	first := map[string][]float32{
		"a.jpg": vec(1, 0, 0, 0),
		"b.jpg": vec(0.98, 0.02, 0, 0),
		"c.jpg": vec(0, 1, 0, 0),
	}
	detected(t, st, first)
	all, _ := st.AllFaces(ctx)
	if _, err := rec.Name(ctx, "Jane", pick(t, all, store.ClusterContaining(all, rec.Match(), store.Grouping{}, all[0].ID))); err != nil {
		t.Fatal(err)
	}

	if err := st.ResetFaces(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(namesOf(t, st)); n != 0 {
		t.Fatalf("the reset left %d faces behind", n)
	}
	// Re-detection: same model, same frames, so the same boxes and embeddings.
	detected(t, st, first)
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	got := namesOf(t, st)
	if got["a.jpg"] != "Jane" || got["b.jpg"] != "Jane" {
		t.Errorf("labels after rebuild = %v, want Jane back on both", got)
	}
	if got["c.jpg"] != "" {
		t.Errorf("c.jpg became %q after the rebuild", got["c.jpg"])
	}
}

// A face nobody has seen before that looks like a known person should be labelled
// without being told — that is what makes the People page useful as the library
// grows.
func TestSyncLabelsNewFacesThatMatchAKnownPerson(t *testing.T) {
	ctx := context.Background()
	_, st, rec := setup(t)

	addItem(t, st, "a.jpg")
	detected(t, st, map[string][]float32{"a.jpg": vec(1, 0, 0, 0)})
	all, _ := st.AllFaces(ctx)
	if _, err := rec.Name(ctx, "Jane", all); err != nil {
		t.Fatal(err)
	}

	addItem(t, st, "new.jpg")
	detected(t, st, map[string][]float32{"new.jpg": vec(0.97, 0.03, 0, 0)})
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := namesOf(t, st); got["new.jpg"] != "Jane" {
		t.Errorf("the new face is %q, want Jane", got["new.jpg"])
	}
}

// Renaming onto an existing name is how you join two groups the clustering split
// apart, so the faces of both must end up under the surviving name — including
// the ones that are NOT similar enough to have merged on their own.
func TestRenameOntoAnExistingNameMerges(t *testing.T) {
	ctx := context.Background()
	cfg, st, rec := setup(t)

	addItem(t, st, "a.jpg")
	addItem(t, st, "b.jpg")
	detected(t, st, map[string][]float32{
		"a.jpg": vec(1, 0, 0, 0),
		"b.jpg": vec(0, 1, 0, 0), // deliberately far apart
	})
	all, _ := st.AllFaces(ctx)
	byPath := map[string][]store.Face{}
	for _, f := range all {
		byPath[f.Relpath] = append(byPath[f.Relpath], f)
	}
	if _, err := rec.Name(ctx, "Jane", byPath["a.jpg"]); err != nil {
		t.Fatal(err)
	}
	johnID, err := rec.Name(ctx, "John", byPath["b.jpg"])
	if err != nil {
		t.Fatal(err)
	}

	if err := rec.Rename(ctx, johnID, "Jane"); err != nil {
		t.Fatal(err)
	}
	got := namesOf(t, st)
	if got["a.jpg"] != "Jane" || got["b.jpg"] != "Jane" {
		t.Errorf("labels = %v, want both under Jane", got)
	}
	doc, _ := sidecar.ReadPeople(cfg.PeoplePath())
	if len(doc.People) != 1 || doc.People[0].Name != "Jane" || len(doc.People[0].Faces) != 2 {
		t.Errorf("people.json = %+v, want one Jane holding both examples", doc.People)
	}
}

func TestForgetKeepsTheFacesButDropsTheName(t *testing.T) {
	ctx := context.Background()
	cfg, st, rec := setup(t)

	addItem(t, st, "a.jpg")
	detected(t, st, map[string][]float32{"a.jpg": vec(1, 0, 0, 0)})
	all, _ := st.AllFaces(ctx)
	id, err := rec.Name(ctx, "Jane", all)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}

	if got := namesOf(t, st); got["a.jpg"] != "" {
		t.Errorf("face is still labelled %q after forgetting", got["a.jpg"])
	}
	after, _ := st.AllFaces(ctx)
	if len(after) != 1 {
		t.Errorf("got %d faces, want the detection itself to survive", len(after))
	}
	if doc, _ := sidecar.ReadPeople(cfg.PeoplePath()); len(doc.People) != 0 {
		t.Errorf("people.json still holds %+v", doc.People)
	}
	// And the group is offered again, so it can be named differently.
	if groups := store.ClusterUnnamed(after, rec.Match(), store.Grouping{}); len(groups) != 1 {
		t.Errorf("got %d unnamed groups, want the face back on the People page", len(groups))
	}
}

// Widening a group is a nudge toward naming it, so it is only worth keeping
// until the name lands: the name is the durable form of the same judgement and
// goes to people.json, where the override could never follow it (face ids are
// cache, and re-detection mints new ones).
func TestWideningStopsCountingOnceTheGroupIsNamed(t *testing.T) {
	ctx := context.Background()
	_, st, rec := setup(t)

	for _, rel := range []string{"a.jpg", "b.jpg"} {
		addItem(t, st, rel)
	}
	// Two halves of one person: 0.707 apart, so the library's 0.8 splits them.
	detected(t, st, map[string][]float32{
		"a.jpg": vec(1, 0, 0, 0),
		"b.jpg": vec(1, 1, 0, 0),
	})
	all, _ := st.AllFaces(ctx)
	if got := store.ClusterUnnamed(all, rec.Match(), store.Grouping{}); len(got) != 2 {
		t.Fatalf("got %d groups, want the person split in two to begin with", len(got))
	}

	if err := st.WidenFaceGroup(ctx, all[0].ID, 0.7); err != nil {
		t.Fatal(err)
	}
	widened, err := st.FaceGroupMatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	joined := store.ClusterUnnamed(all, rec.Match(), store.Grouping{Widened: widened})
	if len(joined) != 1 || joined[0].Faces != 2 {
		t.Fatalf("after widening: got %+v, want one group of both faces", joined)
	}

	// Naming it retires the override rather than leaving a row that would widen
	// whatever the next unnamed group keyed on that face turns out to be.
	if _, err := rec.Name(ctx, "Jane", pick(t, all, joined[0].FaceIDs)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.FaceGroupMatches(ctx); err != nil || len(got) != 0 {
		t.Errorf("widenings after naming = %v (err %v), want none", got, err)
	}
	if got := namesOf(t, st); got["a.jpg"] != "Jane" || got["b.jpg"] != "Jane" {
		t.Errorf("labels = %v, want both halves under Jane", got)
	}
}

// Editing people.json by hand is a supported repair path (it is a plain file in
// the same volume as the sidecars), so a person deleted from it must stop
// existing in the cache too.
func TestSyncTreatsPeopleJSONAsAuthoritative(t *testing.T) {
	ctx := context.Background()
	cfg, st, rec := setup(t)

	addItem(t, st, "a.jpg")
	detected(t, st, map[string][]float32{"a.jpg": vec(1, 0, 0, 0)})
	all, _ := st.AllFaces(ctx)
	if _, err := rec.Name(ctx, "Jane", all); err != nil {
		t.Fatal(err)
	}
	if err := sidecar.WritePeople(cfg.PeoplePath(), sidecar.People{}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := namesOf(t, st); got["a.jpg"] != "" {
		t.Errorf("face is %q, want unnamed after the name was removed from disk", got["a.jpg"])
	}
	if people, _ := st.ListPeople(ctx); len(people) != 0 {
		t.Errorf("people rows = %+v, want none", people)
	}
}

// Naming is a read-modify-write of one small file, and the HTTP layer will
// happily run two of them at once (two tabs, or a name landing while the worker
// reconciles). Without serialization the second write drops the first's entry.
func TestConcurrentNamingKeepsEveryName(t *testing.T) {
	ctx := context.Background()
	cfg, st, rec := setup(t)

	addItem(t, st, "a.jpg")
	addItem(t, st, "b.jpg")
	detected(t, st, map[string][]float32{
		"a.jpg": vec(1, 0, 0, 0),
		"b.jpg": vec(0, 1, 0, 0),
	})
	all, _ := st.AllFaces(ctx)
	byPath := map[string][]store.Face{}
	for _, f := range all {
		byPath[f.Relpath] = append(byPath[f.Relpath], f)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, tc := range []struct {
		name string
		rel  string
	}{{"Jane", "a.jpg"}, {"John", "b.jpg"}} {
		wg.Add(1)
		go func(name, rel string) {
			defer wg.Done()
			if _, err := rec.Name(ctx, name, byPath[rel]); err != nil {
				errs <- err
			}
		}(tc.name, tc.rel)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	doc, err := sidecar.ReadPeople(cfg.PeoplePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.People) != 2 {
		t.Errorf("people.json holds %+v, want both names", doc.People)
	}
	got := namesOf(t, st)
	if got["a.jpg"] != "Jane" || got["b.jpg"] != "John" {
		t.Errorf("labels = %v, want Jane and John", got)
	}
}

func TestIoU(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b [4]float64
		want float64
	}{
		{"identical", [4]float64{0.1, 0.1, 0.2, 0.2}, [4]float64{0.1, 0.1, 0.2, 0.2}, 1},
		{"disjoint", [4]float64{0, 0, 0.1, 0.1}, [4]float64{0.5, 0.5, 0.1, 0.1}, 0},
		{"touching edges only", [4]float64{0, 0, 0.1, 0.1}, [4]float64{0.1, 0, 0.1, 0.1}, 0},
		// A model upgrade that frames faces slightly differently still resolves,
		// which is the whole reason resolve() uses overlap and not equality.
		{"nudged", [4]float64{0.1, 0.1, 0.2, 0.2}, [4]float64{0.11, 0.11, 0.2, 0.2}, 0.82},
	} {
		got := iou(tc.a, tc.b)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("%s: iou = %.3f, want ~%.2f", tc.name, got, tc.want)
		}
	}
}

func TestSampleSpansTheWholeList(t *testing.T) {
	in := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if got := sample(in, 20); len(got) != 10 {
		t.Errorf("sample beyond the length changed it: %v", got)
	}
	got := sample(in, 4)
	if len(got) != 4 {
		t.Fatalf("sample(4) = %v", got)
	}
	// The point of an even stride: keep the variety, not just the head.
	if got[0] != 0 || got[3] < 7 {
		t.Errorf("sample(4) = %v, want entries spread across the input", got)
	}
}

func TestDedupeRefsKeepsFirstOccurrence(t *testing.T) {
	a := sidecar.FaceRef{Path: "a.jpg", Box: [4]float64{0.1, 0.1, 0.2, 0.2}}
	b := sidecar.FaceRef{Path: "a.jpg", Box: [4]float64{0.5, 0.1, 0.2, 0.2}}
	got := dedupeRefs([]sidecar.FaceRef{a, b, a})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("dedupeRefs = %+v", got)
	}
}

// pick resolves face ids back to the faces themselves, the way the HTTP layer
// does when naming a group.
func pick(t *testing.T, all []store.Face, ids []int64) []store.Face {
	t.Helper()
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []store.Face
	for _, f := range all {
		if want[f.ID] {
			out = append(out, f)
		}
	}
	return out
}

func mustPerson(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	people, err := st.ListPeople(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("no person named %q", name)
	return 0
}

// An item the worker can't do anything with keeps faces_done = 0, so it stays in
// the queue — and the queue is asked for its head. A batch's worth of those at
// the front used to be the end of detection: the same twenty items came back
// every tick, forever, and nothing behind them was ever looked at. From outside
// that is a face count that stops at some number and never moves again.
//
// Nothing here can be read (the paths don't exist and there is no ffmpeg
// configured), so every item fails the same way a corrupt file would.
func TestUnreadableItemsDoNotStopTheQueue(t *testing.T) {
	_, st, rec := setup(t)
	const total = batch + 5
	for i := 0; i < total; i++ {
		addItem(t, st, fmt.Sprintf("broken/%02d.jpg", i))
	}

	rec.pass(context.Background())

	if got := rec.Skipped(); got != total {
		t.Errorf("the pass gave up on %d items, want all %d tried", got, total)
	}
}
