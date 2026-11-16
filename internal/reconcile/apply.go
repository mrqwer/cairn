package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

// Driver is the part of tmux.Client the reconciler uses.
type Driver interface {
	Snapshot(ctx context.Context) (tmux.Snapshot, error)
	NewSession(ctx context.Context, name string, w, h int, win tmux.WindowSpec) (sessionID, windowID, paneID string, err error)
	NewWindow(ctx context.Context, sessionID string, win tmux.WindowSpec) (windowID, paneID string, err error)
	SplitPane(ctx context.Context, targetPane, dir string, percent int, p tmux.PaneSpec) (paneID string, err error)
	SelectLayout(ctx context.Context, windowID, layoutString string) error
	SetOption(ctx context.Context, scope, target, name, value string) error
	RespawnPane(ctx context.Context, paneID string, argv []string) error
	SwapPanes(ctx context.Context, paneA, paneB string) error
	KillWindow(ctx context.Context, id string) error
	KillPane(ctx context.Context, id string) error
	PaneIdle(pid int) (bool, error)
}

// Result is the outcome of one op.
type Result struct {
	Op  Op
	Err error
}

// ErrNotRun marks the ops of a project that were not run because an earlier
// op of the same project failed.
var ErrNotRun = errors.New("not run: an earlier operation of this project failed")

// Apply runs ops in order. Notes (report-*, skip-*) do nothing. After the
// first failing op of a project its remaining ops get ErrNotRun; other
// projects continue. Every op gets a Result.
func Apply(ctx context.Context, d Driver, ps []workspace.Project, ops []Op) []Result {
	byID := map[string]workspace.Project{}
	for _, p := range ps {
		byID[p.ID] = p
	}
	failed := map[string]bool{}
	states := map[string]*applier{}
	results := make([]Result, 0, len(ops))
	for _, op := range ops {
		if failed[op.Project] {
			results = append(results, Result{op, ErrNotRun})
			continue
		}
		if op.IsNote() {
			results = append(results, Result{op, nil})
			continue
		}
		err := func() error {
			a := states[op.Project]
			if a == nil {
				p, ok := byID[op.Project]
				if !ok {
					return fmt.Errorf("project %s is not in the config", op.Project)
				}
				var err error
				if a, err = newApplier(ctx, d, p); err != nil {
					return err
				}
				states[op.Project] = a
			}
			return a.do(ctx, op)
		}()
		if err != nil {
			failed[op.Project] = true
		}
		results = append(results, Result{op, err})
	}
	return results
}

// applier runs the ops of one project and remembers the tmux ids of its
// session, windows (by config key) and panes (by @cairn_p key).
type applier struct {
	d       Driver
	dp      DesiredProject
	session string
	windows map[string]string
	panes   map[string]string
}

func newApplier(ctx context.Context, d Driver, p workspace.Project) (*applier, error) {
	a := &applier{d: d, dp: Desired(p, NewSize, func(string) bool { return true }),
		windows: map[string]string{}, panes: map[string]string{}}
	snap, err := d.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range snap.Sessions {
		if s.Key != a.dp.ID {
			continue
		}
		a.session = s.ID
		for _, w := range s.Windows {
			k, ok := strings.CutPrefix(w.Key, a.dp.ID+"/")
			if !ok {
				continue
			}
			if _, seen := a.windows[k]; !seen {
				a.windows[k] = w.ID
			}
			for _, p := range w.Panes {
				if _, seen := a.panes[p.Key]; p.Key != "" && !seen {
					a.panes[p.Key] = p.ID
				}
			}
		}
		break
	}
	return a, nil
}

func (a *applier) window(key string) (DesiredWindow, error) {
	w, ok := a.dp.window(key)
	if !ok {
		return DesiredWindow{}, fmt.Errorf("window %s is not in the config of %s", key, a.dp.ID)
	}
	return w, nil
}

