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

// Result is the outcome of one op. Note is set when the op was skipped
// because the state it would create appeared meanwhile (another process
// applied first); that is not a failure.
type Result struct {
	Op   Op
	Err  error
	Note string
}

// skipped is returned by an applier step that found its work already done.
type skipped string

func (s skipped) Error() string { return string(s) }

// ErrNotRun marks the ops of a project that were not run because an earlier
// op of the same project failed.
var ErrNotRun = errors.New("not run: an earlier operation of this project failed")

// Apply runs ops in order. Notes (report-*, skip-*) do nothing. After the
// first failing op of a project its remaining ops get ErrNotRun; other
// projects continue. Once ctx is done every remaining op gets ctx.Err().
// Every op gets a Result. Projects must be defaulted and valid; an invalid
// one fails its ops with an error.
//
// Safety (decision E13): every call that targets an existing tmux object is
// preceded by a lookup on a fresh snapshot (locate) that checks the object
// still sits in this project's marked session and window and carries the
// expected marker; destructive ops are refused for anything in the config.
func Apply(ctx context.Context, d Driver, ps []workspace.Project, ops []Op) []Result {
	byID := map[string]workspace.Project{}
	for _, p := range ps {
		byID[p.ID] = p
	}
	failed := map[string]bool{}
	states := map[string]*applier{}
	results := make([]Result, 0, len(ops))
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			results = append(results, Result{Op: op, Err: err})
			continue
		}
		if failed[op.Project] {
			results = append(results, Result{Op: op, Err: ErrNotRun})
			continue
		}
		if op.IsNote() {
			results = append(results, Result{Op: op})
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
		var sk skipped
		if errors.As(err, &sk) {
			results = append(results, Result{Op: op, Note: string(sk)})
			continue
		}
		if err != nil {
			failed[op.Project] = true
		}
		results = append(results, Result{Op: op, Err: err})
	}
	return results
}

// applier runs the ops of one project and remembers the tmux ids of its
// session, windows (by config key) and panes (by @cairn_p key). The ids are
// only hints: every use goes through locate on a fresh snapshot.
type applier struct {
	d       Driver
	dp      DesiredProject
	session string
	windows map[string]string
	panes   map[string]string
}

func newApplier(ctx context.Context, d Driver, p workspace.Project) (*applier, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
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
				if _, seen := a.panes[p.Key]; strings.HasPrefix(p.Key, w.Key+"/") && !seen {
					a.panes[p.Key] = p.ID
				}
			}
		}
		break
	}
	return a, nil
}

// location is an object found by locate with what contains it.
type location struct {
	s tmux.Session
	w tmux.Window
	p tmux.Pane
}

// locate finds id in snap ("@n" a window, "%n" a pane) and checks that it is
// still where the op expects it: in the session marked project, in the
// window marked project/windowKey and, for a pane, carrying exactly the
// marker paneKey. When sessionID is not empty the session must also be that
// very tmux session, so a second session carrying the project marker is never
// mistaken for it. Anything else is an error and nothing may be done to the
// object.
func locate(snap tmux.Snapshot, project, sessionID, windowKey, paneKey, id string) (location, error) {
	wantWindow := project + "/" + windowKey
	for _, s := range snap.Sessions {
		if sessionID != "" && s.ID != sessionID {
			// An object that sits in another session is reported as such.
			for _, w := range s.Windows {
				if w.ID == id {
					return location{}, fmt.Errorf("window %s is not in session %s of %s; not touched", id, sessionID, project)
				}
				for _, p := range w.Panes {
					if p.ID == id {
						return location{}, fmt.Errorf("pane %s is not in session %s of %s; not touched", id, sessionID, project)
					}
				}
			}
			continue
		}
		for _, w := range s.Windows {
			if strings.HasPrefix(id, "@") && w.ID == id {
				if s.Key != project || w.Key != wantWindow {
					return location{}, fmt.Errorf("window %s is no longer %s in the session of %s; not touched", id, wantWindow, project)
				}
				return location{s: s, w: w}, nil
			}
			for _, p := range w.Panes {
				if !strings.HasPrefix(id, "%") || p.ID != id {
					continue
				}
				if s.Key != project || w.Key != wantWindow || p.Key != paneKey {
					return location{}, fmt.Errorf("pane %s is no longer %q in window %s of %s; not touched", id, paneKey, wantWindow, project)
				}
				return location{s: s, w: w, p: p}, nil
			}
		}
	}
	return location{}, fmt.Errorf("%s (%s) no longer exists", id, wantWindow)
}

