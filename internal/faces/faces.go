// Package faces is the face-recognition side of the app: a slow background
// worker that sends frames to the (optional, separately-hosted) recognition
// service, plus the bookkeeping that turns detections into named people.
//
// It is entirely optional — nothing runs unless FACE_URL is set — and it follows
// the same shape as internal/describe: a small number of items in flight (see
// FACE_WORKERS) because inference is the bottleneck, retried on a ticker so an
// unreachable service is just slow progress rather than an error state.
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
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/face"
	"mediavault/internal/model"
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

	// refSeconds is how far a people.json timestamp may be from a detected one
	// and still be the same sighting. Tight, because both are computed from the
	// same duration by the same code: this is float noise, not a search window.
	refSeconds = 0.5
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
	failed   map[string]struct{} // items we couldn't get a detection for; skip for this run
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

// Skipped is how many items this run has given up on: no frame came out of
// them, or the detector wouldn't take the one that did. They keep their place in
// the queue and are tried again after a restart; the People page reports the
// number because otherwise a detection count that stops short of the total looks
// like a worker that has stopped, which is the one thing it doesn't mean.
func (r *Recognizer) Skipped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failed)
}

// skip marks an item as not worth trying again this run.
func (r *Recognizer) skip(relpath string) {
	r.mu.Lock()
	r.failed[relpath] = struct{}{}
	r.mu.Unlock()
}

