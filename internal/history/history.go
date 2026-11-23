// Package history implements cairn's private append-only history of config
// changes, with file snapshots so a change can be undone or restored.
//
// The AllowedRoots guard is defence in depth for a user-owned config
// directory: paths are cleaned and must be strict descendants of a root, but
// symlinked intermediate directories inside the roots are followed, and a
// root of "/" allows everything. Undo and Restore write files with mode 0644.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// ErrConflict is returned by Undo when a file is neither at the event's After
// nor at its Before state.
var ErrConflict = errors.New("history: files changed since the event")

// ErrAlreadyApplied is returned by Undo when the event is already undone.
var ErrAlreadyApplied = errors.New("history: already undone")

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

// header is the cheap per-line view used to scan the file without
// materialising snapshots.
type header struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

// scanPath streams the valid lines of the history file (a missing file is
// empty). Lines of any length are supported; unparsable lines are skipped.
func (s Store) scanPath(fn func(h header, line []byte)) error {
	f, err := os.Open(s.Root.Path(s.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read only
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		var h header
		if len(line) > 0 && json.Unmarshal(line, &h) == nil && h.ID != "" {
			fn(h, line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// nextID is max(numeric ID of every valid event)+1, so a deleted or corrupt
// line never causes a duplicate.
func (s Store) nextID() (string, error) {
	maxID := 0
	err := s.scanPath(func(h header, _ []byte) {
		if n, err := strconv.Atoi(h.ID); err == nil && n > maxID {
			maxID = n
		}
	})
	return fmt.Sprintf("%06d", maxID+1), err
}

// lock opens the history file (creating it 0600) and takes the exclusive
// flock; the returned func releases it.
func (s Store) lock() (*os.File, func(), error) {
	if err := s.Root.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(s.Root.Path(s.Path), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("lock history: %w", err)
	}
	return f, func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

// Append records an event and fsyncs. The file is locked exclusively while the
// next ID is computed and the line written, so concurrent processes never
// produce the same ID. IDs are unique and ordered per file. A torn trailing
// line is terminated first so the new record is not merged into it. Kind
// "undo" is refused: only Undo writes undo events (they carry the
// "undo <id>: ..." summary UndoneIDs relies on).
func (s Store) Append(kind, summary string, files []FileChange) (Event, error) {
	if kind == KindUndo {
		return Event{}, errors.New("history: undo events are written by Undo only")
	}
	f, unlock, err := s.lock()
	if err != nil {
		return Event{}, err
	}
	defer unlock()
	return s.appendLocked(f, kind, summary, files)
}

// appendLocked appends while the caller holds the flock on f.
func (s Store) appendLocked(f *os.File, kind, summary string, files []FileChange) (Event, error) {
	id, err := s.nextID()
	if err != nil {
		return Event{}, err
	}
	e := Event{ID: id, TS: time.Now().UTC().Truncate(time.Second), Kind: kind, Summary: summary, Files: files}
	b, err := json.Marshal(e)
	if err != nil {
		return Event{}, fmt.Errorf("marshal event: %w", err)
	}
	b = append(b, '\n')
	st, err := f.Stat()
	if err != nil {
		return Event{}, err
	}
	if st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err != nil {
			return Event{}, err
		}
		if last[0] != '\n' {
			b = append([]byte{'\n'}, b...)
		}
	}
	if _, err := f.Write(b); err != nil {
		return Event{}, err
	}
	if err := f.Sync(); err != nil {
		return Event{}, err
	}
	return e, nil
}

// List returns events newest first; limit <= 0 returns all. Only the returned
// events are fully decoded.
func (s Store) List(limit int) ([]Event, error) {
	var lines [][]byte
	if err := s.scanPath(func(_ header, line []byte) { lines = append(lines, line) }); err != nil {
		return nil, err
	}
	var evs []Event
	for i := len(lines) - 1; i >= 0 && (limit <= 0 || len(evs) < limit); i-- {
		var e Event
		if json.Unmarshal(lines[i], &e) == nil {
			evs = append(evs, e)
		}
	}
	return evs, nil
}

// Get returns the event with the given id, decoding only that event.
func (s Store) Get(id string) (Event, error) {
	e, _, err := s.find(id)
	return e, err
}

// undoTarget returns the id an undo event reverted ("undo <id>: ..." as
// recorded by Undo); ok is false for any other event.
func undoTarget(kind, summary string) (string, bool) {
	if kind != KindUndo {
		return "", false
	}
	rest, ok := strings.CutPrefix(summary, KindUndo+" ")
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, ": ")
	return id, ok && id != ""
}

// UndoneIDs returns the ids of the events in evs that an undo event in evs
// reverted.
func UndoneIDs(evs []Event) map[string]bool {
	out := map[string]bool{}
	for _, e := range evs {
		if id, ok := undoTarget(e.Kind, e.Summary); ok {
			out[id] = true
		}
	}
	return out
}

// find returns event id and whether an undo event for it is recorded.
func (s Store) find(id string) (Event, bool, error) {
	var found []byte
	undone := false
	err := s.scanPath(func(h header, line []byte) {
		if h.ID == id && found == nil {
			found = line
		}
		if t, ok := undoTarget(h.Kind, h.Summary); ok && t == id {
			undone = true
		}
	})
	if err != nil {
		return Event{}, false, err
	}
	var e Event
	if found == nil || json.Unmarshal(found, &e) != nil {
		return Event{}, false, fmt.Errorf("history: event %q not found", id)
	}
	return e, undone, nil
}

// allowed returns the cleaned form of p and whether it may be written:
// absolute, free of "..", and a strict descendant of an allowed root (compared
// on path boundaries; the root itself is refused). Callers must use the
// returned cleaned path, never the raw one.
func (s Store) allowed(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return "", false
		}
	}
	c := filepath.Clean(p)
	for _, r := range s.AllowedRoots {
		r = filepath.Clean(r)
		if c != r && strings.HasPrefix(c, strings.TrimSuffix(r, "/")+"/") {
			return c, true
		}
	}
	return "", false
}

// checkPaths validates every path of e and rewrites them to cleaned form.
func (s Store) checkPaths(e Event) (Event, error) {
	files := make([]FileChange, len(e.Files))
	for i, f := range e.Files {
		c, ok := s.allowed(f.Path)
		if !ok {
			return e, fmt.Errorf("history: refusing path %q in event %s: outside the allowed roots", f.Path, e.ID)
		}
		f.Path = c
		files[i] = f
	}
	e.Files = files
	return e, nil
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

func at(cur []byte, exists bool, want []byte, wantExists bool) bool {
	return exists == wantExists && (!exists || bytes.Equal(cur, want))
}

// Undo applies the inverse of event id under the history flock. A file at the
// event's After state is reverted; one already at Before counts as reverted
// (so re-running after a crash completes the undo); anything else is an
// ErrConflict naming the paths, with nothing written. If a write fails the
// files already written are rolled back best-effort. The reversed changes are
// recorded as an "undo" event; if only that append fails, the files stay
// reverted and a re-run completes and records it. When every file is already
// reverted and an undo of id is recorded, ErrAlreadyApplied is returned.
func (s Store) Undo(id string) (Event, error) {
	f, unlock, err := s.lock()
	if err != nil {
		return Event{}, err
	}
	defer unlock()
	e, undone, err := s.find(id)
	if err != nil {
		return Event{}, err
	}
	if e, err = s.checkPaths(e); err != nil {
		return Event{}, err
	}
	var conflicts []string
	var todo []FileChange
	rev := make([]FileChange, 0, len(e.Files))
	for _, c := range e.Files {
		cur, exists, err := s.current(c.Path)
		if err != nil {
			return Event{}, err
		}
		switch {
		case at(cur, exists, c.After, c.AfterExists):
			ch := FileChange{Path: c.Path, Before: cur, After: c.Before, BeforeExists: exists, AfterExists: c.BeforeExists}
			rev = append(rev, ch)
			todo = append(todo, ch)
		case at(cur, exists, c.Before, c.BeforeExists):
			rev = append(rev, FileChange{Path: c.Path, Before: c.After, After: c.Before, BeforeExists: c.AfterExists, AfterExists: c.BeforeExists})
		default:
			conflicts = append(conflicts, c.Path)
		}
	}
	if len(conflicts) > 0 {
		return Event{}, fmt.Errorf("%w: %s", ErrConflict, strings.Join(conflicts, ", "))
	}
	if len(todo) == 0 && undone {
		return Event{}, fmt.Errorf("%w: event %s", ErrAlreadyApplied, id)
	}
	if err := s.writeAll(todo); err != nil {
		return Event{}, err
	}
	return s.appendLocked(f, KindUndo, fmt.Sprintf("%s %s: %s", KindUndo, e.ID, e.Summary), rev)
}

// Restore writes every file of event id to its After state regardless of
// later changes (a file already there is skipped) and records a "restore"
// event. Failure handling matches Undo.
func (s Store) Restore(id string) (Event, error) {
	f, unlock, err := s.lock()
	if err != nil {
		return Event{}, err
	}
	defer unlock()
	e, err := s.Get(id)
	if err != nil {
		return Event{}, err
	}
	if e, err = s.checkPaths(e); err != nil {
		return Event{}, err
	}
	var todo []FileChange
	ch := make([]FileChange, 0, len(e.Files))
	for _, c := range e.Files {
		cur, exists, err := s.current(c.Path)
		if err != nil {
			return Event{}, err
		}
		fc := FileChange{Path: c.Path, Before: cur, After: c.After, BeforeExists: exists, AfterExists: c.AfterExists}
		ch = append(ch, fc)
		if !at(cur, exists, c.After, c.AfterExists) {
			todo = append(todo, fc)
		}
	}
	if err := s.writeAll(todo); err != nil {
		return Event{}, err
	}
	return s.appendLocked(f, KindRestore, fmt.Sprintf("%s %s: %s", KindRestore, e.ID, e.Summary), ch)
}

// writeAll applies each change's After state; on failure it best-effort rolls
// back the files already written and returns the original error.
func (s Store) writeAll(ch []FileChange) error {
	for i, c := range ch {
		if err := s.put(c.Path, c.After, c.AfterExists); err != nil {
			err = fmt.Errorf("write %s: %w", c.Path, err)
			var failed []string
			for _, d := range ch[:i] {
				if rerr := s.put(d.Path, d.Before, d.BeforeExists); rerr != nil {
					failed = append(failed, d.Path)
				}
			}
			if len(failed) > 0 {
				err = fmt.Errorf("%w (rollback also failed for: %s)", err, strings.Join(failed, ", "))
			}
			return err
		}
	}
	return nil
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
