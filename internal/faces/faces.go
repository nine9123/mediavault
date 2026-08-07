// Package faces is the face-recognition side of the app: a slow background
// worker that sends frames to the (optional, separately-hosted) recognition
// service, plus the bookkeeping that turns detections into named people.
//
// It is entirely optional — nothing runs unless FACE_URL is set — and it follows
// the same shape as internal/describe: one item at a time, because inference is
// the bottleneck, retried on a ticker so an unreachable service is just slow
// progress rather than an error state.
//
// The split of responsibility is the important part:
//
//	people.json (durable)  names + the example faces that define each person
//	faces/people tables    every detection, and who each one is: DERIVED
//
// So Sync is the reconciler: it reads people.json, resolves each example back to
// a detected face, and labels everything else that looks like them. Drop the
// database and the names come back on their own.
package faces

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/face"
	"mediavault/internal/media"
	"mediavault/internal/sidecar"
	"mediavault/internal/store"
)

// DefaultMatch is the cosine-similarity threshold used when neither FACE_MATCH
// nor the service's /health says otherwise. It is a middling value on purpose:
// the right number is a property of the model, so both overrides above are
// preferable to this one.
const DefaultMatch = 0.5

const (
	// batch is how many items the worker claims per query. Small, because each
	// one is an ffmpeg run plus an inference call.
	batch = 20

	// protoCap bounds how many example faces are kept per person — in
	// people.json and when matching. Enough to cover a person across years and
	// lighting; small enough that the file stays readable and the match loop
	// stays trivial.
	protoCap = 32

	// refIoU is how much a people.json box must overlap a detected box to be
	// considered the same face. Boxes come from the same detector on the same
	// frame, so they normally agree almost exactly; the tolerance is for a model
	// upgrade that frames faces slightly differently.
	refIoU = 0.5
)

// Recognizer owns face detection and the people/face bookkeeping. The HTTP layer
// shares one with the background worker so both use the same similarity
// threshold — clustering with a different number than the labelling used would
// show groups that don't match what naming them does.
type Recognizer struct {
	cfg   config.Config
	store *store.Store

	// opMu serializes everything that touches people.json. Each of those
	// operations is a read-modify-write of one small file, so two of them at once
	// (two browser tabs naming groups, or a name landing while the worker
	// reconciles) would have the second overwrite the first's entry.
	opMu sync.Mutex

	mu       sync.Mutex
	match    float64             // resolved threshold; 0 until asked
	failed   map[string]struct{} // frames we couldn't extract; skip for this run
	peopleAt time.Time           // people.json mtime at the last sync
	syncedAt time.Time           // when the last sync ran (rate-limits reconciling)
	dirty    bool                // detections changed since the last sync
}

// New creates a Recognizer.
func New(cfg config.Config, st *store.Store) *Recognizer {
	return &Recognizer{cfg: cfg, store: st, failed: map[string]struct{}{}, dirty: true}
}

// Enabled reports whether a recognition service is configured.
func (r *Recognizer) Enabled() bool { return r.cfg.FacesEnabled() }

// Match is the similarity threshold in force: the configured value, else what
// the service advertises, else DefaultMatch.
func (r *Recognizer) Match() float64 {
	if r.cfg.FaceMatch > 0 {
		return r.cfg.FaceMatch
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.match > 0 {
		return r.match
	}
	return DefaultMatch
}

// learnMatch records the threshold the service recommends for its model.
func (r *Recognizer) learnMatch(v float64) {
	if v <= 0 || v >= 1 {
		return
	}
	r.mu.Lock()
	r.match = v
	r.mu.Unlock()
}

// Run detects faces on undetected items, reconciles people.json, then re-checks
// periodically — which doubles as backoff when the service is unreachable.
func (r *Recognizer) Run(ctx context.Context) {
	log.Printf("faces: on — recognition service at %s", r.cfg.FaceURL)
	if info, err := face.Health(ctx, r.cfg); err != nil {
		log.Printf("faces: NOT reachable at %s: %v — the People page stays empty until this resolves", r.cfg.FaceURL, err)
	} else {
		r.learnMatch(info.Match)
		log.Printf("faces: service reachable — model %q, %d-dim embeddings, matching at cosine >= %.2f",
			info.Model, info.Dim, r.Match())
	}

	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	r.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.pass(ctx)
		}
	}
}

// pass alternates detection with reconciliation until the queue runs dry.
//
// Interleaved rather than detect-everything-then-reconcile: the first run over an
// existing library takes hours, and a user who already has names would otherwise
// watch faces pile up with nobody's name on them until it finished. needsSync
// rate-limits the reconcile, so this costs a full-face scan a minute, not one per
// batch.
func (r *Recognizer) pass(ctx context.Context) {
	for {
		progressed := r.detectBatch(ctx)
		if r.needsSync() {
			if err := r.Sync(ctx); err != nil {
				log.Printf("faces: sync: %v", err)
			}
		}
		if !progressed || ctx.Err() != nil {
			return
		}
	}
}

