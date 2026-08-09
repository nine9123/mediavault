package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/faces"
	"mediavault/internal/model"
	"mediavault/internal/store"
)

// These drive the People endpoints over the real router with a real database.
// Detection is stubbed by writing faces straight into the store — the same thing
// the worker does with what the recognition service returns — so nothing here
// needs FACE_URL to actually answer.

func peopleServer(t *testing.T) (*Server, *store.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		MediaRoot: dir, DataDir: dir, SidecarDir: dir,
		FaceURL:   "http://stub.invalid", // only has to be non-empty: it is never called
		FaceMatch: 0.8,
	}
	st, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(cfg, st, nil, nil, faces.New(cfg, st))
	return s, st, s.Routes()
}

// detect adds one item per relpath with one face on it, at the given direction.
func detect(t *testing.T, st *store.Store, rel string, a, b, c, d float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := st.WriteBatch(ctx, []store.ItemWrite{{Item: model.Item{
		Relpath: rel, Size: 1024, MTime: now, Type: model.MediaImage,
		Width: 800, Height: 600, IndexedAt: now, SeenAt: now,
	}}}); err != nil {
		t.Fatal(err)
	}
	n := math.Sqrt(a*a + b*b + c*c + d*d)
	v := []float32{float32(a / n), float32(b / n), float32(c / n), float32(d / n)}
	if err := st.SetFaces(ctx, rel, []store.FaceWrite{
		{X: 0.25, Y: 0.2, W: 0.2, H: 0.3, Score: 1, Vec: v},
	}); err != nil {
		t.Fatal(err)
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// clusterRefs is the page's view: one ref per unnamed group, with its match.
func clusterRefs(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	code, d := do(t, h, "GET", "/api/people", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/people: status %d", code)
	}
	raw, _ := d["clusters"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		m, _ := c.(map[string]any)
		out = append(out, m)
	}
	return out
}

// pageThrough walks a ref's items the way the grid does: a first page, then
// before=<smallest id shown> until a short page ends it. Returns the relpaths in
// the order they arrived, and fails rather than looping forever if the cursor is
// ignored.
func pageThrough(t *testing.T, h http.Handler, ref string, perPage int) []string {
	t.Helper()
	var out []string
	before := int64(0)
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatalf("still paging after %d requests: the before cursor is being ignored", page)
		}
		url := "/api/people/items?ref=" + ref + "&limit=" + strconv.Itoa(perPage)
		if before > 0 {
			url += "&before=" + strconv.FormatInt(before, 10)
		}
		code, d := do(t, h, "GET", url, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d", url, code)
		}
		raw, _ := d["items"].([]any)
		for _, it := range raw {
			m, _ := it.(map[string]any)
			out = append(out, m["relpath"].(string))
			if id := int64(m["id"].(float64)); before == 0 || id < before {
				before = id
			}
		}
		if len(raw) < perPage {
			return out
		}
	}
}

// The whole point of the endpoint: one person shown as two groups becomes one,
// and the other person on the page is left exactly as they were.
func TestWidenGroupJoinsTheSplitHalfAndLeavesTheRestAlone(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 0.99, 0.01, 0, 0)
	detect(t, st, "c.jpg", 1, 1, 0, 0) // same person, 0.71 away: split at 0.8
	detect(t, st, "d.jpg", 0, 0, 1, 0) // somebody else entirely

	before := clusterRefs(t, h)
	if len(before) != 3 {
		t.Fatalf("got %d groups, want the person split in two plus the third face", len(before))
	}

	code, d := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+before[1]["ref"].(string)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("widen: status %d (%v)", code, d)
	}
	if d["absorbed"].(float64) != 1 {
		t.Errorf("absorbed = %v, want the other half of the split", d["absorbed"])
	}
	if d["faces"].(float64) != 3 || d["match"].(float64) != 0.7 {
		t.Errorf("widened group = %v, want 3 faces at match 0.7", d)
	}

	after := clusterRefs(t, h)
	if len(after) != 2 {
		t.Fatalf("got %d groups after widening, want 2", len(after))
	}
	if after[0]["faces"].(float64) != 3 || after[0]["match"].(float64) != 0.7 {
		t.Errorf("joined group = %v, want 3 faces reported at match 0.7", after[0])
	}
	if _, widened := after[1]["match"]; widened {
		t.Errorf("the untouched group reports a match of its own: %v", after[1])
	}

	// Undo puts the split back, so a widening that pulled in a stranger is not a
	// one-way door.
	code, _ = do(t, h, "DELETE", "/api/people/regroup?ref="+after[0]["ref"].(string), "")
	if code != http.StatusOK {
		t.Fatalf("reset: status %d", code)
	}
	if got := clusterRefs(t, h); len(got) != 3 {
		t.Errorf("got %d groups after resetting, want the original 3", len(got))
	}
}

