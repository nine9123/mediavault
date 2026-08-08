// Faces and people: storage for the optional recognition service, plus the
// clustering that turns a pile of face embeddings into "these are all the same
// person". Kept in the store package because that is where the vectors live and
// because store is the only package allowed to talk to SQLite.
//
// Scale assumption, same as the rest of the vector code here: a personal library
// has thousands of faces, not millions, so grouping them is a brute-force pass
// over a few tens of MB in memory. That is milliseconds, needs no ANN index and
// no vector database, and is only worth revisiting if this grows by two orders
// of magnitude.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"

	"mediavault/internal/media"
	"mediavault/internal/model"
)

// Face is one detected face, joined to enough of its item to crop it.
type Face struct {
	ID         int64
	ItemID     int64
	Relpath    string
	Type       model.MediaType
	Offset     float64
	PersonID   int64  // 0 when unassigned
	PersonName string // "" when unassigned
	X, Y, W, H float64
	Score      float64
	Vec        []float32
}

// FaceWrite is one detection the worker hands us. It mirrors face.Detection
// without importing that package, keeping store free of the HTTP client.
type FaceWrite struct {
	X, Y, W, H float64
	Score      float64
	Vec        []float32
}

// Person is a named identity plus the counts the People page shows.
type Person struct {
	ID          int64
	Name        string
	Faces       int
	Items       int
	CoverFaceID int64
}

// FaceCluster is a group of unnamed faces the app believes are one person.
// KeyFaceID (the lowest face id in the group) identifies it: cluster ids can't
// be stored, since the grouping is recomputed from the embeddings each time, but
// the same face set always yields the same lowest id.
type FaceCluster struct {
	KeyFaceID   int64
	CoverFaceID int64
	Faces       int
	Items       int
	Match       float64 // threshold this group was grouped at; below the library's if widened
	FaceIDs     []int64
}

// Widened is the per-group similarity overrides in force: a face id -> the
// lowered threshold the group containing it is clustered at.
//
// It exists because the one number that controls grouping is global, and the
// mistake it makes is local: one person whose photographs vary a lot lands in
// two groups while everybody else is grouped correctly. Lowering FACE_MATCH for
// the whole library to fix that merges strangers elsewhere. So a widening is
// recorded against the group the user pointed at — keyed on its key face, the
// same handle the UI addresses it by — and applies only there.
type Widened map[int64]float64

const faceCols = `f.id, f.item_id, i.relpath, i.type, COALESCE(i.offset_s,0),
	COALESCE(f.person_id,0), COALESCE(p.name,''), f.x, f.y, f.w, f.h, COALESCE(f.score,0), f.vec`

const faceFrom = `FROM faces f
	JOIN items i ON i.id = f.item_id
	LEFT JOIN people p ON p.id = f.person_id`

func scanFaces(rows *sql.Rows) ([]Face, error) {
	var out []Face
	for rows.Next() {
		var f Face
		var b []byte
		if err := rows.Scan(&f.ID, &f.ItemID, &f.Relpath, &f.Type, &f.Offset,
			&f.PersonID, &f.PersonName, &f.X, &f.Y, &f.W, &f.H, &f.Score, &b); err != nil {
			return nil, err
		}
		f.Vec = decodeVec(b)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetFaces replaces every face on an item and marks detection as done. Done is
// recorded even for zero faces: "we looked and there was nobody" has to be
// distinguishable from "not looked at yet", or the worker would re-send every
// landscape photo in the library on every pass, forever.
func (s *Store) SetFaces(ctx context.Context, relpath string, faces []FaceWrite) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE relpath=? AND missing_since IS NULL`, relpath).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return nil // vanished mid-pass; nothing to record
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM faces WHERE item_id=?`, id); err != nil {
		return err
	}
	for _, f := range faces {
		if len(f.Vec) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO faces(item_id,person_id,x,y,w,h,score,dim,vec) VALUES(?,NULL,?,?,?,?,?,?,?)`,
			id, f.X, f.Y, f.W, f.H, f.Score, len(f.Vec), encodeVec(f.Vec)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET faces_done=1 WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ItemsNeedingFaces returns present visual items that haven't been through the
// detector yet, newest first (so newly-added media is recognised first).
func (s *Store) ItemsNeedingFaces(ctx context.Context, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items
		 WHERE missing_since IS NULL AND width > 0 AND faces_done = 0
		 ORDER BY indexed_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// AllFaces returns every face on a present item, oldest id first. Ordering is
// load-bearing: clustering walks this list in order, so a stable order means a
// stable grouping (and therefore stable cluster ids) between requests.
func (s *Store) AllFaces(ctx context.Context) ([]Face, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+faceCols+` `+faceFrom+`
		 WHERE i.missing_since IS NULL ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFaces(rows)
}

// FaceByID fetches one face (for the crop endpoint).
func (s *Store) FaceByID(ctx context.Context, id int64) (Face, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+faceCols+` `+faceFrom+` WHERE f.id = ?`, id)
	if err != nil {
		return Face{}, err
	}
	defer rows.Close()
	faces, err := scanFaces(rows)
	if err != nil {
		return Face{}, err
	}
	if len(faces) == 0 {
		return Face{}, sql.ErrNoRows
	}
	return faces[0], nil
}