// syncGap is the shortest interval between reconciles while detection is running.
// Sync reads every face in the library, so doing it per batch would eventually
// cost more than the detection it is keeping up with.
const syncGap = time.Minute

// needsSync reports whether detections changed or people.json was edited since
// the last reconcile — and, if detections changed, whether enough time has passed
// to be worth another full pass.
func (r *Recognizer) needsSync() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirty && time.Since(r.syncedAt) >= syncGap {
		return true
	}
	// An edit to people.json is a person asking for something, so it is not rate
	// limited: it is one file stat per tick to find out.
	fi, err := os.Stat(r.cfg.PeoplePath())
	if err != nil {
		return false
	}
	return fi.ModTime().After(r.peopleAt)
}

// detectBatch sends one batch of not-yet-examined items to the service. It reports
// whether anything was stored, which is also the "keep going" signal: false means
// the queue is empty, everything in it was skipped, or the service failed in a way
// that asks to be left alone for a while.
func (r *Recognizer) detectBatch(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	items, err := r.store.ItemsNeedingFaces(ctx, batch)
	if err != nil {
		log.Printf("faces: list: %v", err)
		return false
	}
	progressed := false
	defer func() {
		if progressed {
			r.markDirty()
		}
	}()
	for _, it := range items {
		if ctx.Err() != nil {
			return progressed
		}
		if _, skip := r.failed[it.Relpath]; skip {
			continue
		}
		abs := filepath.Join(r.cfg.MediaRoot, filepath.FromSlash(it.Relpath))
		jpeg, err := media.FrameJPEG(ctx, r.cfg, abs, it.Type, it.Offset, r.cfg.FaceFramePx)
		if err != nil {
			r.failed[it.Relpath] = struct{}{} // no frame; skip this run
			continue
		}
		dets, err := face.Detect(ctx, r.cfg, jpeg)
		if err != nil {
			// A 4xx is this frame being rejected; anything else is the service being
			// unhappy, and hammering it makes that worse. Same split as the
			// captioning worker, for the same reason: one bad input must not starve
			// the queue behind it.
			if face.PermanentError(err) {
				log.Printf("faces: detect %s: %v (skipping)", it.Relpath, err)
				r.failed[it.Relpath] = struct{}{}
				continue
			}
			log.Printf("faces: detect: %v", err)
			return progressed
		}
		if err := r.store.SetFaces(ctx, it.Relpath, toWrites(dets, r.cfg.FaceMaxPerItem)); err != nil {
			log.Printf("faces: save %s: %v", it.Relpath, err)
			continue
		}
		progressed = true
	}
	// A batch of nothing but skips returns false, so the caller stops: none of
	// those rows will gain a detection this run, and re-querying them is a loop.
	return progressed
}

// toWrites keeps the highest-scoring detections, up to max. A crowd shot can
// return dozens of tiny, low-confidence faces whose embeddings are noise, and
// noise is what makes clustering merge two people.
func toWrites(dets []face.Detection, max int) []store.FaceWrite {
	if max > 0 && len(dets) > max {
		sort.SliceStable(dets, func(i, j int) bool { return dets[i].Score > dets[j].Score })
		dets = dets[:max]
	}
	out := make([]store.FaceWrite, 0, len(dets))
	for _, d := range dets {
		out = append(out, store.FaceWrite{X: d.X, Y: d.Y, W: d.W, H: d.H, Score: d.Score, Vec: d.Vec})
	}
	return out
}

func (r *Recognizer) markDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}

// Invalidate asks for a reconcile on the next pass. The HTTP layer calls it after
// clearing detections: that also drops the people rows, so they have to be
// rebuilt from people.json even before any new detection arrives.
func (r *Recognizer) Invalidate() { r.markDirty() }

// Sync reconciles the database with people.json: every named person gets a row,
// their example faces are relabelled, and any unnamed face close enough to one of
// those examples is labelled too. This is what makes a name survive dropping the
// cache, and what makes hand-editing people.json work.
func (r *Recognizer) Sync(ctx context.Context) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.sync(ctx)
}

