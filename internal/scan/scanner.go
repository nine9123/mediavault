// Package scan owns the indexing pipeline. Its shape is deliberate:
//
//	walk (1 goroutine)                 -- stat-only; cheap change-detection
//	   -> jobs chan
//	      -> workers (N goroutines)    -- read file + ffmpeg; NEVER touch the DB
//	         -> results chan
//	            -> writer (1 goroutine)-- the only DB writer; batches into txns
//
// A single writer means we never fight SQLITE_BUSY, and batching makes writes
// cheap. Workers are bounded (config.Workers) because reads go over SMB and the
// network link — not the CPU — is the limit.
package scan

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/media"
	"mediavault/internal/model"
	"mediavault/internal/sidecar"
	"mediavault/internal/store"
)

// Scanner walks the media root and keeps the cache in sync.
type Scanner struct {
	cfg   config.Config
	store *store.Store

	mu      sync.Mutex // guards running
	running bool

	// thumbRetried remembers thumbless files already re-attempted this process
	// run, so files that simply can't be thumbnailed (e.g. RAW) aren't re-probed
	// on every periodic scan. Scans are single-flight, so the walk goroutine is
	// the only accessor at a time.
	thumbRetried map[string]struct{}
}

// New creates a Scanner.
func New(cfg config.Config, st *store.Store) *Scanner {
	return &Scanner{cfg: cfg, store: st, thumbRetried: make(map[string]struct{})}
}

// job is one file that needs (re)processing.
type job struct {
	abs     string
	relpath string
	info    os.FileInfo
	typ     model.MediaType
	updated bool // true if it already existed (changed), false if brand-new
}

// result is what a worker produces for the writer.
type result struct {
	relpath  string
	updated  bool
	write    store.ItemWrite
	err      error
	thumbErr string // non-empty if a thumbnail was attempted and failed
	prevErr  string // non-empty if an animated preview was attempted and failed
	partial  bool   // probe failed in a way that means "truncated/incomplete file"
}