// Naming a widened group has to label everything the card claimed was in it.
func TestNamingAWidenedGroupLabelsAllOfIt(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 1, 1, 0, 0)

	ref := clusterRefs(t, h)[0]["ref"].(string)
	if code, d := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+ref+`"}`); code != http.StatusOK {
		t.Fatalf("widen: status %d (%v)", code, d)
	}
	joined := clusterRefs(t, h)
	if len(joined) != 1 {
		t.Fatalf("got %d groups, want them joined", len(joined))
	}
	code, d := do(t, h, "PUT", "/api/people", `{"ref":"`+joined[0]["ref"].(string)+`","name":"Jane"}`)
	if code != http.StatusOK {
		t.Fatalf("name: status %d (%v)", code, d)
	}
	if d["faces"].(float64) != 2 {
		t.Errorf("named %v faces, want both halves of the widened group", d["faces"])
	}
}

// Merging is for the split widening cannot repair. These three faces are
// mutually orthogonal — no threshold down to the floor would bring any two of
// them together — so the only thing that can join two of them is somebody saying
// so, and the third must not move.
func TestMergeGroupsFusesTwoGroupsNoWideningWouldReach(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 0, 1, 0, 0) // the same person, as far as the user is concerned
	detect(t, st, "c.jpg", 0, 0, 1, 0) // somebody else

	before := clusterRefs(t, h)
	if len(before) != 3 {
		t.Fatalf("got %d groups, want one per face: nothing here looks alike", len(before))
	}
	code, d := do(t, h, "POST", "/api/people/merge",
		`{"ref":"`+before[0]["ref"].(string)+`","into":"`+before[1]["ref"].(string)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("merge: status %d (%v)", code, d)
	}
	if d["faces"].(float64) != 2 || d["items"].(float64) != 2 {
		t.Errorf("merged group = %v, want 2 faces in 2 items", d)
	}

	after := clusterRefs(t, h)
	if len(after) != 2 {
		t.Fatalf("got %d groups after merging, want 2", len(after))
	}
	if after[0]["faces"].(float64) != 2 || after[0]["merged"] != true {
		t.Errorf("merged group = %v, want 2 faces flagged as merged", after[0])
	}
	// A merge compares nothing, so the group is still grouped at the library's
	// number and the card must not imply it was widened.
	if _, widened := after[0]["match"]; widened {
		t.Errorf("merged group reports a match of its own: %v", after[0])
	}
	if _, merged := after[1]["merged"]; merged {
		t.Errorf("the bystander was caught up in the merge: %v", after[1])
	}

	// Naming it labels both halves — being able to name them once is the point.
	code, d = do(t, h, "PUT", "/api/people", `{"ref":"`+after[0]["ref"].(string)+`","name":"Jane"}`)
	if code != http.StatusOK {
		t.Fatalf("name: status %d (%v)", code, d)
	}
	if d["faces"].(float64) != 2 {
		t.Errorf("named %v faces, want both halves of the merge", d["faces"])
	}
}

func TestMergingAThirdGroupInAndUnmergingThemAll(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 0, 1, 0, 0)
	detect(t, st, "c.jpg", 0, 0, 1, 0)

	refs := clusterRefs(t, h)
	if code, d := do(t, h, "POST", "/api/people/merge",
		`{"ref":"`+refs[0]["ref"].(string)+`","into":"`+refs[1]["ref"].(string)+`"}`); code != http.StatusOK {
		t.Fatalf("first merge: status %d (%v)", code, d)
	}
	// The biggest group sorts first, so refs[0] is now the pair and refs[1] the
	// straggler being added to it.
	refs = clusterRefs(t, h)
	code, d := do(t, h, "POST", "/api/people/merge",
		`{"ref":"`+refs[1]["ref"].(string)+`","into":"`+refs[0]["ref"].(string)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second merge: status %d (%v)", code, d)
	}
	if d["faces"].(float64) != 3 {
		t.Errorf("merged group = %v, want all three: merging onto a merge is one group, not two", d)
	}
	joined := clusterRefs(t, h)
	if len(joined) != 1 {
		t.Fatalf("got %d groups, want them all in one", len(joined))
	}

	// One undo rather than one per merge: once three groups are fused there is no
	// "last merge" the page could offer to take back.
	if code, _ := do(t, h, "DELETE", "/api/people/merge?ref="+joined[0]["ref"].(string), ""); code != http.StatusOK {
		t.Fatalf("unmerge: status %d", code)
	}
	if got := clusterRefs(t, h); len(got) != 3 {
		t.Errorf("got %d groups after unmerging, want the original 3", len(got))
	}
}