// sync is Sync without the lock, for callers that already hold it.
func (r *Recognizer) sync(ctx context.Context) error {
	path := r.cfg.PeoplePath()
	doc, err := sidecar.ReadPeople(path)
	if err != nil {
		return err
	}
	faces, err := r.store.AllFaces(ctx)
	if err != nil {
		return err
	}

	// Index detections by relpath (so resolving an example is a map lookup rather
	// than a scan per reference) and by id, so the passes below can keep the
	// in-memory copy in step with the labels they write. Without that, a face
	// relabelled by one pass still looks unnamed to the next one.
	byPath := map[string][]store.Face{}
	pos := make(map[int64]int, len(faces))
	for i, f := range faces {
		byPath[f.Relpath] = append(byPath[f.Relpath], f)
		pos[f.ID] = i
	}

	// 1. Every named person gets a row, and their example faces get their label
	//    back. This is the step that rebuilds identities after a cache wipe.
	protos := map[int64][][]float32{}
	named := map[int64]bool{}
	for _, p := range doc.People {
		id, err := r.store.EnsurePerson(ctx, p.Name)
		if err != nil {
			log.Printf("faces: person %q: %v", p.Name, err)
			continue
		}
		named[id] = true

		var seed []int64
		for _, ref := range p.Faces {
			if f, ok := resolve(byPath[ref.Path], ref.Box); ok {
				seed = append(seed, f.ID)
				protos[id] = append(protos[id], f.Vec)
			}
		}
		// Faces already labelled as this person are evidence too — they are how
		// a person accumulates examples beyond the ones written to disk.
		for _, f := range faces {
			if f.PersonID == id && len(f.Vec) > 0 {
				protos[id] = append(protos[id], f.Vec)
			}
		}
		protos[id] = sample(protos[id], protoCap)
		if err := r.store.AssignFaces(ctx, id, seed); err != nil {
			log.Printf("faces: label %q: %v", p.Name, err)
			continue
		}
		for _, fid := range seed {
			faces[pos[fid]].PersonID = id
		}
	}

	// 2. people.json is authoritative: a person deleted from it (by us or by
	//    hand) stops existing here too, and their faces go back to unnamed —
	//    which makes them eligible for step 3 in this same pass, so a rename that
	//    merges two people doesn't leave half of them unlabelled for a tick.
	existing, err := r.store.ListPeople(ctx)
	if err != nil {
		return err
	}
	for _, p := range existing {
		if named[p.ID] {
			continue
		}
		if err := r.store.DeletePerson(ctx, p.ID); err != nil {
			log.Printf("faces: forget %q: %v", p.Name, err)
			continue
		}
		for i := range faces {
			if faces[i].PersonID == p.ID {
				faces[i].PersonID = 0
			}
		}
	}

	// 3. Spread the net: anything still unnamed that looks like a known example
	//    becomes that person.
	if len(protos) > 0 {
		match := r.Match()
		assign := map[int64][]int64{}
		for _, f := range faces {
			if f.PersonID != 0 || len(f.Vec) == 0 {
				continue
			}
			if id := store.MatchPerson(protos, f.Vec, match); id > 0 {
				assign[id] = append(assign[id], f.ID)
			}
		}
		for id, ids := range assign {
			if err := r.store.AssignFaces(ctx, id, ids); err != nil {
				log.Printf("faces: assign: %v", err)
			}
		}
	}

	r.mu.Lock()
	r.dirty = false
	r.syncedAt = time.Now()
	if fi, err := os.Stat(path); err == nil {
		r.peopleAt = fi.ModTime()
	}
	r.mu.Unlock()
	return nil
}

// Name labels a set of faces as one person and records them in people.json.
// Naming with an existing name merges into that person — which is also how you
// join two clusters the algorithm split apart.
func (r *Recognizer) Name(ctx context.Context, name string, faces []store.Face) (int64, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	id, err := r.store.EnsurePerson(ctx, name)
	if err != nil {
		return 0, err
	}
	ids := make([]int64, 0, len(faces))
	for _, f := range faces {
		ids = append(ids, f.ID)
	}
	if err := r.store.AssignFaces(ctx, id, ids); err != nil {
		return 0, err
	}
	if err := r.remember(name, faces); err != nil {
		return 0, err
	}
	r.markDirty()
	// Synchronous, so the caller's next request already sees the merged result;
	// the alternative is a People page that changes under the user a tick later.
	if err := r.sync(ctx); err != nil {
		log.Printf("faces: sync after naming: %v", err)
	}
	return id, nil
}