// Scan runs one full pass. Overlapping calls are skipped (the periodic ticker
// and the manual "Rescan" button share this guard), so it's always safe to
// call. It returns stats for the run.
func (s *Scanner) Scan(ctx context.Context) (model.ScanStats, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return model.ScanStats{}, nil
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	stats := model.ScanStats{Started: time.Now()}
	var thumbFails int
	var thumbSample string
	var prevFails int
	var prevSample string
	var partial int
	var partialSample string
	var unsettled int

	var unsettledMu sync.Mutex

	// 1. Snapshot the present index up front so change-detection is pure map
	//    lookups — no per-file DB round-trips during the walk.
	index, err := s.store.LoadIndex(ctx)
	if err != nil {
		return stats, err
	}

	seen := make(map[string]struct{}, len(index))
	var seenMu sync.Mutex

	// Image relpaths grouped by directory, collected on the same walk for the
	// chain rebuild in step 6 — a second walk would list the whole tree over the
	// network again just to find the same files.
	chainDirs := map[string][]string{}

	jobs := make(chan job, 256)
	results := make(chan result, 256)

	// 2. Worker pool. Workers only read files + run ffmpeg; they never write.
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case results <- s.process(ctx, j):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }() // close results once workers finish

	// 3. Single writer: persist results in small, frequent transactions so
	//    freshly-indexed items show up in the UI within a second or two (and so
	//    a mid-scan stop loses at most the current partial batch). Writes use a
	//    background context on purpose: cancellation stops the *producers* (the
	//    walk + workers) below, and the writer simply persists whatever they
	//    already produced rather than tearing a half-written batch on shutdown.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		const maxBatch = 32
		const maxWait = 1500 * time.Millisecond
		writeCtx := context.Background()
		batch := make([]store.ItemWrite, 0, maxBatch)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := s.store.WriteBatch(writeCtx, batch); err != nil {
				log.Printf("scan: write batch: %v", err)
				stats.Errors++
			}
			batch = batch[:0]
		}
		ticker := time.NewTicker(maxWait)
		defer ticker.Stop()
		for {
			select {
			case r, ok := <-results:
				if !ok {
					flush()
					return
				}
				if r.err != nil {
					log.Printf("scan: %s: %v", r.relpath, r.err)
					stats.Errors++
					continue
				}
				if r.updated {
					stats.Updated++
				} else {
					stats.Added++
				}
				if r.partial {
					partial++
					if partialSample == "" {
						partialSample = r.relpath
					}
				}
				if r.prevErr != "" {
					prevFails++
					if prevSample == "" {
						prevSample = r.relpath + " (" + r.prevErr + ")"
					}
				}
				if r.thumbErr != "" {
					thumbFails++
					if thumbSample == "" {
						thumbSample = r.relpath + " (" + r.thumbErr + ")"
					}
				}
				batch = append(batch, r.write)
				if len(batch) >= maxBatch {
					flush()
				}
			case <-ticker.C:
				flush() // time-based flush so partial batches still appear
			}
		}
	}()

	// 4. Walk the tree (single-threaded: it's I/O-bound and parallelising the
	//    traversal buys little). Dispatch only new/changed files as jobs.
	walkErr := filepath.WalkDir(s.cfg.MediaRoot, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			log.Printf("scan: walk %s: %v", abs, err)
			return nil // skip unreadable entries, keep going
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") && abs != s.cfg.MediaRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || sidecar.IsSidecar(name) {
			return nil
		}
		typ, ok := s.cfg.MediaTypeFor(name)
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(s.cfg.MediaRoot, abs)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Chains look at the files present, not at what gets indexed, so this is
		// recorded ahead of the size and settle checks below.
		if typ != model.MediaVideo && typ != model.MediaAudio { // chains are image stories
			if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." { // don't chain loose files at the root
				chainDirs[dir] = append(chainDirs[dir], rel)
			}
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() == 0 {
			return nil // empty/partial file (e.g. an in-progress download); pick it up once it has content
		}
		// Still being written? Skip until it settles. An in-progress download has
		// no container index yet, so probing it is guaranteed to fail noisily and
		// the result would be an item with no dimensions or thumbnail. The next
		// scan picks it up. Deliberately before the seen[] bookkeeping below, so a
		// file that is already indexed is left exactly as it was rather than being
		// treated as missing.
		if s.cfg.ScanSettle > 0 && time.Since(info.ModTime()) < s.cfg.ScanSettle {
			if _, known := index[rel]; !known {
				unsettledMu.Lock()
				unsettled++
				unsettledMu.Unlock()
				return nil
			}
		}

		seenMu.Lock()
		seen[rel] = struct{}{}
		seenMu.Unlock()

		// Cheap change-detection: (size, mtime). No bytes read.
		cur, existed := index[rel]
		if existed && cur.Size == info.Size() && cur.MTime == info.ModTime().Unix() {
			// Unchanged. Skip once it has both a thumbnail and a signature;
			// otherwise reprocess once this run to backfill whatever's missing
			// (bounds pointless re-probing of files that can produce neither).
			// A video that predates the preview feature has a thumbnail and a
			// vector but no preview, so previews have to count toward "done"
			// here or they'd never backfill on an existing index.
			if cur.ThumbReady && cur.HasVector && (cur.PreviewReady || !s.wantsPreview(typ)) {
				return nil
			}
			if _, tried := s.thumbRetried[rel]; tried {
				return nil
			}
			s.thumbRetried[rel] = struct{}{}
			// fall through: reprocess once to backfill the thumbnail and/or vector
		}

		select {
		case jobs <- job{abs: abs, relpath: rel, info: info, typ: typ, updated: existed}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	close(jobs)
	<-writerDone // workers + writer have drained

	if ctx.Err() != nil {
		return stats, ctx.Err()
	}
	if walkErr != nil {
		log.Printf("scan: walk aborted: %v", walkErr)
	}

	// 5. Soft-delete files the walk no longer saw — but guard against the NAS
	//    being unmounted. If we saw zero files while the index has some, assume
	//    the mount is gone and skip pruning rather than flagging everything.
	if len(seen) == 0 && len(index) > 0 {
		log.Printf("scan: saw 0 files but index has %d — media root likely unavailable; skipping prune", len(index))
	} else {
		var missing []string
		for rel := range index {
			if _, ok := seen[rel]; !ok {
				missing = append(missing, rel)
			}
		}
		if n, err := s.store.MarkMissing(ctx, missing, stats.Started); err != nil {
			log.Printf("scan: mark missing: %v", err)
		} else {
			stats.Missing = n
		}
	}
	stats.Seen = len(seen)

	// 6. Rebuild chains (folders + chain.json). Fully derived, so rebuilt each
	//    scan, from the image files the walk above collected.
	if err := s.buildChains(ctx, chainDirs); err != nil {
		log.Printf("scan: build chains: %v", err)
	}

	stats.Ended = time.Now()
	if thumbFails > 0 {
		log.Printf("scan: %d file(s) had no thumbnail (unreadable frame / no cover art); e.g. %s", thumbFails, thumbSample)
	}
	if prevFails > 0 {
		log.Printf("scan: %d video(s) had no animated preview; e.g. %s", prevFails, prevSample)
	}
	if unsettled > 0 {
		log.Printf("scan: %d new file(s) skipped as still being written; will pick them up next scan", unsettled)
	}
	if partial > 0 {
		log.Printf("scan: %d file(s) are truncated or incomplete — no container index, "+
			"typically an interrupted download or a copy that stopped early; "+
			"they stay listed but have no dimensions/thumbnail. e.g. %s", partial, partialSample)
	}
	if err := s.store.RecordScanRun(ctx, stats); err != nil {
		log.Printf("scan: record run: %v", err)
	}
	return stats, nil
}

