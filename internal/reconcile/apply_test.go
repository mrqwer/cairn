package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

var _ Driver = tmux.Client{}

// fakeDriver models a tmux server well enough for the reconciler: splits
// insert the new pane right after the target (as tmux does), swaps exchange
// list positions, new windows inherit the session size. Every call is
// recorded.
type fakeDriver struct {
	calls    []string
	sessions []*fSession
	seq      int
	idle     map[int]bool
	fail     map[string]error // call prefix -> error
}

type fSession struct {
	id, name, key string
	w, h          int
	windows       []*fWindow
}

type fWindow struct {
	id, key, name string
	w, h          int
	panes         []*fPane
	layout        string
}

type fPane struct {
	id, key string
	pid     int
	dead    bool
	status  int
	cmd     string
	argv    []string
}

func (f *fakeDriver) next() int { f.seq++; return f.seq }

func (f *fakeDriver) rec(format string, a ...any) error {
	c := fmt.Sprintf(format, a...)
	f.calls = append(f.calls, c)
	for p, err := range f.fail {
		if strings.HasPrefix(c, p) {
			return err
		}
	}
	return nil
}

func (f *fakeDriver) newPane(p tmux.PaneSpec) *fPane {
	n := f.next()
	cmd := "sh"
	if len(p.Argv) > 0 {
		cmd = p.Argv[0]
	}
	return &fPane{id: fmt.Sprintf("%%%d", n), key: p.Key, pid: 100 + n, cmd: cmd, argv: p.Argv}
}

func (f *fakeDriver) findPane(id string) (*fWindow, int) {
	for _, s := range f.sessions {
		for _, w := range s.windows {
			for i, p := range w.panes {
				if p.id == id {
					return w, i
				}
			}
		}
	}
	return nil, -1
}

func (f *fakeDriver) Snapshot(context.Context) (tmux.Snapshot, error) {
	if err := f.rec("Snapshot"); err != nil {
		return tmux.Snapshot{}, err
	}
	var snap tmux.Snapshot
	for _, s := range f.sessions {
		ts := tmux.Session{ID: s.id, Name: s.name, Key: s.key}
		for wi, w := range s.windows {
			tw := tmux.Window{ID: w.id, Key: w.key, Name: w.name, Index: wi, W: w.w, H: w.h}
			for pi, p := range w.panes {
				tw.Panes = append(tw.Panes, tmux.Pane{ID: p.id, Key: p.key, Index: pi, PID: p.pid, Dead: p.dead, DeadStatus: p.status, Cmd: p.cmd})
			}
			ts.Windows = append(ts.Windows, tw)
		}
		snap.Sessions = append(snap.Sessions, ts)
	}
	return snap, nil
}

func (f *fakeDriver) NewSession(_ context.Context, name string, w, h int, win tmux.WindowSpec) (string, string, string, error) {
	if err := f.rec("NewSession %s %dx%d %s %s %s cwd=%s argv=%v remain=%v", name, w, h, win.Key, win.Name,
		win.PaneSpec.Key, win.PaneSpec.Cwd, win.PaneSpec.Argv, win.PaneSpec.RemainOnExit); err != nil {
		return "", "", "", err
	}
	project, _, _ := strings.Cut(win.Key, "/")
	s := &fSession{id: fmt.Sprintf("$%d", f.next()), name: name, key: project, w: w, h: h}
	p := f.newPane(win.PaneSpec)
	wn := &fWindow{id: fmt.Sprintf("@%d", f.next()), key: win.Key, name: win.Name, w: w, h: h, panes: []*fPane{p}}
	s.windows = []*fWindow{wn}
	f.sessions = append(f.sessions, s)
	return s.id, wn.id, p.id, nil
}