// skipping reports whether an item has already been given up on.
func (r *Recognizer) skipping(relpath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.failed[relpath]
	return ok
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
//
// skip is what keeps the pass moving. An item detection can't do anything with
// keeps faces_done = 0 and so stays in the queue, at the same place, forever;
// counting those and asking for the next batch past them is the difference
// between "a few files in this library are unreadable" and a worker that stops
// dead partway through and never says why.
func (r *Recognizer) pass(ctx context.Context) {
	skip := 0
	for {
		examined, done := r.detectBatch(ctx, skip)
		skip += examined
		if r.needsSync() {
			if err := r.Sync(ctx); err != nil {
				log.Printf("faces: sync: %v", err)
			}
		}
		if done || ctx.Err() != nil {
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

// detectBatch sends one batch of not-yet-examined items to the service, starting
// skip items into the queue.
//
// It returns how many of them are still going to be in that queue afterwards —
// the ones to step over next time — and whether the pass should stop, which means
// the queue ran out or the service failed in a way that asks to be left alone for
// a while.
//
// Extraction and inference run concurrently (FACE_WORKERS of them), because they
// are not the same bottleneck: pulling a frame out of a video over a network
// mount is seconds of waiting on ffmpeg and the NAS, during which the detector —
// the expensive half — has nothing to do. The writes stay on this goroutine, in
// queue order, so the store still sees one writer.
func (r *Recognizer) detectBatch(ctx context.Context, skip int) (examined int, done bool) {
	if ctx.Err() != nil {
		return 0, true
	}
	items, err := r.store.ItemsNeedingFaces(ctx, batch, skip)
	if err != nil {
		log.Printf("faces: list: %v", err)
		return 0, true
	}
	todo := make([]model.Item, 0, len(items))
	for _, it := range items {
		if r.skipping(it.Relpath) {
			examined++ // still in the queue, still not going to work; step over it
			continue
		}
		todo = append(todo, it)
	}
	if len(items) < batch {
		done = true // the queue ends here
	}
	if len(todo) == 0 {
		return examined, done
	}

	results := r.detectItems(ctx, todo)
	progressed := false
	defer func() {
		if progressed {
			r.markDirty()
		}
	}()
	unwell := 0 // transient failures this batch; they all have the same cause
	for _, res := range results {
		if !res.ran {
			return examined, true // shutting down
		}
		switch {
		case res.err != nil && res.frame:
			// No frame to send: an unreadable file, a codec ffmpeg won't decode, a
			// mount that went away. Nothing about that changes this run.
			r.skip(res.item.Relpath)
			examined++
		case res.err != nil && face.PermanentError(res.err):
			// A 4xx is this frame being rejected rather than the service being
			// unwell. Same split as the captioning worker, for the same reason: one
			// bad input must not starve the queue behind it.
			log.Printf("faces: detect %s: %v (skipping)", res.item.Relpath, res.err)
			r.skip(res.item.Relpath)
			examined++
		case res.err != nil && face.TimedOut(res.err):
			// The service is alive but this frame took longer than FACE_TIMEOUT.
			// Retrying it means spending that time again to fail again, and doing so
			// at the head of the queue means nothing behind it is ever looked at —
			// which is what a stuck detection count looks like from outside.
			log.Printf("faces: detect %s: gave up after %s (skipping; raise FACE_TIMEOUT, "+
				"or lower FACE_FRAME_PX / FACE_UPSAMPLE if this is most frames)",
				res.item.Relpath, r.cfg.FaceTimeout)
			r.skip(res.item.Relpath)
			examined++
		case res.err != nil:
			// One line, not one per item in flight: they are all the same service
			// having the same problem, and the tail of them says nothing new.
			if unwell == 0 {
				log.Printf("faces: detect: %v", res.err)
			}
			unwell++
			examined++
			done = true // the service is unhappy; hammering it makes that worse
		default:
			if err := r.store.SetFaces(ctx, res.item.Relpath, toWrites(res.dets, r.cfg.FaceMaxPerItem)); err != nil {
				log.Printf("faces: save %s: %v", res.item.Relpath, err)
				examined++
				continue
			}
			progressed = true
		}
	}
	if unwell > 1 {
		log.Printf("faces: detect: %d more frames failed the same way; leaving the service alone for a bit", unwell-1)
	}
	return examined, done
}

// attempt is one item's trip through extraction and detection.
type attempt struct {
	item  model.Item
	dets  []found
	err   error
	frame bool // the error was extracting the frame, not detecting on it
	ran   bool // false only if we shut down before getting to this item
}

// detectItems extracts and detects a batch's worth of items in parallel,
// returning the outcomes in the order they were given. One item can be several
// frames and several detections — see detect — but it is still one unit of work
// here, because it is one row to write and one thing to skip.
func (r *Recognizer) detectItems(ctx context.Context, items []model.Item) []attempt {
	n := r.cfg.FaceWorkers
	if n < 1 {
		n = 1
	}
	if n > len(items) {
		n = len(items)
	}
	out := make([]attempt, len(items))
	queue := make(chan int, len(items))
	for i := range items {
		queue <- i
	}
	close(queue)

	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				if ctx.Err() != nil {
					return
				}
				it := items[i]
				out[i] = attempt{item: it, ran: true}
				out[i].dets, out[i].frame, out[i].err = r.detect(ctx, it)
			}
		}()
	}
	wg.Wait()
	return out
}

// toWrites keeps the highest-scoring detections, up to max. A crowd shot can
// return dozens of tiny, low-confidence faces whose embeddings are noise, and
// noise is what makes clustering merge two people.
//
// The cap is per item, and a video is one item however many frames it was
// sampled at — which it can afford to be, because by here the frames have
// already been collapsed to one detection per person (see uniquePeople), so
// twenty faces still means twenty people.
func toWrites(dets []found, max int) []store.FaceWrite {
	if max > 0 && len(dets) > max {
		sort.SliceStable(dets, func(i, j int) bool { return dets[i].det.Score > dets[j].det.Score })
		dets = dets[:max]
	}
	out := make([]store.FaceWrite, 0, len(dets))
	for _, f := range dets {
		d := f.det
		out = append(out, store.FaceWrite{X: d.X, Y: d.Y, W: d.W, H: d.H, Score: d.Score, T: f.t, Vec: d.Vec})
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
			if f, ok := resolve(byPath[ref.Path], ref); ok {
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
		refs = append(refs, sidecar.FaceRef{Path: f.Relpath, Box: [4]float64{f.X, f.Y, f.W, f.H}, T: f.T})
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
//
// A reference into a video also has to land in the right frame. Sampling is
// deterministic — the same file yields the same points — so the timestamps
// agree exactly and the tolerance is only there for what a round trip through
// JSON and a re-probed duration can do to the last decimal. A reference with no
// timestamp matches on the box alone, which is how everything named before this
// existed carries on working.
func resolve(candidates []store.Face, ref sidecar.FaceRef) (store.Face, bool) {
	best, bestIoU := store.Face{}, refIoU
	for _, f := range candidates {
		if ref.T > 0 && math.Abs(f.T-ref.T) > refSeconds {
			continue // the right box, somewhere else in the video: someone else
		}
		if v := iou(ref.Box, [4]float64{f.X, f.Y, f.W, f.H}); v >= bestIoU {
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

// dedupeRefs drops duplicate (path, timestamp, box) references, keeping the
// first of each. The timestamp is part of the identity: two people filmed
// alternately occupy the same box in the same file, and folding them together
// would cost this person the example that is actually them.
func dedupeRefs(in []sidecar.FaceRef) []sidecar.FaceRef {
	type key struct {
		p string
		t float64
		b [4]float64
	}
	seen := make(map[key]bool, len(in))
	out := make([]sidecar.FaceRef, 0, len(in))
	for _, r := range in {
		k := key{r.Path, r.T, r.Box}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}