// process does the expensive per-file work: probe, thumbnail, optional hash,
// and sidecar read. It runs on a worker goroutine and touches no DB.
func (s *Scanner) process(ctx context.Context, j job) result {
	res := result{relpath: j.relpath, updated: j.updated}

	item := model.Item{
		Relpath:   j.relpath,
		Size:      j.info.Size(),
		MTime:     j.info.ModTime(),
		Type:      j.typ,
		IndexedAt: time.Now(),
		SeenAt:    time.Now(),
	}

	// Read the sidecar before any frame work: it carries the poster offset, which
	// decides WHICH frame becomes the thumbnail, the vision-model input, and the
	// perceptual signature. Reading it afterwards would silently ignore the
	// offset on the scan that first sees it.
	if sc, ok, err := sidecar.Read(s.cfg.SidecarPath(j.relpath)); err != nil {
		log.Printf("scan: sidecar %s: %v", j.relpath, err)
	} else if ok {
		res.write.ManualTags = sc.Tags()
		item.Offset = sc.Offset
	}

	// Probe is best-effort: a file we can't read metadata for (partial download,
	// odd codec, transient SMB hiccup) is still worth indexing so it's visible
	// and downloadable — a later scan re-probes once it settles.
	//
	// Truncated files are counted and summarised once per scan instead of logged
	// individually: a folder of interrupted downloads would otherwise reprint the
	// same "moov atom not found" every five minutes forever.
	if meta, err := media.Probe(ctx, s.cfg, j.abs, j.typ); err != nil {
		if media.IsIncomplete(err) {
			res.partial = true
		} else {
			log.Printf("scan: probe %s: %v", j.relpath, err)
		}
		item.MIME = media.MimeFor(j.abs)
	} else {
		item.Width, item.Height, item.Duration, item.MIME = meta.Width, meta.Height, meta.Duration, meta.MIME
	}

	// Thumbnail only when probing found a visual stream (width > 0). No width
	// means there's nothing to render a frame from — audio with no embedded
	// cover art, an animated webp this ffmpeg can't decode, or an unreadable
	// file — so ffmpeg would just fail. Skipping avoids a guaranteed error per
	// such file. (Cover-less audio shows a ♪; animated webp is rendered directly
	// in the grid.) Thumbnail failure is non-fatal regardless — we still index.
	if item.Width > 0 {
		if err := media.Thumbnail(ctx, s.cfg, j.abs, j.typ, item.Offset, media.ThumbPath(s.cfg.ThumbDir(), j.relpath)); err != nil {
			res.thumbErr = err.Error()
		} else {
			item.ThumbReady = true
		}
		// Perceptual signature for "looks similar" search (best-effort).
		if vec, err := media.Signature(ctx, s.cfg, j.abs, j.typ, item.Offset); err == nil {
			res.write.Vector = vec
		}

		// Animated preview: bursts of frames from across the whole video, so the
		// grid can show what's in it rather than just its opening seconds. Only
		// video needs this — a still has nothing to animate, and an existing
		// gif/animated webp is already its own preview. Clips under
		// PreviewMinDur are skipped for the same reason: at that length the
		// poster frame and the preview would show the same thing.
		if s.wantsPreview(j.typ) && item.Duration >= s.cfg.PreviewMinDur {
			dst := media.PreviewPath(s.cfg.PreviewDir(), j.relpath, s.cfg.PreviewFormat)
			if err := media.Preview(ctx, s.cfg, j.abs, item.Duration, dst); err != nil {
				res.prevErr = err.Error()
			} else {
				item.PreviewReady = true
			}
		}
	}

	// Content hash: dedup + move-detection only, NEVER change-detection
	// (that uses size+mtime). Default is "partial" — a head+tail+size
	// fingerprint that costs two small reads regardless of file size, so it
	// stays cheap over SMB. "full" reads every byte (strong but slow); "off"
	// skips it. Nothing consumes the hash yet; it's stored for future dedup.
	switch s.cfg.HashMode {
	case "full":
		if h, err := hashFileFull(j.abs); err != nil {
			log.Printf("scan: hash %s: %v", j.relpath, err)
		} else {
			item.ContentHash = h
		}
	case "partial":
		if h, err := hashFilePartial(j.abs, item.Size); err != nil {
			log.Printf("scan: hash %s: %v", j.relpath, err)
		} else {
			item.ContentHash = h
		}
	}

	res.write.Item = item
	return res
}