func (f *fakeDriver) NewWindow(_ context.Context, sessionID string, win tmux.WindowSpec) (string, string, error) {
	if err := f.rec("NewWindow %s %s %s %s argv=%v", sessionID, win.Key, win.Name, win.PaneSpec.Key, win.PaneSpec.Argv); err != nil {
		return "", "", err
	}
	for _, s := range f.sessions {
		if s.id == sessionID {
			p := f.newPane(win.PaneSpec)
			wn := &fWindow{id: fmt.Sprintf("@%d", f.next()), key: win.Key, name: win.Name, w: s.w, h: s.h, panes: []*fPane{p}}
			s.windows = append(s.windows, wn)
			return wn.id, p.id, nil
		}
	}
	return "", "", fmt.Errorf("no session %s", sessionID)
}

func (f *fakeDriver) SplitPane(_ context.Context, target, dir string, percent int, p tmux.PaneSpec) (string, error) {
	if err := f.rec("SplitPane %s %s %d %s argv=%v", target, dir, percent, p.Key, p.Argv); err != nil {
		return "", err
	}
	w, i := f.findPane(target)
	if w == nil {
		return "", fmt.Errorf("no pane %s", target)
	}
	np := f.newPane(p)
	w.panes = slices.Insert(w.panes, i+1, np)
	return np.id, nil
}

func (f *fakeDriver) SelectLayout(_ context.Context, windowID, s string) error {
	if err := f.rec("SelectLayout %s %s", windowID, s); err != nil {
		return err
	}
	for _, se := range f.sessions {
		for _, w := range se.windows {
			if w.id == windowID {
				w.layout = s
			}
		}
	}
	return nil
}

func (f *fakeDriver) SetOption(_ context.Context, scope, target, name, value string) error {
	return f.rec("SetOption %s %s %s %s", scope, target, name, value)
}

func (f *fakeDriver) RespawnPane(_ context.Context, paneID string, argv []string) error {
	if err := f.rec("RespawnPane %s %v", paneID, argv); err != nil {
		return err
	}
	w, i := f.findPane(paneID)
	if w == nil {
		return fmt.Errorf("no pane %s", paneID)
	}
	w.panes[i].dead, w.panes[i].pid = false, 100+f.next()
	return nil
}

func (f *fakeDriver) SwapPanes(_ context.Context, a, b string) error {
	if err := f.rec("SwapPanes %s %s", a, b); err != nil {
		return err
	}
	wa, i := f.findPane(a)
	wb, j := f.findPane(b)
	if wa == nil || wa != wb {
		return fmt.Errorf("bad swap %s %s", a, b)
	}
	wa.panes[i], wa.panes[j] = wa.panes[j], wa.panes[i]
	return nil
}

func (f *fakeDriver) KillWindow(_ context.Context, id string) error {
	if err := f.rec("KillWindow %s", id); err != nil {
		return err
	}
	for _, s := range f.sessions {
		s.windows = slices.DeleteFunc(s.windows, func(w *fWindow) bool { return w.id == id })
	}
	return nil
}

func (f *fakeDriver) KillPane(_ context.Context, id string) error {
	if err := f.rec("KillPane %s", id); err != nil {
		return err
	}
	w, i := f.findPane(id)
	if w == nil {
		return fmt.Errorf("no pane %s", id)
	}
	w.panes = slices.Delete(w.panes, i, i+1)
	return nil
}

func (f *fakeDriver) PaneIdle(pid int) (bool, error) {
	_ = f.rec("PaneIdle %d", pid)
	return f.idle[pid], nil
}

func (f *fakeDriver) window(key string) *fWindow {
	for _, s := range f.sessions {
		for _, w := range s.windows {
			if w.key == key {
				return w
			}
		}
	}
	return nil
}

func (f *fakeDriver) paneKeys(key string) []string {
	var out []string
	for _, p := range f.window(key).panes {
		out = append(out, p.key)
	}
	return out
}

