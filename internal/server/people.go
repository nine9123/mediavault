package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"mediavault/internal/media"
	"mediavault/internal/store"
)

// The People page. Two kinds of thing appear on it, and the difference is worth
// keeping straight in the URL as well as the UI:
//
//	p<id>  a named person   — a human said "this is Jane"
//	f<id>  an unnamed group — the app noticed these faces look alike
//
// A group has no id of its own to link to, because the grouping is recomputed
// from the embeddings rather than stored (see store.ClusterUnnamed). It is
// therefore addressed by the lowest face id in it, which is stable for as long as
// the group is.
const (
	personPrefix  = "p"
	clusterPrefix = "f"
)

// facesOn reports whether face recognition is available.
func (s *Server) facesOn() bool { return s.rec != nil && s.rec.Enabled() }

// requireFaces writes the 503 that tells the UI to hide the People page.
func (s *Server) requireFaces(w http.ResponseWriter) bool {
	if s.facesOn() {
		return true
	}
	http.Error(w, "face recognition is not configured (set FACE_URL)", http.StatusServiceUnavailable)
	return false
}

// maxClusters bounds the unnamed groups returned to the UI. A freshly-detected
// library has thousands of one-off faces; the page leads with the big groups
// (ClusterUnnamed sorts by size) and reports how many were left out rather than
// silently truncating.
const maxClusters = 120

// How far a single group can be widened, and by how much per click.
//
// The floor is not a property of the model the way the threshold is — it is the
// point past which "these look alike" stops meaning anything for any model, and
// one group swallows the page. A user who really wants everything in one group
// has a better tool for it: name them all the same thing.
const (
	widenStep = 0.1
	minMatch  = 0.1
)

type personDTO struct {
	Ref      string  `json:"ref"`
	Name     string  `json:"name,omitempty"`
	Faces    int     `json:"faces"`
	Items    int     `json:"items"`
	CoverURL string  `json:"cover_url,omitempty"`
	Match    float64 `json:"match,omitempty"`  // set only on a widened group
	Merged   bool    `json:"merged,omitempty"` // set only on a manually merged group
}

// handlePeople lists named people first, then the unnamed groups worth naming.
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	people, err := s.store.ListPeople(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	match := s.rec.Match()
	clusters := store.ClusterUnnamed(faces, match, manual)

	outPeople := make([]personDTO, 0, len(people))
	for _, p := range people {
		outPeople = append(outPeople, personDTO{
			Ref:      personPrefix + strconv.FormatInt(p.ID, 10),
			Name:     p.Name,
			Faces:    p.Faces,
			Items:    p.Items,
			CoverURL: faceURL(p.CoverFaceID),
		})
	}
	shown := clusters
	if len(shown) > maxClusters {
		shown = shown[:maxClusters]
	}
	outClusters := make([]personDTO, 0, len(shown))
	for _, c := range shown {
		d := personDTO{
			Ref:      clusterPrefix + strconv.FormatInt(c.KeyFaceID, 10),
			Faces:    c.Faces,
			Items:    c.Items,
			CoverURL: faceURL(c.CoverFaceID),
			Merged:   c.Merged,
		}
		// Only when it differs, so the page can tell "widened" from "normal"
		// without knowing the library-wide number.
		if c.Match < match {
			d.Match = c.Match
		}
		outClusters = append(outClusters, d)
	}

	resp := map[string]any{
		"people":         outPeople,
		"clusters":       outClusters,
		"clusters_total": len(clusters),
		"match":          match,
	}
	if visual, done, nfaces, named, err := s.store.FaceCounts(r.Context()); err == nil {
		resp["progress"] = map[string]any{
			"visual": visual, "done": done, "faces": nfaces, "named": named,
		}
	}
	writeJSON(w, resp)
}