func TestMergeGroupsRejectsAnythingThatIsntTwoGroups(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 0, 1, 0, 0)
	ref := clusterRefs(t, h)[0]["ref"].(string)

	if code, _ := do(t, h, "POST", "/api/people/merge", `{"ref":"`+ref+`","into":"p1"}`); code != http.StatusBadRequest {
		t.Errorf("a named person as the target got status %d, want 400", code)
	}
	if code, _ := do(t, h, "POST", "/api/people/merge", `{"ref":"`+ref+`","into":"f9999"}`); code != http.StatusConflict {
		t.Errorf("an unknown group got status %d, want 409", code)
	}
	if code, _ := do(t, h, "POST", "/api/people/merge", `{"ref":"`+ref+`","into":"`+ref+`"}`); code != http.StatusConflict {
		t.Errorf("merging a group with itself got status %d, want 409", code)
	}
	// None of those should have written anything.
	for _, c := range clusterRefs(t, h) {
		if _, merged := c["merged"]; merged {
			t.Errorf("a rejected merge still marked %v as merged", c)
		}
	}
	if code, _ := do(t, h, "DELETE", "/api/people/merge?ref=p1", ""); code != http.StatusBadRequest {
		t.Errorf("unmerging a person ref got status %d, want 400", code)
	}
}

func TestWidenGroupRejectsBadRefsAndStopsAtTheFloor(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)

	if code, _ := do(t, h, "POST", "/api/people/regroup", `{"ref":"p1"}`); code != http.StatusBadRequest {
		t.Errorf("a person ref got status %d, want 400: only unnamed groups have a threshold", code)
	}
	if code, _ := do(t, h, "POST", "/api/people/regroup", `{"ref":"f9999"}`); code != http.StatusConflict {
		t.Errorf("an unknown group got status %d, want 409", code)
	}

	ref := clusterRefs(t, h)[0]["ref"].(string)
	// 0.8 down to the 0.1 floor in steps of 0.1, then it must refuse rather than
	// silently do nothing.
	for i := 0; i < 7; i++ {
		if code, d := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+ref+`"}`); code != http.StatusOK {
			t.Fatalf("widen %d: status %d (%v)", i, code, d)
		}
	}
	code, _ := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+ref+`"}`)
	if code != http.StatusConflict {
		t.Errorf("widening past the floor got status %d, want 409", code)
	}
}

// Somebody who appears in more items than one page holds has to be reachable
// past that page — before this, the grid asked for a single page and the person
// view stopped there however much was behind it.
func TestPeopleItemsPageBeyondTheFirstPage(t *testing.T) {
	_, st, h := peopleServer(t)
	const total = 5
	for i := 0; i < total; i++ {
		detect(t, st, strconv.Itoa(i)+".jpg", 1, 0, 0, 0)
	}

	// An unnamed group first: it has no id, so its faces are resolved per request
	// and the cursor has to survive that.
	ref := clusterRefs(t, h)[0]["ref"].(string)
	got := pageThrough(t, h, ref, 2)
	if len(got) != total || len(uniq(got)) != total {
		t.Errorf("paged %d items (%d distinct) for the group, want %d each", len(got), len(uniq(got)), total)
	}

	// And the same for a named person, which is the ref the People page hands
	// over once the group has a name.
	if code, d := do(t, h, "PUT", "/api/people", `{"ref":"`+ref+`","name":"Jane"}`); code != http.StatusOK {
		t.Fatalf("name: status %d (%v)", code, d)
	}
	code, d := do(t, h, "GET", "/api/people", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/people: status %d", code)
	}
	people, _ := d["people"].([]any)
	if len(people) != 1 {
		t.Fatalf("got %d people, want Jane", len(people))
	}
	pref := people[0].(map[string]any)["ref"].(string)
	got = pageThrough(t, h, pref, 2)
	if len(got) != total || len(uniq(got)) != total {
		t.Errorf("paged %d items (%d distinct) for Jane, want %d each", len(got), len(uniq(got)), total)
	}
}

// The grouping is cached between requests, so the thing to prove is that the
// cache notices the two ways its inputs move underneath it: the detector finding
// a face, and a face changing hands. Both are invisible to this layer — they
// happen in the store — which is exactly why the fingerprint exists.
func TestPeopleSeesFacesThatArriveAfterTheFirstRequest(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	if got := clusterRefs(t, h); len(got) != 1 {
		t.Fatalf("got %d groups, want the one face", len(got))
	}

	// A detection lands between requests.
	detect(t, st, "b.jpg", 0, 1, 0, 0)
	groups := clusterRefs(t, h)
	if len(groups) != 2 {
		t.Fatalf("got %d groups after a second face arrived, want 2", len(groups))
	}

	// And a face changes hands: naming one group must take it off the page,
	// leaving the other where it was.
	if code, d := do(t, h, "PUT", "/api/people", `{"ref":"`+groups[0]["ref"].(string)+`","name":"Jane"}`); code != http.StatusOK {
		t.Fatalf("name: status %d (%v)", code, d)
	}
	code, d := do(t, h, "GET", "/api/people", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/people: status %d", code)
	}
	if people, _ := d["people"].([]any); len(people) != 1 {
		t.Errorf("got %d named people, want Jane", len(people))
	}
	if left, _ := d["clusters"].([]any); len(left) != 1 {
		t.Errorf("got %d unnamed groups, want the one nobody named", len(left))
	}
}

func uniq(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