// plan plans with the fake's current state.
func (f *fakeDriver) plan(t *testing.T, ps []workspace.Project, opts Options) []Op {
	t.Helper()
	snap, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if opts.RootExists == nil {
		opts.RootExists = allRoots
	}
	if opts.PaneIdle == nil {
		opts.PaneIdle = f.PaneIdle
	}
	return Plan(ps, snap, opts)
}

func mustApply(t *testing.T, f *fakeDriver, ps []workspace.Project, ops []Op) {
	t.Helper()
	for _, r := range Apply(context.Background(), f, ps, ops) {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Op.Kind, r.Err)
		}
	}
}

func TestApplyOrderAndMarkers(t *testing.T) {
	demo := devProject("demo")
	f := &fakeDriver{}
	ops := f.plan(t, []workspace.Project{demo}, Options{})
	f.calls = nil
	mustApply(t, f, []workspace.Project{demo}, ops)

	tree, _ := layout.Build(demo.Windows[0].Panes)
	ls, err := layout.TmuxString(tree, 200, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Snapshot",
		"NewSession demo 200x50 demo/code code demo/code/1 cwd=/home/u/demo argv=[nvim] remain=true",
		"SplitPane %2 h 35 demo/code/2 argv=[]",
		"Snapshot",
		"SelectLayout @3 " + ls,
		"NewWindow $1 demo/run run demo/run/1 argv=[]",
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls\n got: %q\nwant: %q", f.calls, want)
	}
	if s := f.sessions[0]; s.key != "demo" || s.name != "demo" {
		t.Errorf("session marker %+v", s)
	}
	if got := f.paneKeys("demo/code"); !slices.Equal(got, []string{"demo/code/1", "demo/code/2"}) {
		t.Errorf("pane markers %v", got)
	}
	if f.window("demo/run") == nil {
		t.Error("run window missing")
	}
	if again := f.plan(t, []workspace.Project{demo}, Options{}); len(again) != 0 {
		t.Fatalf("second plan not empty: %s", kinds(again))
	}
}

// fourPanes: tree h(v(1,3), v(2,4)), traversal order 1,3,2,4.
func fourPanes(id string) workspace.Project {
	p := workspace.Project{ID: id, Root: "/home/u/" + id, Windows: []workspace.Window{{Key: "main", Panes: []workspace.Pane{
		{},
		{Split: "h", Size: 40},
		{Split: "v", From: 1, Size: 30},
		{Split: "v", From: 2, Size: 50},
	}}}}
	p.ApplyDefaults("/home/u")
	return p
}

// Splitting in config order inserts every new pane right after its From
// pane, which is already traversal order: a new window needs no swaps.
func TestApplyNewSessionInTraversalOrder(t *testing.T) {
	p := fourPanes("demo")
	f := &fakeDriver{}
	mustApply(t, f, []workspace.Project{p}, f.plan(t, []workspace.Project{p}, Options{}))
	want := []string{"demo/main/1", "demo/main/3", "demo/main/2", "demo/main/4"}
	if got := f.paneKeys("demo/main"); !slices.Equal(got, want) {
		t.Fatalf("pane order before layout %v want %v", got, want)
	}
	swaps := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "SwapPanes") {
			swaps++
		}
	}
	if swaps != 0 {
		t.Errorf("swaps %d (calls %q)", swaps, f.calls)
	}
	tree, _ := layout.Build(p.Windows[0].Panes)
	ls, _ := layout.TmuxString(tree, 200, 50)
	if w := f.window("demo/main"); w.layout != ls {
		t.Errorf("layout %q want %q", w.layout, ls)
	}
}

