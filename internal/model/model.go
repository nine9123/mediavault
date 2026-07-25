// Package model holds the plain domain types shared across the app. It imports
// nothing internal, so it can never be part of an import cycle.
package model

import "time"

// MediaType is the coarse kind of a media file, decided from its extension.
type MediaType string

const (
	MediaImage    MediaType = "image"    // still: jpg, png, avif, ...
	MediaAnimated MediaType = "animated" // gif / animated webp (looping, no audio)
	MediaVideo    MediaType = "video"    // mp4, webm, mkv, ...
	MediaAudio    MediaType = "audio"    // mp3, flac, m4a, ... (cover art used as thumbnail)
)

// Item is one media file. Everything here is *derivable* from the file (plus
// its sidecar), which is why the DB row is a disposable cache: drop it and a
// rescan reconstructs every field.
type Item struct {
	ID           int64
	Relpath      string // path under the media root, slash-separated; the stable address
	Size         int64
	MTime        time.Time
	ContentHash  string // empty until hashed; used for dedup + move-detection only
	Type         MediaType
	MIME         string
	Width        int
	Height       int
	Duration     float64 // seconds; video + audio
	ThumbReady   bool
	Views        int // times opened in the viewer (popularity signal)
	IndexedAt    time.Time
	SeenAt       time.Time
	MissingSince *time.Time // set when a scan stops seeing the file (soft delete)
}

// Tag is a booru-style label. Namespace is optional ("" when absent), e.g.
// the tag "artist:foo" parses to {Namespace: "artist", Name: "foo"}.
type Tag struct {
	ID        int64
	Namespace string
	Name      string
}

// TagSource distinguishes human-confirmed tags from machine suggestions so the
// UI can show them differently and so a DB rebuild can re-run the AI tagger
// without touching manual tags.
type TagSource string

const (
	TagManual TagSource = "manual" // from a sidecar, authored by you
	TagAI     TagSource = "ai"     // from the (optional) tagger container
)

// Chain is an ordered reading sequence — your "story". Derived from the
// filesystem (a folder, optionally refined by a chain.json manifest).
type Chain struct {
	ID     int64
	Key    string // stable key: the folder's relpath (or an id from chain.json)
	Title  string
	Kind   string // "folder" | "manifest"
	Source string // path that defined it
}

// ScanStats summarizes one scan pass.
type ScanStats struct {
	Seen    int
	Added   int
	Updated int
	Missing int
	Errors  int
	Started time.Time
	Ended   time.Time
}