// handlePeopleItems returns the items a person (or an unnamed group) appears in.
func (s *Server) handlePeopleItems(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	limit := clampAtoi(r.URL.Query().Get("limit"), 200, 1, 500)
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	// before=<id>: the next page, older than the cursor. Same shape as
	// /api/items, so the grid's infinite scroll works here unchanged.
	var before int64
	if b := r.URL.Query().Get("before"); b != "" {
		before, _ = strconv.ParseInt(b, 10, 64)
	}

	if id, ok := personRef(ref); ok {
		items, err := s.store.ItemsForPerson(r.Context(), id, before, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeItems(w, items)
		return
	}
	faceID, ok := clusterRef(ref)
	if !ok {
		http.Error(w, "bad ref", http.StatusBadRequest)
		return
	}
	ids, err := s.clusterFaceIDs(r, faceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items, err := s.store.ItemsForFaces(r.Context(), ids, before, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeItems(w, items)
}

// handleNamePerson names an unnamed group, or renames a person. Naming with a
// name that already exists merges into that person — which is also how you join
// two groups the clustering split apart.
func (s *Server) handleNamePerson(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	var req struct {
		Ref  string `json:"ref"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	if len(name) > 120 {
		http.Error(w, "name is too long", http.StatusBadRequest)
		return
	}

	if id, ok := personRef(req.Ref); ok {
		if err := s.rec.Rename(r.Context(), id, name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "name": name})
		return
	}
	faceID, ok := clusterRef(req.Ref)
	if !ok {
		http.Error(w, "bad ref", http.StatusBadRequest)
		return
	}
	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ids := store.ClusterContaining(faces, s.rec.Match(), manual, faceID)
	if len(ids) == 0 {
		// The group dissolved under us — someone else named it, or a rescan
		// changed the face set. Saying so beats silently creating an empty person.
		http.Error(w, "that group of faces no longer exists — reload the People page", http.StatusConflict)
		return
	}
	chosen := make([]store.Face, 0, len(ids))
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, f := range faces {
		if want[f.ID] {
			chosen = append(chosen, f)
		}
	}
	pid, err := s.rec.Name(r.Context(), name, chosen)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"ok":    true,
		"ref":   personPrefix + strconv.FormatInt(pid, 10),
		"name":  name,
		"faces": len(chosen),
	})
}

// handleForgetPerson drops a name. The faces survive as unnamed detections, so
// the People page offers the group again — the detections are evidence, and only
// the name was a judgement.
func (s *Server) handleForgetPerson(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	id, ok := personRef(strings.TrimSpace(r.URL.Query().Get("ref")))
	if !ok {
		http.Error(w, "bad ref", http.StatusBadRequest)
		return
	}
	if err := s.rec.Forget(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleWidenGroup lowers one group's similarity threshold and re-groups just
// there, so the other half of a person split in two gets pulled in.
//
// This is the cheap repair for the error clustering deliberately makes (see
// store.group): everything else on the page is grouped correctly, so re-running
// the library at a looser number would trade this split for merged strangers
// elsewhere. Nothing is re-detected — the embeddings are already stored, and the
// only thing that changes is the number they are compared at.
func (s *Server) handleWidenGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	var req struct {
		Ref  string  `json:"ref"`
		Step float64 `json:"step"` // optional; how far to lower it
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	faceID, ok := clusterRef(strings.TrimSpace(req.Ref))
	if !ok {
		// A named person has no threshold of their own: their faces are matched
		// against their examples, not clustered.
		http.Error(w, "widening applies to unnamed groups only", http.StatusBadRequest)
		return
	}
	step := req.Step
	if step <= 0 {
		step = widenStep
	}
	if step > 0.5 {
		step = 0.5
	}

	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	match := s.rec.Match()
	before := store.ClusterUnnamed(faces, match, manual)
	cur, ok := store.ClusterOf(before, faceID)
	if !ok {
		http.Error(w, "that group of faces no longer exists — reload the People page", http.StatusConflict)
		return
	}
	// Rounded, because this number is shown on the card and stored: repeated
	// subtraction of 0.1 in binary floating point reaches 0.7000000000000001,
	// and two hundredths is finer than any threshold anyone reasons about.
	next := math.Round((cur.Match-step)*100) / 100
	if next < minMatch {
		next = minMatch
	}
	if next >= cur.Match {
		http.Error(w, "this group is already as wide as it goes — merge it with the other group by hand instead",
			http.StatusConflict)
		return
	}

	if err := s.store.WidenFaceGroup(r.Context(), cur.KeyFaceID, next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-group with the new number in hand rather than making the page ask again:
	// what the user wants to know is whether anything joined.
	manual.Widened[cur.KeyFaceID] = next
	got, ok := store.ClusterOf(store.ClusterUnnamed(faces, match, manual), cur.KeyFaceID)
	if !ok {
		http.Error(w, "regrouping lost that group — reload the People page", http.StatusInternalServerError)
		return
	}
	in := make(map[int64]bool, len(got.FaceIDs))
	for _, id := range got.FaceIDs {
		in[id] = true
	}
	absorbed := 0
	for _, c := range before {
		if c.KeyFaceID != cur.KeyFaceID && in[c.KeyFaceID] {
			absorbed++
		}
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"ref":      clusterPrefix + strconv.FormatInt(got.KeyFaceID, 10),
		"match":    next,
		"faces":    got.Faces,
		"items":    got.Items,
		"absorbed": absorbed, // how many other groups joined this one
	})
}

// handleResetGroup puts a widened group back under the library-wide threshold.
func (s *Server) handleResetGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	faceID, ok := clusterRef(strings.TrimSpace(r.URL.Query().Get("ref")))
	if !ok {
		http.Error(w, "bad ref", http.StatusBadRequest)
		return
	}
	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c, ok := store.ClusterOf(store.ClusterUnnamed(faces, s.rec.Match(), manual), faceID)
	if !ok {
		http.Error(w, "that group of faces no longer exists — reload the People page", http.StatusConflict)
		return
	}
	// The whole group, not its key: widening merges groups, so the face the
	// override was written against may be any member by now.
	if err := s.store.ResetFaceGroup(r.Context(), c.FaceIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleMergeGroups joins two unnamed groups that a person can see are the same
// person, whatever the embeddings make of them.
//
// It is the other half of widening, and the difference is who is being asked.
// Widening puts the question back to the model at a looser number, which works
// when the two halves really are close. When they are not — the same face twenty
// years and two haircuts apart, or one of them lit from behind — no number short
// of one that also reaches strangers will join them, and each further click of
// Widen is a bigger gamble on the rest of the page. A merge asks the model
// nothing. It is applied after grouping, so no other group can move as a result,
// and the only faces it can affect are the ones the user pointed at.
func (s *Server) handleMergeGroups(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	var req struct {
		Ref  string `json:"ref"`
		Into string `json:"into"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	fromID, fromOK := clusterRef(strings.TrimSpace(req.Ref))
	intoID, intoOK := clusterRef(strings.TrimSpace(req.Into))
	if !fromOK || !intoOK {
		// A named person is not a group: their faces are matched against their
		// examples rather than clustered, so there is nothing to join them to.
		http.Error(w, "merging joins two unnamed groups — to add a group to a named person, name it with that person's name",
			http.StatusBadRequest)
		return
	}
	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	match := s.rec.Match()
	before := store.ClusterUnnamed(faces, match, manual)
	from, ok := store.ClusterOf(before, fromID)
	if !ok {
		http.Error(w, "that group of faces no longer exists — reload the People page", http.StatusConflict)
		return
	}
	into, ok := store.ClusterOf(before, intoID)
	if !ok {
		http.Error(w, "that group of faces no longer exists — reload the People page", http.StatusConflict)
		return
	}
	if from.KeyFaceID == into.KeyFaceID {
		// Both refs landed in one group — a stale page, or a widening joined them
		// between the two clicks. Nothing to do, and saying so beats writing a
		// merge that explains nothing later.
		http.Error(w, "those two are already the same group — reload the People page", http.StatusConflict)
		return
	}
	if err := s.store.MergeFaceGroups(r.Context(), from.FaceIDs, into.FaceIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-group with the merge in hand rather than making the page ask again: what
	// the user wants to know is how big the group they just made is.
	merged, err := s.store.FaceGroupMerges(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	manual.Merged = merged
	got, ok := store.ClusterOf(store.ClusterUnnamed(faces, match, manual), from.KeyFaceID)
	if !ok {
		http.Error(w, "regrouping lost that group — reload the People page", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"ok":    true,
		"ref":   clusterPrefix + strconv.FormatInt(got.KeyFaceID, 10),
		"faces": got.Faces,
		"items": got.Items,
	})
}

// handleUnmergeGroup undoes the manual joins on a group, handing its faces back
// to the clustering. Every merge the group holds goes, not the last one: after
// three groups have been fused there is no "last one" the page could name.
func (s *Server) handleUnmergeGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	faceID, ok := clusterRef(strings.TrimSpace(r.URL.Query().Get("ref")))
	if !ok {
		http.Error(w, "bad ref", http.StatusBadRequest)
		return
	}
	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c, ok := store.ClusterOf(store.ClusterUnnamed(faces, s.rec.Match(), manual), faceID)
	if !ok {
		http.Error(w, "that group of faces no longer exists — reload the People page", http.StatusConflict)
		return
	}
	if err := s.store.UnmergeFaceGroup(r.Context(), c.FaceIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleResetFaces clears every detection so the service is asked again (after
// changing the model, say). Names are untouched: they live in people.json and are
// re-attached as detection catches up.
func (s *Server) handleResetFaces(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	if err := s.store.ResetFaces(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The reset dropped the people rows too, so the worker has to rebuild them
	// from people.json — it would otherwise wait for a detection to notice.
	s.rec.Invalidate()
	writeJSON(w, map[string]any{"ok": true})
}

// handleFaceCrop serves a cropped face thumbnail, generating it on first request.
// Crops are made lazily rather than during detection because most faces are never
// looked at: only a group's cover ever appears on screen.
func (s *Server) handleFaceCrop(w http.ResponseWriter, r *http.Request) {
	if !s.requireFaces(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad face id", http.StatusBadRequest)
		return
	}
	f, err := s.store.FaceByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dst := media.FaceCropPath(s.cfg.FaceDir(), f.Relpath, f.X, f.Y, f.W, f.H)
	if _, err := os.Stat(dst); err != nil {
		abs, ok := s.resolve(f.Relpath)
		if !ok {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		// Wait for a slot rather than starting an ffmpeg per request: opening the
		// People page for the first time asks for every cover at once.
		select {
		case s.cropSem <- struct{}{}:
			defer func() { <-s.cropSem }()
		case <-r.Context().Done():
			return // the browser gave up; don't start work for nobody
		}
		// Someone else may have finished this exact crop while we queued.
		if _, err := os.Stat(dst); err != nil {
			// Crop to a unique temp name and rename into place, so two requests for
			// the same cover can't have one serve what the other is still writing.
			tmp := fmt.Sprintf("%s.%d.tmp", dst, s.cropSeq.Add(1))
			if err := media.FaceCrop(r.Context(), s.cfg, abs, f.Type, f.Offset, f.X, f.Y, f.W, f.H, tmp); err != nil {
				_ = os.Remove(tmp)
				http.Error(w, "could not crop that face: "+err.Error(), http.StatusUnprocessableEntity)
				return
			}
			if err := os.Rename(tmp, dst); err != nil {
				_ = os.Remove(tmp)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, dst)
}

// faceURL is the crop endpoint for a face, or "" for no face at all (a person
// whose only photos went missing).
func faceURL(faceID int64) string {
	if faceID <= 0 {
		return ""
	}
	return "/api/face/" + strconv.FormatInt(faceID, 10)
}

// clusterFaceIDs resolves an unnamed group's key face to the whole group.
func (s *Server) clusterFaceIDs(r *http.Request, keyFaceID int64) ([]int64, error) {
	faces, manual, err := s.faceGrouping(r)
	if err != nil {
		return nil, err
	}
	return store.ClusterContaining(faces, s.rec.Match(), manual, keyFaceID), nil
}

// faceGrouping fetches every input to clustering: all the faces, plus the manual
// corrections in force. Always together — grouping the page one way and naming a
// group another would label a different set of faces than the one shown.
func (s *Server) faceGrouping(r *http.Request) ([]store.Face, store.Grouping, error) {
	faces, err := s.store.AllFaces(r.Context())
	if err != nil {
		return nil, store.Grouping{}, err
	}
	widened, err := s.store.FaceGroupMatches(r.Context())
	if err != nil {
		return nil, store.Grouping{}, err
	}
	merged, err := s.store.FaceGroupMerges(r.Context())
	if err != nil {
		return nil, store.Grouping{}, err
	}
	return faces, store.Grouping{Widened: widened, Merged: merged}, nil
}

// faceDTO is one face in one item, as the viewer's card shows it. Deliberately
// not personDTO: the counts on that would be about this single face, which reads
// as information and isn't.
type faceDTO struct {
	Ref      string `json:"ref"`
	Name     string `json:"name,omitempty"`
	CoverURL string `json:"cover_url,omitempty"`
}

// facesForItemDTO is the "who is in this?" list shown in the viewer's card. An
// unnamed face links to the group it belongs to, so the card is also a way into
// naming whoever is in front of you.
func (s *Server) facesForItemDTO(r *http.Request, itemID int64) []faceDTO {
	if !s.facesOn() {
		return nil
	}
	list, err := s.store.FacesForItem(r.Context(), itemID)
	if err != nil || len(list) == 0 {
		return nil
	}
	out := make([]faceDTO, 0, len(list))
	for _, f := range list {
		ref := clusterPrefix + strconv.FormatInt(f.ID, 10)
		if f.PersonID > 0 {
			ref = personPrefix + strconv.FormatInt(f.PersonID, 10)
		}
		out = append(out, faceDTO{Ref: ref, Name: f.PersonName, CoverURL: faceURL(f.ID)})
	}
	return out
}

// personRef parses "p<id>".
func personRef(ref string) (int64, bool) { return parseRef(ref, personPrefix) }

// clusterRef parses "f<face id>".
func clusterRef(ref string) (int64, bool) { return parseRef(ref, clusterPrefix) }

func parseRef(ref, prefix string) (int64, bool) {
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(ref[len(prefix):], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
