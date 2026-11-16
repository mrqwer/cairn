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
	pane tmux.Pane
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
					return object{sess: s, win: w, pane: p, key: p.Key}, true
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
// session (and window) under the same session name, a pane at the same
// position and size (no swap-pane or resize in between), and carries no
// cairn marker or exactly m.Value; its session is unmarked or marked with
// m's project. It returns the object as it is now.
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
	case m.Scope == "pane" && (c.pane.Left != o.pane.Left || c.pane.Top != o.pane.Top || c.pane.W != o.pane.W || c.pane.H != o.pane.H):
		return object{}, fmt.Errorf("%s was moved or resized since the capture (%d,%d %dx%d, now %d,%d %dx%d)", m,
			o.pane.Left, o.pane.Top, o.pane.W, o.pane.H, c.pane.Left, c.pane.Top, c.pane.W, c.pane.H)
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
// written). On any failure, and when ctx is cancelled between marks, the
// marks this run set are removed again and the error names the object.
func Apply(ctx context.Context, d Driver, snap tmux.Snapshot, marks []Mark) error {
	ordered := append([]Mark(nil), marks...)
	sort.SliceStable(ordered, func(i, j int) bool { return scopeRank(ordered[i].Scope) < scopeRank(ordered[j].Scope) })
	var set []Mark
	for _, m := range ordered {
		if err := ctx.Err(); err != nil {
			return rollback(ctx, d, set, fmt.Errorf("adopt interrupted: %w", err))
		}
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
		// Recorded first: a set that fails after tmux applied it is rolled
		// back too (Unmark only removes a marker carrying m.Value).
		set = append(set, m)
		if err := d.SetOption(ctx, m.Scope, m.Target, m.Name, m.Value); err != nil {
			return rollback(ctx, d, set, fmt.Errorf("adopt aborted: %s: %w", m, err))
		}
	}
	return nil
}

func rollback(ctx context.Context, d Driver, set []Mark, cause error) error {
	if len(set) == 0 {
		return cause
	}
	if err := Unmark(ctx, d, set); err != nil {
		project, _, _ := strings.Cut(set[0].Value, "/")
		return fmt.Errorf("%w; removing the %d markers already set failed (remove them with: cairn adopt --release %s): %v",
			cause, len(set), project, err)
	}
	return fmt.Errorf("%w; the %d markers already set were removed", cause, len(set))
}

// Unmark removes marks in reverse order, each only while its object (on a
// fresh snapshot) still carries exactly the mark's value. It may therefore
// also remove an identical marker left over from before the run. Session
// markers come last and are kept when removing any window or pane marker
// failed, so the session stays findable by Release. It runs to the end even
// when ctx is cancelled and returns the joined errors.
func Unmark(ctx context.Context, d Driver, marks []Mark) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	unmark := func(m Mark) {
		cur, err := d.Snapshot(ctx)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if c, ok := find(cur, m.Scope, m.Target); !ok || c.key != m.Value {
			return // gone, or no longer ours: nothing to remove
		}
		if err := d.UnsetOption(ctx, m.Scope, m.Target, m.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m, err))
		}
	}
	for i := len(marks) - 1; i >= 0; i-- {
		if marks[i].Scope != "session" {
			unmark(marks[i])
		}
	}
	for i := len(marks) - 1; i >= 0; i-- {
		if marks[i].Scope != "session" {
			continue
		}
		if len(errs) > 0 {
			errs = append(errs, fmt.Errorf("%s kept so that cairn adopt --release %s can find the session", marks[i], marks[i].Value))
			continue
		}
		unmark(marks[i])
	}
	return errors.Join(errs...)
}

// Release removes the markers of project id: on the session(s) marked
// @cairn_s = id the session marker first (so an interrupted release leaves
// an unmanaged session, which apply never touches), then every window and
// pane whose marker starts with "<id>/" in those sessions and in unmarked
// sessions (orphans of an interrupted release or a failed rollback).
// Sessions marked for another project are never looked into. Each object
// is checked on a fresh snapshot (still in the same session, that session
// still unmarked or marked id, still carrying exactly that marker) and
// skipped otherwise; other markers and unmarked objects are never touched.
// It returns how many markers were removed.
func Release(ctx context.Context, d Driver, id string) (int, error) {
	snap, err := d.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	type target struct {
		m       Mark
		session string
	}
	var sessions, windows, panes []target
	prefix := id + "/"
	for _, s := range Sessions(snap, id) {
		if s.Key == id {
			sessions = append(sessions, target{Mark{Scope: "session", Target: s.ID, Name: OptSession, Value: id}, s.ID})
		}
		for _, w := range s.Windows {
			if strings.HasPrefix(w.Key, prefix) {
				windows = append(windows, target{Mark{Scope: "window", Target: w.ID, Name: OptWindow, Value: w.Key}, s.ID})
			}
			for _, p := range w.Panes {
				if strings.HasPrefix(p.Key, prefix) {
					panes = append(panes, target{Mark{Scope: "pane", Target: p.ID, Name: OptPane, Value: p.Key}, s.ID})
				}
			}
		}
	}
	targets := append(append(sessions, windows...), panes...)
	if len(targets) == 0 {
		return 0, fmt.Errorf("no live tmux session carries a marker of project %s", id)
	}
	removed := 0
	var errs []error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("release interrupted (run it again to finish): %w", err))
			break
		}
		cur, err := d.Snapshot(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}
		c, ok := find(cur, t.m.Scope, t.m.Target)
		if !ok || c.sess.ID != t.session || c.key != t.m.Value || (c.sess.Key != "" && c.sess.Key != id) {
			continue // gone, moved or claimed meanwhile: not ours to touch
		}
		if err := d.UnsetOption(ctx, t.m.Scope, t.m.Target, t.m.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.m, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Sessions returns the live sessions Release would act on for project id:
// those marked id, and unmarked ones holding a window or pane marker
// starting with "<id>/".
func Sessions(snap tmux.Snapshot, id string) []tmux.Session {
	prefix := id + "/"
	var out []tmux.Session
	for _, s := range snap.Sessions {
		switch {
		case s.Key == id:
			out = append(out, s)
		case s.Key == "" && hasPrefixedMarker(s, prefix):
			out = append(out, s)
		}
	}
	return out
}

func hasPrefixedMarker(s tmux.Session, prefix string) bool {
	for _, w := range s.Windows {
		if strings.HasPrefix(w.Key, prefix) {
			return true
		}
		for _, p := range w.Panes {
			if strings.HasPrefix(p.Key, prefix) {
				return true
			}
		}
	}
	return false
}