func TestApplyRecreatesMiddlePaneAndReorders(t *testing.T) {
	p := fourPanes("demo")
	f := &fakeDriver{}
	mustApply(t, f, []workspace.Project{p}, f.plan(t, []workspace.Project{p}, Options{}))
	w := f.window("demo/main")
	w.panes = slices.DeleteFunc(w.panes, func(fp *fPane) bool { return fp.key == "demo/main/2" })
	w.layout = ""

	ops := f.plan(t, []workspace.Project{p}, Options{})
	if got := kinds(ops); got != "demo:create-pane:main:demo/main/2 demo:set-layout:main" {
		t.Fatalf("ops %s", got)
	}
	f.calls = nil
	mustApply(t, f, []workspace.Project{p}, ops)
	if !slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "SwapPanes") }) {
		t.Errorf("expected a swap: %q", f.calls)
	}
	if !strings.HasPrefix(f.calls[1], "SplitPane "+w.panes[0].id+" h 40 demo/main/2") {
		t.Errorf("pane 2 must be split from pane 1: %q", f.calls)
	}
	want := []string{"demo/main/1", "demo/main/3", "demo/main/2", "demo/main/4"}
	if got := f.paneKeys("demo/main"); !slices.Equal(got, want) {
		t.Fatalf("pane order %v want %v", got, want)
	}
	tree, _ := layout.Build(p.Windows[0].Panes)
	ls, _ := layout.TmuxString(tree, 200, 50)
	if w.layout != ls {
		t.Errorf("layout %q want %q", w.layout, ls)
	}
	if again := f.plan(t, []workspace.Project{p}, Options{}); len(again) != 0 {
		t.Fatalf("second plan not empty: %s", kinds(again))
	}
}

func TestApplyRespawnUsesConfigArgv(t *testing.T) {
	demo := devProject("demo")
	f := &fakeDriver{}
	mustApply(t, f, []workspace.Project{demo}, f.plan(t, []workspace.Project{demo}, Options{}))
	ed := f.window("demo/code").panes[0]
	ed.dead, ed.status = true, 1
	ops := f.plan(t, []workspace.Project{demo}, Options{})
	f.calls = nil
	mustApply(t, f, []workspace.Project{demo}, ops)
	if !slices.Contains(f.calls, "RespawnPane "+ed.id+" [nvim]") {
		t.Fatalf("calls %q", f.calls)
	}
	if ed.dead {
		t.Error("pane still dead")
	}
}

func TestApplyUnmanagedPaneSkipsLayout(t *testing.T) {
	demo := devProject("demo")
	f := &fakeDriver{}
	mustApply(t, f, []workspace.Project{demo}, f.plan(t, []workspace.Project{demo}, Options{}))
	w := f.window("demo/code")
	w.panes[1] = &fPane{id: "%99", pid: 999, cmd: "sh"} // the user's pane replaces the shell pane
	w.layout = ""
	ops := f.plan(t, []workspace.Project{demo}, Options{Prune: true})
	if got := kinds(ops); got != "demo:create-pane:code:demo/code/2 demo:skip-layout:code" {
		t.Fatalf("ops %s", got)
	}
	f.calls = nil
	mustApply(t, f, []workspace.Project{demo}, ops)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "SelectLayout") || strings.HasPrefix(c, "Kill") || strings.HasPrefix(c, "SwapPanes") {
			t.Errorf("unexpected %s", c)
		}
	}
	if len(w.panes) != 3 || !slices.ContainsFunc(w.panes, func(p *fPane) bool { return p.id == "%99" }) {
		t.Errorf("user pane must stay: %v", f.paneKeys("demo/code"))
	}
}

func TestApplyPruneRechecksIdle(t *testing.T) {
	demo := devProject("demo")
	f := &fakeDriver{idle: map[int]bool{}}
	mustApply(t, f, []workspace.Project{demo}, f.plan(t, []workspace.Project{demo}, Options{}))
	w := f.window("demo/code")
	extra := &fPane{id: "%90", key: "demo/code/3", pid: 900, cmd: "zsh"}
	w.panes = append(w.panes, extra)
	f.idle[900] = true
	ops := f.plan(t, []workspace.Project{demo}, Options{Prune: true})
	if got := kinds(ops); got != "demo:kill-pane:code:demo/code/3 demo:set-layout:code" {
		t.Fatalf("ops %s", got)
	}
	// The shell started a program between plan and apply.
	f.idle[900] = false
	res := Apply(context.Background(), f, []workspace.Project{demo}, ops)
	if res[0].Err == nil || !errors.Is(res[1].Err, ErrNotRun) {
		t.Fatalf("results %+v", res)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "KillPane") }) {
		t.Fatal("a busy pane was killed")
	}
	f.idle[900] = true
	f.calls = nil
	mustApply(t, f, []workspace.Project{demo}, ops)
	if got := f.paneKeys("demo/code"); !slices.Equal(got, []string{"demo/code/1", "demo/code/2"}) {
		t.Fatalf("panes %v", got)
	}
}