// fresh takes a snapshot and locates id in it.
func (a *applier) fresh(ctx context.Context, windowKey, paneKey, id string) (location, error) {
	if id == "" {
		return location{}, fmt.Errorf("%s/%s: no tmux id", a.dp.ID, windowKey)
	}
	snap, err := a.d.Snapshot(ctx)
	if err != nil {
		return location{}, err
	}
	return locate(snap, a.dp.ID, a.session, windowKey, paneKey, id)
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
		return a.respawn(ctx, op)
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

// createWindow adds a window to the project's session after checking, on a
// fresh snapshot, that the session still carries the project's marker.
func (a *applier) createWindow(ctx context.Context, w DesiredWindow) error {
	snap, err := a.d.Snapshot(ctx)
	if err != nil {
		return err
	}
	ok := false
	for _, s := range snap.Sessions {
		if a.session == "" || s.ID != a.session || s.Key != a.dp.ID {
			continue
		}
		ok = true
		for _, tw := range s.Windows {
			if tw.Key == w.FullKey() {
				// Another cairn built it between plan and now.
				a.windows[w.Key] = tw.ID
				return skipped(fmt.Sprintf("window %s already exists (%s); not created twice", w.FullKey(), tw.ID))
			}
		}
	}
	if !ok {
		return fmt.Errorf("session of %s (%s) is gone or no longer marked; window %s not created", a.dp.ID, a.session, w.Key)
	}
	wid, pid, err := a.d.NewWindow(ctx, a.session, w.Spec)
	if err != nil {
		return err
	}
	return a.buildWindow(ctx, w, wid, pid)
}

// buildWindow splits the remaining panes of a new window in config order
// and applies the layout. Panes are created as shells and their command is
// started only after their options and markers are set, so a command that
// exits at once leaves a marked dead pane rather than a vanished one. A
// layout that does not fit leaves the window as split; the next apply
// finishes it.
func (a *applier) buildWindow(ctx context.Context, w DesiredWindow, wid, firstPane string) error {
	a.windows[w.Key] = wid
	a.panes[w.Panes[0].Key] = firstPane
	for _, dp := range w.Panes[1:] {
		from := w.Panes[dp.From-1].Key
		if err := a.split(ctx, w, from, a.panes[from], dp.Split, dp.Size, dp); err != nil {
			return err
		}
	}
	return a.setLayout(ctx, w, false)
}

// split splits the pane id (expected to carry marker targetKey, "" for an
// unmarked pane) of window w after re-checking it on a fresh snapshot.
func (a *applier) split(ctx context.Context, w DesiredWindow, targetKey, id, dir string, size int, dp DesiredPane) error {
	if _, err := a.fresh(ctx, w.Key, targetKey, id); err != nil {
		return err
	}
	nid, err := a.d.SplitPane(ctx, id, dir, size, dp.Spec)
	if err != nil {
		return err
	}
	a.panes[dp.Key] = nid
	return nil
}

// createPane recreates a missing pane of an existing window by splitting
// its From pane. When that is gone too (or it is the first pane) it splits
// the window's last managed pane: the new pane then sits among the managed
// ones, and splitting a user's pane would shrink their space. Only a window
// with no managed pane at all falls back to its last pane. set-layout puts
// the new pane in place afterwards.
func (a *applier) createPane(ctx context.Context, op Op) error {
	w, err := a.window(op.WindowKey)
	if err != nil {
		return err
	}
	dp, ok := w.pane(op.PaneKey)
	if !ok {
		return fmt.Errorf("pane %s is not in the config", op.PaneKey)
	}
	loc, err := a.fresh(ctx, w.Key, "", a.windows[w.Key])
	if err != nil {
		return err
	}
	for _, p := range loc.w.Panes {
		if p.Key == dp.Key {
			return skipped(fmt.Sprintf("pane %s already exists (%s); not created twice", dp.Key, p.ID))
		}
	}
	dir, size := dp.Split, dp.Size
	if dp.From == 0 {
		dir, size = workspace.SplitH, workspace.DefaultSize
	}
	var target tmux.Pane
	found := false
	if dp.From > 0 {
		fromKey := w.Panes[dp.From-1].Key
		for _, p := range loc.w.Panes {
			if p.Key == fromKey && p.ID == a.panes[fromKey] {
				target, found = p, true
			}
		}
	}
	for i := len(loc.w.Panes) - 1; i >= 0 && !found; i-- {
		if _, managed := w.pane(loc.w.Panes[i].Key); managed {
			target, found = loc.w.Panes[i], true
		}
	}
	if !found && len(loc.w.Panes) > 0 {
		target, found = loc.w.Panes[len(loc.w.Panes)-1], true
	}
	if !found {
		return fmt.Errorf("window %s has no pane to split", w.FullKey())
	}
	return a.split(ctx, w, target.Key, target.ID, dir, size, dp)
}

// setLayout reads the window from a fresh snapshot, orders its panes by
// layout traversal with SwapPanes (decision E6) and selects the layout for
// the window's current size. It only acts when the window, in the project's
// marked session, holds exactly the configured panes; otherwise, or when
// the layout does not fit, strict returns an error and non-strict returns
// nil.
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
	loc, err := a.fresh(ctx, w.Key, "", a.windows[w.Key])
	if err != nil {
		return skip("%v", err)
	}
	tw := loc.w
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

// respawn restarts a configured pane only while it is still dead and still
// where the plan saw it; a live program is never restarted.
func (a *applier) respawn(ctx context.Context, op Op) error {
	w, err := a.window(op.WindowKey)
	if err != nil {
		return err
	}
	dp, ok := w.pane(op.PaneKey)
	if !ok || !dp.HasRun() {
		return fmt.Errorf("pane %s has no configured command", op.PaneKey)
	}
	loc, err := a.fresh(ctx, op.WindowKey, op.PaneKey, op.Target)
	if err != nil {
		return err
	}
	if !loc.p.Dead {
		return fmt.Errorf("pane %s (%s) is running again; not restarted", op.PaneKey, op.Target)
	}
	return a.d.RespawnPane(ctx, op.Target, dp.Spec.Argv)
}

// kill re-checks, on a fresh snapshot, that the window or pane is not in the
// config, is still in this project's marked session and window with its
// marker, and that every pane involved carries this window's marker and is
// an idle shell; only then it kills.
func (a *applier) kill(ctx context.Context, op Op) error {
	var panes []tmux.Pane
	if op.Kind == KindKillWindow {
		if _, configured := a.dp.window(op.WindowKey); configured {
			return fmt.Errorf("window %s is in the config; not killed", op.WindowKey)
		}
		loc, err := a.fresh(ctx, op.WindowKey, "", op.Target)
		if err != nil {
			return err
		}
		panes = loc.w.Panes
	} else {
		if w, ok := a.dp.window(op.WindowKey); ok {
			if _, configured := w.pane(op.PaneKey); configured {
				return fmt.Errorf("pane %s is in the config; not killed", op.PaneKey)
			}
		}
		if !strings.HasPrefix(op.PaneKey, a.dp.ID+"/"+op.WindowKey+"/") {
			return fmt.Errorf("pane %s does not belong to window %s; not killed", op.PaneKey, op.WindowKey)
		}
		loc, err := a.fresh(ctx, op.WindowKey, op.PaneKey, op.Target)
		if err != nil {
			return err
		}
		if len(loc.w.Panes) < 2 {
			return fmt.Errorf("pane %s is the last pane of its window; not killed", op.PaneKey)
		}
		panes = []tmux.Pane{loc.p}
	}
	prefix := a.dp.ID + "/" + op.WindowKey + "/"
	opts := Options{PaneIdle: a.d.PaneIdle}
	for _, p := range panes {
		if !strings.HasPrefix(p.Key, prefix) {
			return fmt.Errorf("%s holds pane %s (%q) that cairn does not own here; not killed", op.Target, p.ID, p.Key)
		}
		if !prunable(p, opts) {
			return fmt.Errorf("pane %s (%s) is no longer an idle shell; not killed", p.Key, p.ID)
		}
	}
	if len(panes) == 0 {
		return fmt.Errorf("%s has no panes; not killed", op.Target)
	}
	var err error
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