// FacesForItem returns the faces found in one item, largest first (the biggest
// face in a frame is usually its subject).
func (s *Store) FacesForItem(ctx context.Context, itemID int64) ([]Face, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+faceCols+` `+faceFrom+` WHERE f.item_id = ? ORDER BY f.w*f.h DESC, f.id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFaces(rows)
}

// FaceCounts reports detection progress for the status line: how many present
// items are visual, how many have been through the detector, how many faces were
// found, and how many people have been named.
func (s *Store) FaceCounts(ctx context.Context) (visual, done, faces, people int, err error) {
	if err = s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN width>0 THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN width>0 AND faces_done=1 THEN 1 ELSE 0 END),0)
		FROM items WHERE missing_since IS NULL`).Scan(&visual, &done); err != nil {
		return
	}
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM faces f JOIN items i ON i.id=f.item_id
		 WHERE i.missing_since IS NULL`).Scan(&faces); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM people`).Scan(&people)
	return
}

// ResetFaces clears every detection so the worker starts over (after changing
// the model, say). People rows go too, but people.json does not — so the names
// come back, re-attached by similarity, as detection catches up.
func (s *Store) ResetFaces(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM faces`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM people`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET faces_done=0`); err != nil {
		return err
	}
	// The face ids these are keyed on are gone, and re-detection mints new ones —
	// so a widening cannot survive this even in principle.
	if _, err := tx.ExecContext(ctx, `DELETE FROM face_group_match`); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- widened groups ----

// FaceGroupMatches returns the per-group thresholds still in force. Rows whose
// face has since been named or deleted are left out: naming is the durable form
// of the same judgement (it goes to people.json), so once a widened group has a
// name the override has nothing left to say.
func (s *Store) FaceGroupMatches(ctx context.Context) (Widened, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.face_id, g.match FROM face_group_match g
		JOIN faces f ON f.id = g.face_id
		JOIN items i ON i.id = f.item_id
		WHERE f.person_id IS NULL AND i.missing_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Widened{}
	for rows.Next() {
		var id int64
		var m float64
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// WidenFaceGroup records the lowered threshold for the group keyed on faceID.
func (s *Store) WidenFaceGroup(ctx context.Context, faceID int64, match float64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO face_group_match(face_id,match) VALUES(?,?)
		 ON CONFLICT(face_id) DO UPDATE SET match=excluded.match`, faceID, match)
	return err
}

// ResetFaceGroup drops the widening on a set of faces, putting them back under
// the library-wide threshold. It takes the whole group rather than its key
// because merging moves the key: the face the user widened may no longer be it.
func (s *Store) ResetFaceGroup(ctx context.Context, faceIDs []int64) error {
	if len(faceIDs) == 0 {
		return nil
	}
	ph := make([]string, len(faceIDs))
	args := make([]any, len(faceIDs))
	for i, id := range faceIDs {
		ph[i], args[i] = "?", id
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM face_group_match WHERE face_id IN (`+strings.Join(ph, ",")+`)`, args...)
	return err
}

// ---- people ----

// EnsurePerson returns the id of the person with this name, creating the row if
// needed. Names are compared case-insensitively (the column is COLLATE NOCASE),
// so "jane" never becomes a second Jane.
func (s *Store) EnsurePerson(ctx context.Context, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("empty person name")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO people(name) VALUES(?)`, name); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM people WHERE name=?`, name).Scan(&id)
	return id, err
}

// PersonByID returns one person's name.
func (s *Store) PersonByID(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM people WHERE id=?`, id).Scan(&name)
	return name, err
}