// wantsPreview reports whether this media type gets an animated preview.
func (s *Scanner) wantsPreview(typ model.MediaType) bool {
	return s.cfg.PreviewsEnabled() && typ == model.MediaVideo
}

// buildChains derives reading-order chains from the filesystem: any directory
// holding two or more images becomes a chain, ordered by filename. A chain.json
// in the directory overrides the title and gives an explicit order. byDir is
// the image relpaths the scan's walk found, keyed by their directory.
func (s *Scanner) buildChains(ctx context.Context, byDir map[string][]string) error {
	var out []store.ChainDef
	for dir, relpaths := range byDir {
		if len(relpaths) < 2 {
			continue // a single image isn't a chain
		}
		def := &store.ChainDef{Key: dir, Title: filepath.Base(dir), Kind: "folder", Source: dir, Relpaths: relpaths}
		sort.Strings(def.Relpaths) // default order: by filename
		if man, ok := sidecar.ReadChainManifest(filepath.Join(s.cfg.MediaRoot, def.Key)); ok {
			if man.Title != "" {
				def.Title = man.Title
			}
			if len(man.Order) > 0 {
				def.Relpaths = man.ResolveOrder(def.Key, def.Relpaths)
			}
			def.Kind = "manifest"
		}
		out = append(out, *def)
	}
	return s.store.ReplaceAllChains(ctx, out)
}

// RunPeriodic does an initial scan, then rescans on cfg.ScanEvery until ctx is
// cancelled. (SMB makes inotify unreliable, so we poll; the UI also exposes a
// manual rescan via POST /api/scan.)
func (s *Scanner) RunPeriodic(ctx context.Context) {
	logRun := func(st model.ScanStats, err error) {
		if err != nil {
			log.Printf("scan: %v", err)
			return
		}
		log.Printf("scan done: seen=%d added=%d updated=%d missing=%d errors=%d in %s",
			st.Seen, st.Added, st.Updated, st.Missing, st.Errors, st.Ended.Sub(st.Started).Round(time.Millisecond))
	}

	logRun(s.Scan(ctx))
	if s.cfg.ScanEvery <= 0 {
		return
	}
	t := time.NewTicker(s.cfg.ScanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logRun(s.Scan(ctx))
		}
	}
}

// hashFileFull returns the hex sha256 of a file's entire contents. Strong, but
// reads every byte — slow over SMB for large media. Used only for HASH_MODE=full.
func hashFileFull(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashBufs recycles hashFilePartial's read buffer: one per worker in flight,
// rather than a fresh 64 KiB allocation for every file a scan hashes.
var hashBufs = sync.Pool{New: func() any {
	b := make([]byte, partialChunk)
	return &b
}}

// partialChunk is how much hashFilePartial reads from each end of a file.
const partialChunk = 64 * 1024

// hashFilePartial fingerprints a file from its size plus its first and last
// 64 KiB. It reads at most 128 KiB no matter how large the file is, so it stays
// cheap over the network while still reliably matching / distinguishing real
// media files (enough to detect a moved or renamed file). Not a cryptographic
// uniqueness guarantee — a dedup/move hint, not something to trust blindly.
func hashFilePartial(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	const chunk = partialChunk
	h := sha256.New()

	// Fold in the size so different-length files can never collide.
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])

	bp := hashBufs.Get().(*[]byte)
	defer hashBufs.Put(bp)
	buf := *bp
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	h.Write(buf[:n])

	// Only read a tail if it wouldn't overlap the head we already hashed.
	if size > int64(2*chunk) {
		if _, err := f.Seek(-int64(chunk), io.SeekEnd); err != nil {
			return "", err
		}
		m, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return "", err
		}
		h.Write(buf[:m])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Running reports whether a scan is in progress (used by the UI to show a live
// indexing indicator).
func (s *Scanner) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