func (a *applier) do(ctx context.Context, op Op) error {
	switch op.Kind {
	case KindCreateSession:
		return a.createSession(ctx)
	case KindCreateWindow:
		w, err := a.window(op.WindowKey)
		if err != nil {
			return err
		}
		return a.createWindow(ctx, w)
	case KindCreatePane:
		return a.createPane(ctx, op)
	case KindSetLayout:
		w, err := a.window(op.WindowKey)
		if err != nil {
			return err
		}
		return a.setLayout(ctx, w, true)
	case KindRespawnPane:
		w, err := a.window(op.WindowKey)
		if err != nil {
			return err
		}
		dp, ok := w.pane(op.PaneKey)
		if !ok || !dp.HasRun() {
			return fmt.Errorf("pane %s has no configured command", op.PaneKey)
		}
		return a.d.RespawnPane(ctx, op.Target, dp.Spec.Argv)
	case KindKillPane, KindKillWindow:
		return a.kill(ctx, op)
	}
	return fmt.Errorf("unknown op kind %q", op.Kind)
}

// createSession creates the session with all its windows and panes, laid
// out, at NewSize.
func (a *applier) createSession(ctx context.Context) error {
	if a.session != "" {
		return fmt.Errorf("session of %s already exists (%s)", a.dp.ID, a.session)
	}
	first := a.dp.Windows[0]
	sid, wid, pid, err := a.d.NewSession(ctx, a.dp.Session, NewSize.W, NewSize.H, first.Spec)
	if err != nil {
		return err
	}
	a.session = sid
	if err := a.buildWindow(ctx, first, wid, pid); err != nil {
		return err
	}
	for _, w := range a.dp.Windows[1:] {
		if err := a.createWindow(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

func (a *applier) createWindow(ctx context.Context, w DesiredWindow) error {
	if a.session == "" {
		return fmt.Errorf("session of %s does not exist", a.dp.ID)
	}
	wid, pid, err := a.d.NewWindow(ctx, a.session, w.Spec)
	if err != nil {
		return err
	}
	return a.buildWindow(ctx, w, wid, pid)
}

// buildWindow splits the remaining panes of a new window in config order
// and applies the layout. A layout that does not fit, or a pane that is
// already gone (a command that exited before remain-on-exit was set,
// decision E11), leaves the window as split; the next apply finishes it.
func (a *applier) buildWindow(ctx context.Context, w DesiredWindow, wid, firstPane string) error {
	a.windows[w.Key] = wid
	a.panes[w.Panes[0].Key] = firstPane
	for _, dp := range w.Panes[1:] {
		id, err := a.d.SplitPane(ctx, a.panes[w.Panes[dp.From-1].Key], dp.Split, dp.Size, dp.Spec)
		if err != nil {
			return err
		}
		a.panes[dp.Key] = id
	}
	return a.setLayout(ctx, w, false)
}

// createPane recreates a missing pane of an existing window by splitting
// its From pane (or, when that is gone too or it is the first pane, any
// pane of the window); set-layout puts it in place afterwards.
func (a *applier) createPane(ctx context.Context, op Op) error {
	w, err := a.window(op.WindowKey)
	if err != nil {
		return err
	}
	dp, ok := w.pane(op.PaneKey)
	if !ok {
		return fmt.Errorf("pane %s is not in the config", op.PaneKey)
	}
	wid := a.windows[w.Key]
	if wid == "" {
		return fmt.Errorf("window %s does not exist", w.FullKey())
	}
	dir, size, target := dp.Split, dp.Size, ""
	if dp.From > 0 {
		target = a.panes[w.Panes[dp.From-1].Key]
	} else {
		dir, size = workspace.SplitH, workspace.DefaultSize
	}
	if target == "" {
		snap, err := a.d.Snapshot(ctx)
		if err != nil {
			return err
		}
		if tw, ok := findWindow(snap, wid); ok && len(tw.Panes) > 0 {
			target = tw.Panes[len(tw.Panes)-1].ID
		}
	}
	if target == "" {
		return fmt.Errorf("window %s has no pane to split", w.FullKey())
	}
	id, err := a.d.SplitPane(ctx, target, dir, size, dp.Spec)
	if err != nil {
		return err
	}
	a.panes[dp.Key] = id
	return nil
}

// setLayout reads the window from a fresh snapshot, orders its panes by
// layout traversal with SwapPanes (decision E6) and selects the layout for
// the window's current size. It only acts when the window holds exactly the
// configured panes; otherwise, or when the layout does not fit, strict
// returns an error and non-strict returns nil.
func (a *applier) setLayout(ctx context.Context, w DesiredWindow, strict bool) error {
	if len(w.Panes) < 2 && !strict {
		return nil
	}
	skip := func(format string, args ...any) error {
		if !strict {
			return nil
		}
		return fmt.Errorf("window %s: layout not applied: %s", w.FullKey(), fmt.Sprintf(format, args...))
	}
	snap, err := a.d.Snapshot(ctx)
	if err != nil {
		return err
	}
	tw, ok := findWindow(snap, a.windows[w.Key])
	if !ok {
		return skip("window is gone")
	}
	current := make([]string, 0, len(tw.Panes))
	ids := map[string]string{}
	for _, p := range tw.Panes {
		if _, ok := w.pane(p.Key); !ok {
			return skip("pane %s (%s) is not in the config", p.Key, p.ID)
		}
		if _, dup := ids[p.Key]; dup {
			return skip("pane %s appears twice", p.Key)
		}
		ids[p.Key] = p.ID
		current = append(current, p.Key)
	}
	if len(current) != len(w.Panes) {
		return skip("%d of %d panes exist", len(current), len(w.Panes))
	}
	s, err := w.LayoutFor(layout.Size{W: tw.W, H: tw.H})
	if err != nil {
		return skip("%v", err)
	}
	// Selection sort: at most n-1 swaps.
	for i, want := range w.Order() {
		if current[i] == want {
			continue
		}
		j := i + 1
		for current[j] != want {
			j++
		}
		if err := a.d.SwapPanes(ctx, ids[current[i]], ids[current[j]]); err != nil {
			return err
		}
		current[i], current[j] = current[j], current[i]
	}
	return a.d.SelectLayout(ctx, tw.ID, s)
}

// kill re-checks, on a fresh snapshot, that every pane involved still
// carries the expected marker and is an idle shell, then kills.
func (a *applier) kill(ctx context.Context, op Op) error {
	snap, err := a.d.Snapshot(ctx)
	if err != nil {
		return err
	}
	var panes []tmux.Pane
	if op.Kind == KindKillWindow {
		tw, ok := findWindow(snap, op.Target)
		if !ok || tw.Key != a.dp.ID+"/"+op.WindowKey {
			return fmt.Errorf("window %s (%s) changed since the plan; not killed", op.WindowKey, op.Target)
		}
		panes = tw.Panes
	} else {
		tp, ok := findPane(snap, op.Target)
		if !ok || tp.Key != op.PaneKey {
			return fmt.Errorf("pane %s (%s) changed since the plan; not killed", op.PaneKey, op.Target)
		}
		panes = []tmux.Pane{tp}
	}
	if len(panes) == 0 || !allPrunable(panes, Options{PaneIdle: a.d.PaneIdle}) {
		return fmt.Errorf("%s %s is no longer an idle shell; not killed", strings.TrimPrefix(op.Kind, "kill-"), op.Target)
	}
	if op.Kind == KindKillWindow {
		err = a.d.KillWindow(ctx, op.Target)
	} else {
		err = a.d.KillPane(ctx, op.Target)
	}
	if err == nil {
		for k, id := range a.panes {
			for _, p := range panes {
				if p.ID == id {
					delete(a.panes, k)
				}
			}
		}
	}
	return err
}

func findWindow(snap tmux.Snapshot, id string) (tmux.Window, bool) {
	for _, s := range snap.Sessions {
		for _, w := range s.Windows {
			if w.ID == id {
				return w, true
			}
		}
	}
	return tmux.Window{}, false
}

func findPane(snap tmux.Snapshot, id string) (tmux.Pane, bool) {
	for _, s := range snap.Sessions {
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				if p.ID == id {
					return p, true
				}
			}
		}
	}
	return tmux.Pane{}, false
}
