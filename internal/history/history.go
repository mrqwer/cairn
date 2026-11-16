// Package history implements cairn's private append-only history of config
// changes, with file snapshots so a change can be undone or restored.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/fsx"
)

// Event kinds recorded by Undo and Restore.
const (
	KindUndo    = "undo"
	KindRestore = "restore"
)

// ErrConflict is returned by Undo when a file changed after the event.
var ErrConflict = errors.New("history: files changed since the event")

// FileChange is a snapshot of one file before and after a change. Path is
// absolute. Before and After are base64 in JSON.
type FileChange struct {
	Path         string `json:"path"`
	Before       []byte `json:"before"`
	After        []byte `json:"after"`
	BeforeExists bool   `json:"before_exists"`
	AfterExists  bool   `json:"after_exists"`
}

// Event is one history record. ID is a zero-padded sequence (000001, ...):
// the number of valid events already in the file plus one at append time. IDs
// are unique and ordered per history file.
type Event struct {
	ID      string       `json:"id"`
	TS      time.Time    `json:"ts"`
	Kind    string       `json:"kind"`
	Summary string       `json:"summary"`
	Files   []FileChange `json:"files"`
}

// Store is an append-only JSONL history file at Path under Root (mode 0600).
// Undo and Restore only write paths inside AllowedRoots; with none, they
// refuse every path.
type Store struct {
	Root         fsx.Root
	Path         string
	AllowedRoots []string
}

// Append records an event and fsyncs. The file is read and written under an
// exclusive flock so concurrent processes never produce the same ID. A torn
// trailing line is terminated first so the new record is not merged into it.
func (s Store) Append(kind, summary string, files []FileChange) (Event, error) {
	if err := s.Root.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return Event{}, err
	}
	f, err := os.OpenFile(s.Root.Path(s.Path), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close() //nolint:errcheck // read-side close; write errors are returned below
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return Event{}, fmt.Errorf("lock history: %w", err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:errcheck

	data, err := os.ReadFile(s.Root.Path(s.Path))
	if err != nil {
		return Event{}, err
	}
	e := Event{
		ID:      fmt.Sprintf("%06d", len(parse(data))+1),
		TS:      time.Now().UTC().Truncate(time.Second),
		Kind:    kind,
		Summary: summary,
		Files:   files,
	}
	b, err := json.Marshal(e)
	if err != nil {
		return Event{}, fmt.Errorf("marshal event: %w", err)
	}
	b = append(b, '\n')
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b = append([]byte{'\n'}, b...)
	}
	if _, err := f.Write(b); err != nil {
		return Event{}, err
	}
	if err := f.Sync(); err != nil {
		return Event{}, err
	}
	return e, f.Close()
}

// parse returns the valid events in file order; unparsable and unterminated
// lines are skipped.
func parse(data []byte) []Event {
	var evs []Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<30)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		evs = append(evs, e)
	}
	return evs
}

func (s Store) all() ([]Event, error) {
	data, err := s.Root.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parse(data), nil
}

// List returns events newest first; limit <= 0 returns all.
func (s Store) List(limit int) ([]Event, error) {
	evs, err := s.all()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
	}
	return evs, nil
}

// Get returns the event with the given id.
func (s Store) Get(id string) (Event, error) {
	evs, err := s.all()
	if err != nil {
		return Event{}, err
	}
	for _, e := range evs {
		if e.ID == id {
			return e, nil
		}
	}
	return Event{}, fmt.Errorf("history: event %q not found", id)
}

