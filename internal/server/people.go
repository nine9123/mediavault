package server

import (
	"encoding/json"
	"fmt"
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

type personDTO struct {
	Ref      string `json:"ref"`
	Name     string `json:"name,omitempty"`
	Faces    int    `json:"faces"`
	Items    int    `json:"items"`
	CoverURL string `json:"cover_url,omitempty"`
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
	faces, err := s.store.AllFaces(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	clusters := store.ClusterUnnamed(faces, s.rec.Match())

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
		outClusters = append(outClusters, personDTO{
			Ref:      clusterPrefix + strconv.FormatInt(c.KeyFaceID, 10),
			Faces:    c.Faces,
			Items:    c.Items,
			CoverURL: faceURL(c.CoverFaceID),
		})
	}

	resp := map[string]any{
		"people":         outPeople,
		"clusters":       outClusters,
		"clusters_total": len(clusters),
		"match":          s.rec.Match(),
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

	if id, ok := personRef(ref); ok {
		items, err := s.store.ItemsForPerson(r.Context(), id, limit)
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
	items, err := s.store.ItemsForFaces(r.Context(), ids, limit)
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
	faces, err := s.store.AllFaces(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ids := store.ClusterContaining(faces, s.rec.Match(), faceID)
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
	faces, err := s.store.AllFaces(r.Context())
	if err != nil {
		return nil, err
	}
	return store.ClusterContaining(faces, s.rec.Match(), keyFaceID), nil
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