// RenamePerson changes a name in place, keeping every face assignment.
func (s *Store) RenamePerson(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty person name")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE people SET name=? WHERE id=?`, name, id)
	return err
}

// DeletePerson forgets a name. Its faces survive as unnamed detections (the
// clustering will offer them again), because the faces are evidence and only the
// name was a judgement.
func (s *Store) DeletePerson(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE faces SET person_id=NULL WHERE person_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM people WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AssignFaces labels faces as one person (personID 0 clears the label).
func (s *Store) AssignFaces(ctx context.Context, personID int64, faceIDs []int64) error {
	if len(faceIDs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE faces SET person_id=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	var pid any
	if personID > 0 {
		pid = personID
	}
	for _, id := range faceIDs {
		if _, err := stmt.ExecContext(ctx, pid, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListPeople returns every named person with their face/item counts and a cover
// face (the highest-scoring one), most-photographed first.
func (s *Store) ListPeople(ctx context.Context) ([]Person, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name,
		       COUNT(f.id),
		       COUNT(DISTINCT f.item_id),
		       COALESCE((SELECT f2.id FROM faces f2
		                 JOIN items i2 ON i2.id = f2.item_id
		                 WHERE f2.person_id = p.id AND i2.missing_since IS NULL
		                 ORDER BY COALESCE(f2.score,0) DESC, f2.w*f2.h DESC, f2.id LIMIT 1), 0)
		FROM people p
		LEFT JOIN faces f ON f.person_id = p.id
		LEFT JOIN items i ON i.id = f.item_id AND i.missing_since IS NULL
		GROUP BY p.id
		ORDER BY COUNT(DISTINCT f.item_id) DESC, p.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Faces, &p.Items, &p.CoverFaceID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ItemsForPerson returns the items a named person appears in, newest first.
func (s *Store) ItemsForPerson(ctx context.Context, personID int64, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixCols("i")+` FROM items i
		 JOIN faces f ON f.item_id = i.id
		 WHERE f.person_id = ? AND i.missing_since IS NULL
		 GROUP BY i.id
		 ORDER BY i.id DESC LIMIT ?`, personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ItemsForFaces returns the items a given set of faces belongs to, newest first.
// Used for an unnamed cluster, which has no id to join on.
func (s *Store) ItemsForFaces(ctx context.Context, faceIDs []int64, limit int) ([]model.Item, error) {
	if len(faceIDs) == 0 {
		return nil, nil
	}
	ph := make([]string, len(faceIDs))
	args := make([]any, 0, len(faceIDs)+1)
	for i, id := range faceIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixCols("i")+` FROM items i
		 JOIN faces f ON f.item_id = i.id
		 WHERE f.id IN (`+strings.Join(ph, ",")+`) AND i.missing_since IS NULL
		 GROUP BY i.id
		 ORDER BY i.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ---- clustering ----

// ClusterUnnamed groups the faces nobody has named yet, biggest group first.
// Groups of one are included (a person photographed once is still a person), but
// they sort last, so the page leads with the faces worth naming.
//
// widened may be nil; see Widened for what a non-empty one does.
func ClusterUnnamed(faces []Face, threshold float64, widened Widened) []FaceCluster {
	var pool []Face
	for _, f := range faces {
		if f.PersonID == 0 && len(f.Vec) > 0 {
			pool = append(pool, f)
		}
	}
	groups := widen(group(pool, threshold), threshold, widened)

	out := make([]FaceCluster, 0, len(groups))
	for _, g := range groups {
		c := FaceCluster{KeyFaceID: g[0].ID, CoverFaceID: g[0].ID, Match: threshold, FaceIDs: make([]int64, 0, len(g))}
		items := make(map[int64]struct{}, len(g))
		bestScore, bestArea := -1.0, -1.0
		for _, f := range g {
			c.FaceIDs = append(c.FaceIDs, f.ID)
			items[f.ItemID] = struct{}{}
			if t, ok := widened[f.ID]; ok && t < c.Match {
				c.Match = t
			}
			// Highest confidence, breaking ties on size — a detector that reports
			// no confidence (they exist; see facerec/) would otherwise always make
			// the first face the cover, however small.
			area := f.W * f.H
			if f.Score > bestScore || (f.Score == bestScore && area > bestArea) {
				bestScore, bestArea = f.Score, area
				c.CoverFaceID = f.ID
			}
		}
		c.Faces = len(g)
		c.Items = len(items)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Faces != out[j].Faces {
			return out[i].Faces > out[j].Faces
		}
		return out[i].KeyFaceID < out[j].KeyFaceID
	})
	return out
}

// ClusterContaining returns the group the given face belongs to — the faces that
// get labelled when you name that cluster. Empty if the face is already named or
// no longer exists.
func ClusterContaining(faces []Face, threshold float64, widened Widened, faceID int64) []int64 {
	if c, ok := ClusterOf(ClusterUnnamed(faces, threshold, widened), faceID); ok {
		return c.FaceIDs
	}
	return nil
}

// ClusterOf finds the group containing a face in an already-computed listing —
// for callers that need the group's counts and threshold too, or that are about
// to look at the other groups as well and shouldn't cluster twice to do it.
func ClusterOf(clusters []FaceCluster, faceID int64) (FaceCluster, bool) {
	for _, c := range clusters {
		for _, id := range c.FaceIDs {
			if id == faceID {
				return c, true
			}
		}
	}
	return FaceCluster{}, false
}

// group is greedy single-pass agglomeration: walk the faces in id order and put
// each one in the first group whose centroid it is close enough to, otherwise
// start a group. It is O(faces x groups), deterministic, and needs no target
// number of clusters — which matters, because how many people are in a library
// is exactly what we're trying to find out.
//
// The trade-off versus a proper hierarchical clustering is that it can split one
// person across two groups when their photos vary a lot (different lighting, ten
// years apart). That direction of error is the safe one: naming both groups with
// the same name merges them, whereas a wrong merge would need per-face surgery
// the UI doesn't offer.
func group(faces []Face, threshold float64) [][]Face {
	type cl struct {
		members  []Face
		sum      []float64 // running total; centroid = normalized sum
		centroid []float32
	}
	var cls []*cl

	for _, f := range faces {
		bestIdx, bestSim := -1, threshold
		for i, c := range cls {
			if len(c.centroid) != len(f.Vec) {
				continue // a model change mid-library; incomparable vectors
			}
			if sim := media.Cosine(c.centroid, f.Vec); sim >= bestSim {
				bestIdx, bestSim = i, sim
			}
		}
		if bestIdx < 0 {
			sum := make([]float64, len(f.Vec))
			for i, x := range f.Vec {
				sum[i] = float64(x)
			}
			cls = append(cls, &cl{members: []Face{f}, sum: sum, centroid: unit(sum)})
			continue
		}
		c := cls[bestIdx]
		c.members = append(c.members, f)
		for i, x := range f.Vec {
			c.sum[i] += float64(x)
		}
		c.centroid = unit(c.sum)
	}

	out := make([][]Face, 0, len(cls))
	for _, c := range cls {
		out = append(out, c.members)
	}
	return out
}

// widen re-joins whole groups that a lowered threshold says are the same person.
// It is the repair for the split described on group: instead of re-running the
// library at a looser number, only the groups the user pointed at go looking for
// a partner, and they compare against finished groups rather than loose faces.
//
// After the greedy pass rather than inside it, because a group's key face is not
// necessarily the first one the pass sees. Lowering the threshold as faces
// arrive would only ever pull in higher face ids, so widening the second of two
// split groups would do nothing at all — and which half a user clicks is not
// something they should have to reason about. Comparing settled groups is
// symmetric: either half pulls the other in.
//
// Every comparison is against the groups as clustering left them, and each group
// is judged against the widened one only. Both matter: a widened group that took
// its new members into account before looking again would drift towards the
// average of everything it had swallowed, and the average of two people looks a
// little like a third — on a library of hundreds of groups, one click ran that
// feedback loop to its conclusion and returned a single group.
func widen(groups [][]Face, threshold float64, widened Widened) [][]Face {
	if len(widened) == 0 {
		return groups
	}
	sums := make([][]float64, len(groups))
	thr := make([]float64, len(groups))
	any := false
	for i, g := range groups {
		// group only ever puts same-dimension vectors together, so one length
		// covers the whole group.
		s := make([]float64, len(g[0].Vec))
		thr[i] = threshold
		for _, f := range g {
			for k, x := range f.Vec {
				s[k] += float64(x)
			}
			if t, ok := widened[f.ID]; ok && t < thr[i] {
				thr[i] = t
			}
		}
		sums[i] = s
		any = any || thr[i] < threshold
	}
	// Every override named a face that has since been labelled or deleted.
	if !any {
		return groups
	}

	// Which group each one ends up in, by index. Merging is recorded here rather
	// than done in place, so that what a later comparison sees is unaffected by
	// an earlier one.
	joined := make([]int, len(groups))
	for i := range joined {
		joined[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for joined[i] != i {
			joined[i] = joined[joined[i]]
			i = joined[i]
		}
		return i
	}
	for i := range groups {
		if thr[i] >= threshold {
			continue // not widened: only the marked groups go looking
		}
		for j := range groups {
			if i == j || len(sums[j]) != len(sums[i]) {
				continue
			}
			t := math.Min(thr[i], thr[j])
			if meanSim(sums[i], sums[j], len(groups[i]), len(groups[j])) < t {
				continue
			}
			if a, b := root(i), root(j); a != b {
				// Lower index wins, so the merged group keeps the earliest
				// members and the whole pass stays order-independent.
				if a < b {
					joined[b] = a
				} else {
					joined[a] = b
				}
			}
		}
	}

	out := make([][]Face, 0, len(groups))
	at := make(map[int]int, len(groups)) // group index -> where it landed in out
	for i, g := range groups {
		r := root(i)
		if r == i {
			at[i] = len(out)
			// Clipped, so absorbing into it copies rather than writing over
			// whatever spare capacity the grouping pass left on the end.
			out = append(out, g[:len(g):len(g)])
			continue
		}
		out[at[r]] = append(out[at[r]], g...)
	}
	for _, g := range out {
		// Back into id order, so the group is still identified by its lowest
		// face id however the merging got there.
		sort.Slice(g, func(i, j int) bool { return g[i].ID < g[j].ID })
	}
	return out
}

// meanSim is the average cosine similarity over every pair of faces with one
// face from each group — which falls out of the sums the groups are already
// summarized by, since the mean of a·b over the pairs is (Σa)·(Σb)/(na·nb).
//
// The obvious alternative is the cosine of the two normalized centroids, and it
// is the wrong measure to compare against a threshold that was chosen for faces:
// normalizing divides out the shrinkage that averaging causes, and how much a
// sum shrinks depends on how varied the group is and how many faces are in it.
// So the bigger a group got, the more similar its centroid read to everything —
// widening a large group by as little as 0.001 pulled in dozens of strangers.
func meanSim(a, b []float64, na, nb int) float64 {
	if len(a) != len(b) || na == 0 || nb == 0 {
		return -1
	}
	var d float64
	for i := range a {
		d += a[i] * b[i]
	}
	return d / float64(na*nb)
}

// MatchPerson returns the id of the person whose example faces best match vec,
// or 0 if none reach the threshold. protos maps person id -> that person's
// example embeddings.
//
// Comparison is against individual examples rather than an average of them: a
// person's photos legitimately form several tight groups (with and without
// glasses, teenager and adult), and averaging those together produces a centroid
// that is close to none of them.
func MatchPerson(protos map[int64][][]float32, vec []float32, threshold float64) int64 {
	bestID, bestSim := int64(0), threshold
	for id, list := range protos {
		for _, p := range list {
			if len(p) != len(vec) {
				continue
			}
			if sim := media.Cosine(p, vec); sim >= bestSim {
				bestID, bestSim = id, sim
			}
		}
	}
	return bestID
}

// unit returns the L2-normalized float32 form of v.
func unit(v []float64) []float32 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	out := make([]float32, len(v))
	if s <= 0 {
		return out
	}
	s = math.Sqrt(s)
	for i, x := range v {
		out[i] = float32(x / s)
	}
	return out
}
