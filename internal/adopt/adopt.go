// Package adopt brings a live, unmanaged tmux session under cairn's
// management without restarting or changing it: it only sets cairn's own
// user options (@cairn_s, @cairn_w, @cairn_p) on the session, its windows
// and its panes, and it can remove them again (Release).
package adopt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mrqwer/cairn/internal/capture"
	"github.com/mrqwer/cairn/internal/tmux"
)

// Marker option names, one per level (tmux format lookup falls back from
// pane to window to session, so they must differ).
const (
	OptSession = "@cairn_s"
	OptWindow  = "@cairn_w"
	OptPane    = "@cairn_p"
)

// Mark is one user option to set: Name = Value on the object Target (a tmux
// id) at Scope "session", "window" or "pane".
type Mark struct {
	Scope  string
	Target string
	Name   string
	Value  string
}

// String describes the marked object, e.g. "pane %3 (demo/main/2)".
func (m Mark) String() string { return fmt.Sprintf("%s %s (%s)", m.Scope, m.Target, m.Value) }

// Driver is the part of tmux.Client adoption uses: it reads snapshots and
// sets or removes user options, nothing else.
type Driver interface {
	Snapshot(ctx context.Context) (tmux.Snapshot, error)
	SetOption(ctx context.Context, scope, target, name, value string) error
	UnsetOption(ctx context.Context, scope, target, name string) error
}

// Markers returns the marks that make the captured session managed, in the
// order session, windows (config order), panes (config order):
// @cairn_s = <project>, @cairn_w = <project>/<window key>,
// @cairn_p = <project>/<window key>/<n>.
func Markers(r capture.Result) []Mark {
	id := r.Project.ID
	marks := []Mark{{Scope: "session", Target: r.SessionID, Name: OptSession, Value: id}}
	seen := map[string]bool{}
	for _, ref := range r.Mapping {
		if !seen[ref.WindowID] {
			seen[ref.WindowID] = true
			marks = append(marks, Mark{Scope: "window", Target: ref.WindowID, Name: OptWindow, Value: id + "/" + ref.WindowKey})
		}
	}
	for _, ref := range r.Mapping {
		marks = append(marks, Mark{Scope: "pane", Target: ref.PaneID, Name: OptPane,
			Value: fmt.Sprintf("%s/%s/%d", id, ref.WindowKey, ref.Pane)})
	}
	return marks
}

// object is a tmux object found in a snapshot with what contains it and the
// cairn marker it carries.
type object struct {
	sess tmux.Session
	win  tmux.Window
	key  string
}

// find locates the object m targets in snap.
func find(snap tmux.Snapshot, scope, target string) (object, bool) {
	for _, s := range snap.Sessions {
		if scope == "session" && s.ID == target {
			return object{sess: s, key: s.Key}, true
		}
		for _, w := range s.Windows {
			if scope == "window" && w.ID == target {
				return object{sess: s, win: w, key: w.Key}, true
			}
			if scope != "pane" {
				continue
			}
			for _, p := range w.Panes {
				if p.ID == target {
					return object{sess: s, win: w, key: p.Key}, true
				}
			}
		}
	}
	return object{}, false
}

func scopeRank(scope string) int {
	switch scope {
	case "session":
		return 0
	case "window":
		return 1
	}
	return 2
}

// verify checks mark m against cur, the server now, given orig, the
// snapshot the marks were made from: the object still exists, in the same
// session (and window) under the same session name, and carries no cairn
// marker or exactly m.Value; its session is unmarked or marked with m's
// project. It returns the object as it is now.
func verify(orig, cur tmux.Snapshot, m Mark) (object, error) {
	o, ok := find(orig, m.Scope, m.Target)
	if !ok {
		return object{}, fmt.Errorf("%s: not in the captured session", m)
	}
	c, ok := find(cur, m.Scope, m.Target)
	switch {
	case !ok:
		return object{}, fmt.Errorf("%s no longer exists", m)
	case c.sess.ID != o.sess.ID || c.win.ID != o.win.ID:
		return object{}, fmt.Errorf("%s was moved to another session or window", m)
	case c.sess.Name != o.sess.Name:
		return object{}, fmt.Errorf("%s: session %q was renamed to %q", m, o.sess.Name, c.sess.Name)
	case c.key != "" && c.key != m.Value:
		return object{}, fmt.Errorf("%s: already carries marker %q", m, c.key)
	}
	project, _, _ := strings.Cut(m.Value, "/")
	if c.sess.Key != "" && c.sess.Key != project {
		return object{}, fmt.Errorf("%s: its session is managed as project %s", m, c.sess.Key)
	}
	return c, nil
}

