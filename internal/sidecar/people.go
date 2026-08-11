package sidecar

import (
	"encoding/json"
	"os"
	"strings"
)

// People is the durable answer to "who is who": a name plus the faces that
// define that person. It lives next to the tag sidecars (see
// config.PeoplePath), not in the database, for the same reason tags do — the
// database is a disposable cache, and a name is a human judgement that no
// rescan could reconstruct.
//
// Faces are referenced by (path, box) rather than by an embedding blob, so the
// file stays readable and hand-editable:
//
//	{
//	  "people": [
//	    { "name": "Jane", "faces": [
//	        { "path": "2019/beach/img_0042.jpg", "box": [0.31, 0.12, 0.18, 0.24] }
//	    ] }
//	  ]
//	}
//
// After the cache is dropped, re-detection produces boxes in the same places,
// those references resolve back to embeddings, and everyone who looks like them
// is re-labelled. A reference that no longer resolves (the file was deleted, or
// a newer model frames faces differently) costs that person one example rather
// than their identity.
type People struct {
	People []Person `json:"people"`
}

// Person is one named identity and its example faces.
type Person struct {
	Name  string    `json:"name"`
	Faces []FaceRef `json:"faces"`
}

// FaceRef locates one example face: a media relpath plus the face box as
// fractions of the frame ([x, y, w, h]), which is resolution-independent and so
// survives a change of FACE_FRAME_PX.
//
// T is how many seconds into a video the face was found, and is what keeps two
// people apart in one file: a video is sampled across its length, and in an
// interview two speakers filmed alternately occupy the same box in the frame.
// Absent (and zero) for stills, and for anything named before faces carried a
// timestamp — where it means "wherever in this file the box matches", which is
// what resolving used to do for everything.
type FaceRef struct {
	Path string     `json:"path"`
	Box  [4]float64 `json:"box"`
	T    float64    `json:"t,omitempty"`
}

// ReadPeople loads people.json. A missing file is not an error — it just means
// nobody has been named yet.
func ReadPeople(path string) (People, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return People{}, nil
	}
	if err != nil {
		return People{}, err
	}
	var p People
	if err := json.Unmarshal(b, &p); err != nil {
		return People{}, err
	}
	return p, nil
}

// WritePeople persists people.json atomically (temp file + rename), like Write.
func WritePeople(path string, p People) error {
	if p.People == nil {
		p.People = []Person{}
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// Find returns the index of the person with this name (case-insensitive, since
// "Jane" and "jane" are one person to everyone except a byte comparison), or -1.
func (p People) Find(name string) int {
	name = strings.TrimSpace(name)
	for i, q := range p.People {
		if strings.EqualFold(strings.TrimSpace(q.Name), name) {
			return i
		}
	}
	return -1
}
