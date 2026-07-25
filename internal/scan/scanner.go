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
	"encoding/hex"
	"fmt"
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
}

// New creates a Scanner.
func New(cfg config.Config, st *store.Store) *Scanner {
	return &Scanner{cfg: cfg, store: st}
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
	relpath string
	updated bool
	write   store.ItemWrite
	err     error
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

	// 1. Snapshot the present index up front so change-detection is pure map
	//    lookups — no per-file DB round-trips during the walk.
	index, err := s.store.LoadIndex(ctx)
	if err != nil {
		return stats, err
	}

	seen := make(map[string]struct{}, len(index))
	var seenMu sync.Mutex

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

	// 3. Single writer: batch results into transactions.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		const batchSize = 128
		batch := make([]store.ItemWrite, 0, batchSize)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := s.store.WriteBatch(ctx, batch); err != nil {
				log.Printf("scan: write batch: %v", err)
				stats.Errors++
			}
			batch = batch[:0]
		}
		for r := range results {
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
			batch = append(batch, r.write)
			if len(batch) >= batchSize {
				flush()
			}
		}
		flush()
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

		info, err := d.Info()
		if err != nil {
			return nil
		}

		seenMu.Lock()
		seen[rel] = struct{}{}
		seenMu.Unlock()

		// Cheap change-detection: (size, mtime). No bytes read.
		cur, existed := index[rel]
		if existed && cur.Size == info.Size() && cur.MTime == info.ModTime().Unix() {
			return nil // unchanged -> skip the whole expensive path
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
	//    scan. This is a second, stat-only walk; it could be folded into the
	//    walk above later, but keeping it separate keeps the pipeline clean.
	if err := s.buildChains(ctx); err != nil {
		log.Printf("scan: build chains: %v", err)
	}

	stats.Ended = time.Now()
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

	meta, err := media.Probe(ctx, s.cfg, j.abs, j.typ)
	if err != nil {
		res.err = fmt.Errorf("probe: %w", err)
		return res
	}
	item.Width, item.Height, item.Duration, item.MIME = meta.Width, meta.Height, meta.Duration, meta.MIME

	// Thumbnail failure is non-fatal — we still index the item.
	if err := media.Thumbnail(ctx, s.cfg, j.abs, j.typ, media.ThumbPath(s.cfg.ThumbDir(), j.relpath)); err != nil {
		log.Printf("scan: thumbnail %s: %v", j.relpath, err)
	} else {
		item.ThumbReady = true
	}

	// Content hash: dedup + move-detection only, NEVER change-detection.
	// Honest note: ffmpeg (not us) read the file for the probe/thumbnail, so
	// this hash is a *second* full read over the network per new file. It's
	// opt-in and isolated here precisely so it's easy to make cheaper — swap
	// hashFile for a head+tail+size partial hash if the full read costs too
	// much. Change-detection never needs it; it uses (size, mtime).
	if s.cfg.HashOnIndex {
		if h, err := hashFile(j.abs); err != nil {
			log.Printf("scan: hash %s: %v", j.relpath, err)
		} else {
			item.ContentHash = h
		}
	}

	// Fold in human-authored tags from the sidecar (source of truth).
	if sc, ok, err := sidecar.Read(j.abs); err != nil {
		log.Printf("scan: sidecar %s: %v", j.relpath, err)
	} else if ok {
		res.write.ManualTags = sc.Tags()
	}

	res.write.Item = item
	return res
}

// buildChains derives reading-order chains from the filesystem: any directory
// holding two or more images becomes a chain, ordered by filename. A chain.json
// in the directory overrides the title and gives an explicit order.
func (s *Scanner) buildChains(ctx context.Context) error {
	byDir := map[string]*store.ChainDef{}

	err := filepath.WalkDir(s.cfg.MediaRoot, func(abs string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || sidecar.IsSidecar(name) {
			return nil
		}
		typ, ok := s.cfg.MediaTypeFor(name)
		if !ok || typ == model.MediaVideo {
			return nil // chains are image stories
		}
		rel, err := filepath.Rel(s.cfg.MediaRoot, abs)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			return nil // don't chain loose files at the root
		}
		def := byDir[dir]
		if def == nil {
			def = &store.ChainDef{Key: dir, Title: filepath.Base(dir), Kind: "folder", Source: dir}
			byDir[dir] = def
		}
		def.Relpaths = append(def.Relpaths, rel)
		return nil
	})
	if err != nil {
		return err
	}

	var out []store.ChainDef
	for _, def := range byDir {
		if len(def.Relpaths) < 2 {
			continue // a single image isn't a chain
		}
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

// hashFile returns the hex sha256 of a file's contents. See the note in
// process about the cost of this read over SMB.
func hashFile(path string) (string, error) {
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

// Running reports whether a scan is in progress (used by the UI to show a live
// indexing indicator).
func (s *Scanner) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