// Rename changes a person's name in both places. The new name may already exist,
// in which case this is a merge.
func (r *Recognizer) Rename(ctx context.Context, id int64, name string) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	old, err := r.store.PersonByID(ctx, id)
	if err != nil {
		return err
	}
	path := r.cfg.PeoplePath()
	doc, err := sidecar.ReadPeople(path)
	if err != nil {
		return err
	}
	from := doc.Find(old)
	if from < 0 {
		return nil // not on disk (hand-edited away); the next Sync settles it
	}
	if to := doc.Find(name); to >= 0 && to != from {
		// A merge keeps both sets of examples, at twice the usual cap. Every
		// example that survives here is a face Sync relabels directly; the ones
		// dropped rely on looking like a survivor, which is exactly the assumption
		// a merge says is unreliable (why else were they two groups?).
		doc.People[to].Faces = sample(dedupeRefs(append(doc.People[to].Faces, doc.People[from].Faces...)), protoCap*2)
		doc.People = append(doc.People[:from], doc.People[from+1:]...)
		if err := sidecar.WritePeople(path, doc); err != nil {
			return err
		}
	} else {
		doc.People[from].Name = name
		if err := sidecar.WritePeople(path, doc); err != nil {
			return err
		}
		// Not a merge, so keep the row — and with it the person's id, which the
		// URL of the page doing the renaming refers to, and every face assignment.
		// Letting Sync recreate the row instead would unassign and re-match every
		// one of their faces to achieve exactly the same result.
		if err := r.store.RenamePerson(ctx, id, name); err != nil {
			// people.json is the authority and already says the new name, so let
			// Sync reconcile rather than failing the request.
			log.Printf("faces: rename row %d: %v", id, err)
		}
	}
	r.markDirty()
	return r.sync(ctx) // creates/merges rows and relabels
}

// Forget removes a person entirely. Their faces stay as unnamed detections, so
// the People page will offer them again — the detections were evidence, only the
// name was a judgement.
func (r *Recognizer) Forget(ctx context.Context, id int64) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	name, err := r.store.PersonByID(ctx, id)
	if err != nil {
		return err
	}
	path := r.cfg.PeoplePath()
	doc, err := sidecar.ReadPeople(path)
	if err != nil {
		return err
	}
	if i := doc.Find(name); i >= 0 {
		doc.People = append(doc.People[:i], doc.People[i+1:]...)
		if err := sidecar.WritePeople(path, doc); err != nil {
			return err
		}
	}
	if err := r.store.DeletePerson(ctx, id); err != nil {
		return err
	}
	r.markDirty()
	return nil
}

// remember adds example faces for a name to people.json, merging with whatever
// is already recorded there.
func (r *Recognizer) remember(name string, faces []store.Face) error {
	path := r.cfg.PeoplePath()
	doc, err := sidecar.ReadPeople(path)
	if err != nil {
		return err
	}
	refs := make([]sidecar.FaceRef, 0, len(faces))
	for _, f := range faces {
		refs = append(refs, sidecar.FaceRef{Path: f.Relpath, Box: [4]float64{f.X, f.Y, f.W, f.H}})
	}
	if i := doc.Find(name); i >= 0 {
		doc.People[i].Faces = sample(dedupeRefs(append(doc.People[i].Faces, refs...)), protoCap)
	} else {
		doc.People = append(doc.People, sidecar.Person{Name: name, Faces: sample(dedupeRefs(refs), protoCap)})
	}
	return sidecar.WritePeople(path, doc)
}

// resolve finds the detected face matching a people.json reference. Overlap
// rather than equality: the numbers were written by a detector and will be
// re-produced by a detector, but not necessarily bit-for-bit.
func resolve(candidates []store.Face, box [4]float64) (store.Face, bool) {
	best, bestIoU := store.Face{}, refIoU
	for _, f := range candidates {
		if v := iou(box, [4]float64{f.X, f.Y, f.W, f.H}); v >= bestIoU {
			best, bestIoU = f, v
		}
	}
	return best, best.ID != 0
}

// iou is intersection-over-union of two [x,y,w,h] boxes.
func iou(a, b [4]float64) float64 {
	ax2, ay2 := a[0]+a[2], a[1]+a[3]
	bx2, by2 := b[0]+b[2], b[1]+b[3]
	ix := min(ax2, bx2) - max(a[0], b[0])
	iy := min(ay2, by2) - max(a[1], b[1])
	if ix <= 0 || iy <= 0 {
		return 0
	}
	inter := ix * iy
	union := a[2]*a[3] + b[2]*b[3] - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// sample thins a list to at most n entries by taking an even stride through it,
// so the survivors span the whole list instead of being its first n. For face
// examples that matters: the variety across a person's photos is the point.
func sample[T any](in []T, n int) []T {
	if n <= 0 || len(in) <= n {
		return in
	}
	out := make([]T, 0, n)
	step := float64(len(in)) / float64(n)
	for i := 0; i < n; i++ {
		out = append(out, in[int(float64(i)*step)])
	}
	return out
}

// dedupeRefs drops duplicate (path, box) references, keeping the first of each.
func dedupeRefs(in []sidecar.FaceRef) []sidecar.FaceRef {
	type key struct {
		p string
		b [4]float64
	}
	seen := make(map[key]bool, len(in))
	out := make([]sidecar.FaceRef, 0, len(in))
	for _, r := range in {
		k := key{r.Path, r.Box}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}