// Check verifies every mark against snap itself (see Apply) without
// touching tmux, so a conflict is found before anything is written.
func Check(snap tmux.Snapshot, marks []Mark) error {
	for _, m := range marks {
		if _, err := verify(snap, snap, m); err != nil {
			return err
		}
	}
	return nil
}

// Apply sets marks in the order session, windows, panes. Before each one it
// reads a fresh snapshot and requires the object to still exist where snap
// (the snapshot the marks were made from) saw it, under the same session
// name, carrying no cairn marker or exactly the one to set (then nothing is
// written). On any failure the marks this run set are removed again and the
// error names the object.
func Apply(ctx context.Context, d Driver, snap tmux.Snapshot, marks []Mark) error {
	ordered := append([]Mark(nil), marks...)
	sort.SliceStable(ordered, func(i, j int) bool { return scopeRank(ordered[i].Scope) < scopeRank(ordered[j].Scope) })
	var set []Mark
	for _, m := range ordered {
		cur, err := d.Snapshot(ctx)
		if err != nil {
			return rollback(ctx, d, set, fmt.Errorf("adopt: %w", err))
		}
		c, err := verify(snap, cur, m)
		if err != nil {
			return rollback(ctx, d, set, fmt.Errorf("adopt aborted: %w", err))
		}
		if c.key == m.Value {
			continue
		}
		if err := d.SetOption(ctx, m.Scope, m.Target, m.Name, m.Value); err != nil {
			return rollback(ctx, d, set, fmt.Errorf("adopt aborted: %s: %w", m, err))
		}
		set = append(set, m)
	}
	return nil
}

func rollback(ctx context.Context, d Driver, set []Mark, cause error) error {
	if len(set) == 0 {
		return cause
	}
	if err := Unmark(ctx, d, set); err != nil {
		return fmt.Errorf("%w; removing the %d markers already set failed: %v", cause, len(set), err)
	}
	return fmt.Errorf("%w; the %d markers already set were removed", cause, len(set))
}

// Unmark removes marks in reverse order, each only while its object (on a
// fresh snapshot) still carries exactly the mark's value. It runs to the end
// even when ctx is cancelled and returns the joined errors.
func Unmark(ctx context.Context, d Driver, marks []Mark) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	for i := len(marks) - 1; i >= 0; i-- {
		m := marks[i]
		cur, err := d.Snapshot(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if c, ok := find(cur, m.Scope, m.Target); !ok || c.key != m.Value {
			continue // gone, or no longer ours: nothing to remove
		}
		if err := d.UnsetOption(ctx, m.Scope, m.Target, m.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m, err))
		}
	}
	return errors.Join(errs...)
}

// Release removes the markers of project id from the live session(s)
// marked @cairn_s = id: the session marker first (so an interrupted release
// leaves an unmanaged session, which apply never touches), then every
// window and pane of that session whose marker starts with "<id>/". Each
// object is checked on a fresh snapshot (still in that session, still
// carrying exactly that marker) and skipped otherwise; other projects'
// markers and unmarked objects are never touched. It returns how many
// markers were removed.
func Release(ctx context.Context, d Driver, id string) (int, error) {
	snap, err := d.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	type target struct {
		m       Mark
		session string
	}
	var targets []target
	prefix := id + "/"
	for _, s := range snap.Sessions {
		if s.Key != id {
			continue
		}
		targets = append(targets, target{Mark{Scope: "session", Target: s.ID, Name: OptSession, Value: id}, s.ID})
		for _, w := range s.Windows {
			if strings.HasPrefix(w.Key, prefix) {
				targets = append(targets, target{Mark{Scope: "window", Target: w.ID, Name: OptWindow, Value: w.Key}, s.ID})
			}
		}
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				if strings.HasPrefix(p.Key, prefix) {
					targets = append(targets, target{Mark{Scope: "pane", Target: p.ID, Name: OptPane, Value: p.Key}, s.ID})
				}
			}
		}
	}
	if len(targets) == 0 {
		return 0, fmt.Errorf("no live tmux session carries the marker of project %s", id)
	}
	removed := 0
	var errs []error
	for _, t := range targets {
		cur, err := d.Snapshot(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if c, ok := find(cur, t.m.Scope, t.m.Target); !ok || c.sess.ID != t.session || c.key != t.m.Value {
			continue // gone, moved or re-marked meanwhile: not ours to touch
		}
		if err := d.UnsetOption(ctx, t.m.Scope, t.m.Target, t.m.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.m, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