// allowed reports whether p may be written: absolute, free of "..", and equal
// to or below one of the allowed roots (compared on path boundaries).
func (s Store) allowed(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return false
		}
	}
	c := filepath.Clean(p)
	for _, r := range s.AllowedRoots {
		r = filepath.Clean(r)
		if c == r || strings.HasPrefix(c, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

func (s Store) checkPaths(e Event) error {
	for _, f := range e.Files {
		if !s.allowed(f.Path) {
			return fmt.Errorf("history: refusing path %q in event %s: outside the allowed roots", f.Path, e.ID)
		}
	}
	return nil
}

// current reads the file's present content and existence.
func (s Store) current(p string) ([]byte, bool, error) {
	b, err := s.Root.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (s Store) put(p string, data []byte, exists bool) error {
	if !exists {
		return s.Root.Remove(p)
	}
	return s.Root.WriteAtomic(p, data, 0o644)
}

// Undo applies the inverse of event id. Each file must still match the
// event's After state, otherwise ErrConflict names the paths and nothing is
// written. The reversed changes are recorded as an "undo" event.
func (s Store) Undo(id string) (Event, error) {
	e, err := s.Get(id)
	if err != nil {
		return Event{}, err
	}
	if err := s.checkPaths(e); err != nil {
		return Event{}, err
	}
	var conflicts []string
	rev := make([]FileChange, 0, len(e.Files))
	for _, f := range e.Files {
		cur, exists, err := s.current(f.Path)
		if err != nil {
			return Event{}, err
		}
		if exists != f.AfterExists || (exists && !bytes.Equal(cur, f.After)) {
			conflicts = append(conflicts, f.Path)
		}
		rev = append(rev, FileChange{Path: f.Path, Before: cur, After: f.Before, BeforeExists: exists, AfterExists: f.BeforeExists})
	}
	if len(conflicts) > 0 {
		return Event{}, fmt.Errorf("%w: %s", ErrConflict, strings.Join(conflicts, ", "))
	}
	return s.apply(KindUndo, fmt.Sprintf("undo %s: %s", e.ID, e.Summary), rev)
}

// Restore writes every file of event id to its After state regardless of
// later changes and records a "restore" event.
func (s Store) Restore(id string) (Event, error) {
	e, err := s.Get(id)
	if err != nil {
		return Event{}, err
	}
	if err := s.checkPaths(e); err != nil {
		return Event{}, err
	}
	ch := make([]FileChange, 0, len(e.Files))
	for _, f := range e.Files {
		cur, exists, err := s.current(f.Path)
		if err != nil {
			return Event{}, err
		}
		ch = append(ch, FileChange{Path: f.Path, Before: cur, After: f.After, BeforeExists: exists, AfterExists: f.AfterExists})
	}
	return s.apply(KindRestore, fmt.Sprintf("restore %s: %s", e.ID, e.Summary), ch)
}

func (s Store) apply(kind, summary string, ch []FileChange) (Event, error) {
	for _, c := range ch {
		if err := s.put(c.Path, c.After, c.AfterExists); err != nil {
			return Event{}, fmt.Errorf("write %s: %w", c.Path, err)
		}
	}
	return s.Append(kind, summary, ch)
}

// Diff renders a small line-based diff: common prefix and suffix lines are
// trimmed, the middle is shown as removed ("- ") then added ("+ ") lines.
func Diff(c FileChange) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", c.Path, c.Path)
	switch {
	case !c.BeforeExists && !c.AfterExists:
		return b.String()
	case !c.BeforeExists:
		b.WriteString("(new file)\n")
	case !c.AfterExists:
		b.WriteString("(removed)\n")
	}
	var old, nu []string
	if c.BeforeExists {
		old = lines(c.Before)
	}
	if c.AfterExists {
		nu = lines(c.After)
	}
	p := 0
	for p < len(old) && p < len(nu) && old[p] == nu[p] {
		p++
	}
	sfx := 0
	for sfx < len(old)-p && sfx < len(nu)-p && old[len(old)-1-sfx] == nu[len(nu)-1-sfx] {
		sfx++
	}
	for _, l := range old[p : len(old)-sfx] {
		b.WriteString("- " + l + "\n")
	}
	for _, l := range nu[p : len(nu)-sfx] {
		b.WriteString("+ " + l + "\n")
	}
	return b.String()
}

func lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}