func TestApplyPruneKillsIdleWindow(t *testing.T) {
	demo := devProject("demo")
	f := &fakeDriver{idle: map[int]bool{}}
	mustApply(t, f, []workspace.Project{demo}, f.plan(t, []workspace.Project{demo}, Options{}))
	s := f.sessions[0]
	s.windows = append(s.windows, &fWindow{id: "@90", key: "demo/old", w: 200, h: 50,
		panes: []*fPane{{id: "%91", key: "demo/old/1", pid: 901, cmd: "bash"}}})
	f.idle[901] = true
	ops := f.plan(t, []workspace.Project{demo}, Options{Prune: true})
	if got := kinds(ops); got != "demo:kill-window:old" {
		t.Fatalf("ops %s", got)
	}
	mustApply(t, f, []workspace.Project{demo}, ops)
	if f.window("demo/old") != nil {
		t.Fatal("window not killed")
	}
}

func TestApplyStopsProjectAfterErrorAndContinuesOthers(t *testing.T) {
	demo, web := devProject("demo"), devProject("web")
	f := &fakeDriver{}
	ps := []workspace.Project{web, demo}
	ops := f.plan(t, ps, Options{})
	f.fail = map[string]error{"NewSession demo": errors.New("boom")}
	res := Apply(context.Background(), f, ps, ops)
	if len(res) != 2 {
		t.Fatalf("results %+v", res)
	}
	if res[0].Op.Project != "demo" || res[0].Err == nil {
		t.Errorf("demo should fail: %+v", res[0])
	}
	if res[1].Op.Project != "web" || res[1].Err != nil {
		t.Errorf("web should apply: %+v", res[1])
	}
	if f.window("web/run") == nil {
		t.Error("web not created")
	}

	// A failing op stops the rest of that project.
	f2 := &fakeDriver{}
	p := devProject("demo")
	mustApply(t, f2, []workspace.Project{p}, f2.plan(t, []workspace.Project{p}, Options{}))
	f2.sessions[0].windows = f2.sessions[0].windows[:1]
	f2.window("demo/code").panes[0].dead = true
	f2.window("demo/code").panes[0].status = 1
	ops = f2.plan(t, []workspace.Project{p}, Options{})
	if got := kinds(ops); got != "demo:respawn-pane:code:demo/code/1 demo:create-window:run" {
		t.Fatalf("ops %s", got)
	}
	f2.fail = map[string]error{"RespawnPane": errors.New("boom")}
	res = Apply(context.Background(), f2, []workspace.Project{p}, ops)
	if res[0].Err == nil || !errors.Is(res[1].Err, ErrNotRun) {
		t.Fatalf("results %+v", res)
	}
	if f2.window("demo/run") != nil {
		t.Fatal("ran an op after the project's first failure")
	}
}

func TestApplyNotesDoNothing(t *testing.T) {
	f := &fakeDriver{}
	ops := []Op{
		{Kind: KindSkipMissingRoot, Project: "demo"},
		{Kind: KindSkipNameClash, Project: "web"},
	}
	res := Apply(context.Background(), f, nil, ops)
	if len(res) != 2 || res[0].Err != nil || res[1].Err != nil || len(f.calls) != 0 {
		t.Fatalf("results %+v calls %q", res, f.calls)
	}
}
