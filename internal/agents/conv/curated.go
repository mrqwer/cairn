package conv

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"
)

const curatedDir = "curated"

// Curated is the user-curated part of a conversation (R2): name, purpose,
// tags, archive and removal. It lives in its own small file per
// conversation, <Dir>/curated/<id>.json, so every change to it is a file
// change that internal/history records and undo reverts. When the file
// exists its fields win over the record's.
type Curated struct {
	Name       string     `json:"name,omitempty"`
	Purpose    string     `json:"purpose,omitempty"`
	Tags       []string   `json:"tags,omitempty"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	// Removed hides the conversation; the record itself is kept.
	Removed bool `json:"removed,omitempty"`
}

// CuratedOf returns the curated fields of c.
func CuratedOf(c Conversation) Curated {
	return Curated{Name: c.Name, Purpose: c.Purpose, Tags: append([]string(nil), c.Tags...), ArchivedAt: c.ArchivedAt}
}

func (cu Curated) apply(c *Conversation) {
	c.Name, c.Purpose, c.Tags, c.ArchivedAt = cu.Name, cu.Purpose, append([]string(nil), cu.Tags...), cu.ArchivedAt
}

// Encode renders the overlay file content.
func (cu Curated) Encode() []byte {
	b, _ := json.Marshal(cu) // a struct of strings, times and bools cannot fail
	return append(b, '\n')
}

// Decode parses overlay file content.
func (cu *Curated) Decode(data []byte) error { return json.Unmarshal(data, cu) }

// CuratedPath is the overlay file of id, as an absolute path under s.Root
// (the form internal/history records).
func (s Store) CuratedPath(id string) string {
	return s.Dir + "/" + curatedDir + "/" + safeName(id) + ".json"
}

// CuratedDir is the directory of the overlay files.
func (s Store) CuratedDir() string { return s.Dir + "/" + curatedDir }

// ReadCurated returns the overlay file of id and whether it exists.
func (s Store) ReadCurated(id string) ([]byte, bool, error) {
	b, err := s.Root.ReadFile(s.CuratedPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// WriteCurated atomically writes the overlay of id (0600).
func (s Store) WriteCurated(id string, cu Curated) error {
	if err := s.Root.MkdirAll(s.CuratedDir(), 0o700); err != nil {
		return err
	}
	return s.Root.WriteAtomic(s.CuratedPath(id), cu.Encode(), 0o600)
}

// SeedCurated creates the overlay of id from its record when there is none,
// so that a later recorded change has a "before" state that undo can return
// to. An existing overlay is left as it is.
func (s Store) SeedCurated(id string) error {
	if _, ok, err := s.ReadCurated(id); err != nil || ok {
		return err
	}
	recs, err := s.loadRaw()
	if err != nil {
		return err
	}
	for _, c := range recs {
		if c.ID == id {
			return s.WriteCurated(id, CuratedOf(c))
		}
	}
	return ErrNotFound
}

// overlays reads every overlay file, keyed by file name. An unreadable or
// unparsable one is ignored (the record shows as written).
func (s Store) overlays() (map[string]Curated, error) {
	ents, err := os.ReadDir(s.Root.Path(s.CuratedDir()))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]Curated, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(s.Root.Path(s.CuratedDir() + "/" + e.Name()))
		if err != nil {
			continue
		}
		var cu Curated
		if cu.Decode(b) != nil {
			continue
		}
		out[e.Name()] = cu
	}
	return out, nil
}
