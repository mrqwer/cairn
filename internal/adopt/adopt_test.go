package adopt

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/capture"
	"github.com/mrqwer/cairn/internal/reconcile"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

// fakeDriver is a tmux server model: Snapshot returns a deep copy, set and
// unset change the markers, every call is recorded. before runs ahead of
// the n-th Snapshot (1-based) and may change the server; failSet makes the
// n-th SetOption fail.
type fakeDriver struct {
	snap    tmux.Snapshot
	calls   []string
	snaps   int
	sets    int
	before  func(n int, s *tmux.Snapshot)
	failSet int
	// failSetApplied: the failing set still changes the option (tmux
	// applied it, then reported an error).
	failSetApplied bool
	failUnset      string
	afterSet       func()
}

func clone(s tmux.Snapshot) tmux.Snapshot {
	out := tmux.Snapshot{Skipped: slices.Clone(s.Skipped)}
	for _, se := range s.Sessions {
		ws := make([]tmux.Window, 0, len(se.Windows))
		for _, w := range se.Windows {
			w.Panes = slices.Clone(w.Panes)
			ws = append(ws, w)
		}
		se.Windows = ws
		out.Sessions = append(out.Sessions, se)
	}
	return out
}

func (f *fakeDriver) Snapshot(context.Context) (tmux.Snapshot, error) {
	f.snaps++
	f.calls = append(f.calls, "Snapshot")
	if f.before != nil {
		f.before(f.snaps, &f.snap)
	}
	return clone(f.snap), nil
}

// key returns a pointer to the marker of object target at scope.
func (f *fakeDriver) key(scope, target string) *string {
	for si := range f.snap.Sessions {
		s := &f.snap.Sessions[si]
		if scope == "session" && s.ID == target {
			return &s.Key
		}
		for wi := range s.Windows {
			w := &s.Windows[wi]
			if scope == "window" && w.ID == target {
				return &w.Key
			}
			for pi := range w.Panes {
				if scope == "pane" && w.Panes[pi].ID == target {
					return &w.Panes[pi].Key
				}
			}
		}
	}
	return nil
}

func (f *fakeDriver) SetOption(_ context.Context, scope, target, name, value string) error {
	f.calls = append(f.calls, fmt.Sprintf("set %s %s %s %s", scope, target, name, value))
	f.sets++
	if f.afterSet != nil {
		defer f.afterSet()
	}
	k := f.key(scope, target)
	if f.sets == f.failSet {
		if f.failSetApplied && k != nil {
			*k = value
		}
		return errors.New("tmux set: boom")
	}
	if k == nil {
		return fmt.Errorf("no %s %s", scope, target)
	}
	*k = value
	return nil
}

func (f *fakeDriver) UnsetOption(_ context.Context, scope, target, name string) error {
	f.calls = append(f.calls, fmt.Sprintf("unset %s %s %s", scope, target, name))
	if target == f.failUnset {
		return errors.New("tmux set -u: boom")
	}
	k := f.key(scope, target)
	if k == nil {
		return fmt.Errorf("no %s %s", scope, target)
	}
	*k = ""
	return nil
}

// mutations returns the recorded set/unset calls.
func (f *fakeDriver) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if c != "Snapshot" {
			out = append(out, c)
		}
	}
	return out
}

// markers lists every non-empty marker of the model.
func (f *fakeDriver) markers() []string {
	var out []string
	for _, s := range f.snap.Sessions {
		if s.Key != "" {
			out = append(out, s.ID+"="+s.Key)
		}
		for _, w := range s.Windows {
			if w.Key != "" {
				out = append(out, w.ID+"="+w.Key)
			}
			for _, p := range w.Panes {
				if p.Key != "" {
					out = append(out, p.ID+"="+p.Key)
				}
			}
		}
	}
	return out
}

// liveWindow lays cfg out like tmux: rects from layout.Geometry, panes
// listed in traversal order (pane_index) with the given ids and cwds (by
// config position).
func liveWindow(t *testing.T, id, name string, cfg []workspace.Pane, ids, cwds []string) tmux.Window {
	t.Helper()
	tree, err := layout.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rects := layout.Geometry(tree, 200, 50)
	w := tmux.Window{ID: id, Name: name, W: 200, H: 50}
	for idx, pos := range layout.Order(tree) {
		r := rects[pos-1]
		w.Panes = append(w.Panes, tmux.Pane{ID: ids[pos-1], Index: idx, PID: 100 + idx,
			Left: r.X, Top: r.Y, W: r.W, H: r.H, Cwd: cwds[pos-1], Cmd: "zsh"})
	}
	return w
}

// liveServer has the unmanaged session "web" ($3): window "code" (@4) whose
// pane 3 is split from pane 1 (traversal order 1, 3, 2; ids by config
// position %7, %5, %9) and window "run" (@8), plus the managed session
// "api".
func liveServer(t *testing.T) tmux.Snapshot {
	t.Helper()
	cfg := []workspace.Pane{{}, {Split: "v", From: 1, Size: 40}, {Split: "h", From: 1, Size: 30}}
	code := liveWindow(t, "@4", "code", cfg, []string{"%7", "%5", "%9"},
		[]string{"/home/u/web", "/home/u/web/logs", "/home/u/web/app"})
	run := liveWindow(t, "@8", "run", []workspace.Pane{{}}, []string{"%2"}, []string{"/home/u/web"})
	return tmux.Snapshot{Sessions: []tmux.Session{
		{ID: "$1", Name: "api", Key: "api", Windows: []tmux.Window{{ID: "@1", Name: "main", Key: "api/main", W: 200, H: 50,
			Panes: []tmux.Pane{{ID: "%1", Key: "api/main/1", W: 200, H: 50, Cwd: "/home/u/api", Cmd: "zsh"}}}}},
		{ID: "$3", Name: "web", Windows: []tmux.Window{code, run}},
	}}
}

func captureWeb(t *testing.T, snap tmux.Snapshot) capture.Result {
	t.Helper()
	s, err := capture.Find(snap, "web")
	if err != nil {
		t.Fatal(err)
	}
	r, err := capture.Session(s, capture.Options{ID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMarkersMapPanesByPosition(t *testing.T) {
	r := captureWeb(t, liveServer(t))
	got := Markers(r)
	// Config pane 2 (bottom, cwd logs) is %5, pane 3 (top right, cwd app)
	// %9, although tmux lists them as %7, %9, %5 (pane_index order).
	if c := r.Project.Windows[0].Panes; c[1].Cwd != "logs" || c[2].Cwd != "app" {
		t.Fatalf("captured panes %+v", c)
	}
	want := []Mark{
		{Scope: "session", Target: "$3", Name: "@cairn_s", Value: "web"},
		{Scope: "window", Target: "@4", Name: "@cairn_w", Value: "web/code"},
		{Scope: "window", Target: "@8", Name: "@cairn_w", Value: "web/run"},
		{Scope: "pane", Target: "%7", Name: "@cairn_p", Value: "web/code/1"},
		{Scope: "pane", Target: "%5", Name: "@cairn_p", Value: "web/code/2"},
		{Scope: "pane", Target: "%9", Name: "@cairn_p", Value: "web/code/3"},
		{Scope: "pane", Target: "%2", Name: "@cairn_p", Value: "web/run/1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("marks\n got %+v\nwant %+v", got, want)
	}
}

func TestApplyVerifiesEachMarkThenPlanIsEmpty(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	r := captureWeb(t, snap)
	marks := Markers(r)
	// Apply sorts by scope itself: hand it the panes first.
	shuffled := append(slices.Clone(marks[3:]), marks[:3]...)
	if err := Apply(context.Background(), f, snap, shuffled); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, m := range marks {
		want = append(want, "Snapshot", fmt.Sprintf("set %s %s %s %s", m.Scope, m.Target, m.Name, m.Value))
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls\n got %q\nwant %q", f.calls, want)
	}

	p := r.Project
	p.ApplyDefaults("")
	ops := reconcile.Plan([]workspace.Project{p}, f.snap, reconcile.Options{RootExists: func(string) bool { return true }})
	if len(ops) != 0 {
		t.Fatalf("plan after adopt: %+v", ops)
	}
	// Before adoption the same project only meets a name clash.
	ops = reconcile.Plan([]workspace.Project{p}, snap, reconcile.Options{RootExists: func(string) bool { return true }})
	if len(ops) != 1 || ops[0].Kind != reconcile.KindSkipNameClash {
		t.Fatalf("plan before adopt: %+v", ops)
	}
}

func TestApplyAbortsAndRollsBack(t *testing.T) {
	cases := []struct {
		name    string
		before  func(n int, s *tmux.Snapshot)
		failSet int
		want    string
	}{
		{"pane gone", func(n int, s *tmux.Snapshot) {
			if n == 5 { // before the second pane mark
				w := &s.Sessions[1].Windows[0]
				w.Panes = slices.DeleteFunc(w.Panes, func(p tmux.Pane) bool { return p.ID == "%5" })
			}
		}, 0, "pane %5 (web/code/2) no longer exists"},
		{"marked by another project", func(n int, s *tmux.Snapshot) {
			if n == 3 {
				s.Sessions[1].Windows[1].Key = "api/run"
			}
		}, 0, `window @8 (web/run): already carries marker "api/run"`},
		{"session renamed", func(n int, s *tmux.Snapshot) {
			if n == 4 {
				s.Sessions[1].Name = "web2"
			}
		}, 0, `renamed`},
		{"set fails midway", nil, 4, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDriver{snap: liveServer(t)}
			snap := clone(f.snap)
			marks := Markers(captureWeb(t, snap))
			f.before, f.failSet = tc.before, tc.failSet
			err := Apply(context.Background(), f, snap, marks)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			// Only the managed session's markers remain (and the one the
			// other project put there in the meantime).
			for _, m := range f.markers() {
				if !strings.Contains(m, "=api") {
					t.Fatalf("marker left behind: %s (calls %q)", m, f.calls)
				}
			}
			unsets := 0
			for _, c := range f.mutations() {
				if strings.HasPrefix(c, "unset") {
					unsets++
				}
			}
			if unsets == 0 {
				t.Fatalf("no rollback: %q", f.calls)
			}
		})
	}
}

func TestApplyLeavesIdenticalMarkAndRefusesForeignOne(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	f.snap.Sessions[1].Windows[1].Panes[0].Key = "web/run/1" // left over, same value
	snap := clone(f.snap)
	marks := Markers(captureWeb(t, snap))
	if err := Check(snap, marks); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), f, snap, marks); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.calls, "set pane %2 @cairn_p web/run/1") {
		t.Fatalf("identical mark set again: %q", f.calls)
	}

	f = &fakeDriver{snap: liveServer(t)}
	f.snap.Sessions[1].Windows[0].Panes[0].Key = "api/main/2"
	snap = clone(f.snap)
	marks = Markers(captureWeb(t, snap))
	if err := Check(snap, marks); err == nil || !strings.Contains(err.Error(), "pane %7") {
		t.Fatalf("check: %v", err)
	}
	if err := Apply(context.Background(), f, snap, marks); err == nil {
		t.Fatal("foreign marker overwritten")
	}
	if m := f.markers(); !slices.Contains(m, "%7=api/main/2") || len(m) != 4 {
		t.Fatalf("markers %v", m)
	}
}

func TestReleaseRemovesOnlyThisProjectsMarkers(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	if err := Apply(context.Background(), f, snap, Markers(captureWeb(t, snap))); err != nil {
		t.Fatal(err)
	}
	web := &f.snap.Sessions[1]
	// A user's own pane and one carrying another project's marker.
	web.Windows[1].Panes = append(web.Windows[1].Panes,
		tmux.Pane{ID: "%20"}, tmux.Pane{ID: "%21", Key: "api/main/9"})
	f.calls = nil
	n, err := Release(context.Background(), f, "web")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("removed %d, want 7 (calls %q)", n, f.calls)
	}
	if m := f.markers(); !reflect.DeepEqual(m, []string{"$1=api", "@1=api/main", "%1=api/main/1", "%21=api/main/9"}) {
		t.Fatalf("markers after release %v", m)
	}
	// Session first, so an interrupted release leaves an unmanaged session.
	if mu := f.mutations(); mu[0] != "unset session $3 @cairn_s" || slices.ContainsFunc(mu, func(c string) bool { return strings.HasPrefix(c, "set") }) {
		t.Fatalf("mutations %q", mu)
	}
	// Tmux objects are untouched: same sessions, windows and panes.
	if len(f.snap.Sessions) != 2 || len(web.Windows) != 2 || len(web.Windows[0].Panes) != 3 {
		t.Fatalf("objects changed: %+v", f.snap)
	}
	if _, err := Release(context.Background(), f, "web"); err == nil {
		t.Fatal("release of an unmarked project succeeded")
	}
}

func TestReleaseSkipsObjectsChangedMeanwhile(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	if err := Apply(context.Background(), f, snap, Markers(captureWeb(t, snap))); err != nil {
		t.Fatal(err)
	}
	f.calls, f.snaps = nil, 0
	f.before = func(n int, s *tmux.Snapshot) {
		if n == 3 { // the second object (window @4) now belongs elsewhere
			s.Sessions[1].Windows[0].Key = "api/code"
		}
	}
	n, err := Release(context.Background(), f, "web")
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 || slices.Contains(f.calls, "unset window @4 @cairn_w") {
		t.Fatalf("removed %d, calls %q", n, f.calls)
	}
}

// webMarks applies the adoption of "web" to a fresh liveServer model.
func webMarks(t *testing.T) (*fakeDriver, []Mark) {
	t.Helper()
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	marks := Markers(captureWeb(t, snap))
	if err := Apply(context.Background(), f, snap, marks); err != nil {
		t.Fatal(err)
	}
	f.calls, f.snaps = nil, 0
	return f, marks
}

// A child marker that cannot be removed keeps the session marker, so the
// session stays findable by --release; a later release cleans up.
func TestUnmarkKeepsSessionMarkerWhenAChildFails(t *testing.T) {
	f, marks := webMarks(t)
	f.failUnset = "%9"
	err := Unmark(context.Background(), f, marks)
	if err == nil || !strings.Contains(err.Error(), "%9") {
		t.Fatalf("err %v", err)
	}
	if m := f.markers(); !slices.Contains(m, "$3=web") || !slices.Contains(m, "%9=web/code/3") || len(m) != 5 {
		t.Fatalf("markers %v", m)
	}
	f.failUnset = ""
	n, err := Release(context.Background(), f, "web")
	if err != nil || n != 2 {
		t.Fatalf("release %d %v", n, err)
	}
	if m := f.markers(); !reflect.DeepEqual(m, []string{"$1=api", "@1=api/main", "%1=api/main/1"}) {
		t.Fatalf("markers %v", m)
	}
}

// Rollback that fails names the release command, and that command works.
func TestApplyRollbackFailureHint(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	marks := Markers(captureWeb(t, snap))
	f.failSet, f.failUnset = 6, "%5"
	err := Apply(context.Background(), f, snap, marks)
	if err == nil || !strings.Contains(err.Error(), "cairn adopt --release web") {
		t.Fatalf("err %v", err)
	}
	if !slices.Contains(f.markers(), "$3=web") {
		t.Fatalf("session marker removed although a child failed: %v", f.markers())
	}
	f.failUnset = ""
	if _, err := Release(context.Background(), f, "web"); err != nil {
		t.Fatal(err)
	}
	if m := f.markers(); len(m) != 3 {
		t.Fatalf("markers %v", m)
	}
}

// A release interrupted after the session marker went leaves orphan
// window and pane markers in an unmarked session; release reaches them,
// but never inside another project's session nor other prefixes.
func TestReleaseReachesOrphanMarkers(t *testing.T) {
	f, _ := webMarks(t)
	f.snap.Sessions[1].Key = "" // interrupted release
	// web-prefixed window inside the managed api session: not ours to touch.
	f.snap.Sessions[0].Windows = append(f.snap.Sessions[0].Windows, tmux.Window{ID: "@30", Key: "web/stray",
		Panes: []tmux.Pane{{ID: "%30", Key: "web/stray/1"}}})
	// an unmarked session with another project's leftovers and a web one.
	f.snap.Sessions = append(f.snap.Sessions, tmux.Session{ID: "$9", Name: "misc", Windows: []tmux.Window{
		{ID: "@40", Key: "webby/x", Panes: []tmux.Pane{{ID: "%40", Key: "webby/x/1"}, {ID: "%41", Key: "web/old/2"}}}}})
	n, err := Release(context.Background(), f, "web")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 { // 2 windows + 4 panes of the old session, %41 in misc
		t.Fatalf("removed %d (calls %q)", n, f.calls)
	}
	want := []string{"$1=api", "@1=api/main", "%1=api/main/1", "@30=web/stray", "%30=web/stray/1", "@40=webby/x", "%40=webby/x/1"}
	if m := f.markers(); !reflect.DeepEqual(m, want) {
		t.Fatalf("markers\n got %v\nwant %v", m, want)
	}
	if _, err := Release(context.Background(), f, "web"); err == nil {
		t.Fatal("second release found something")
	}
}

// An orphan whose session meanwhile became another project's is skipped.
func TestReleaseSkipsOrphanWhoseSessionWasClaimed(t *testing.T) {
	f, _ := webMarks(t)
	f.snap.Sessions[1].Key = ""
	f.before = func(n int, s *tmux.Snapshot) {
		if n == 2 {
			s.Sessions[1].Key = "api2"
		}
	}
	n, err := Release(context.Background(), f, "web")
	if err != nil || n != 0 {
		t.Fatalf("removed %d, err %v, calls %q", n, err, f.calls)
	}
}

// A pane moved or resized between capture and apply aborts the adoption.
func TestApplyAbortsOnPaneGeometryChange(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	marks := Markers(captureWeb(t, snap))
	f.before = func(n int, s *tmux.Snapshot) {
		if n == 4 {
			s.Sessions[1].Windows[0].Panes[0].W -= 5
		}
	}
	err := Apply(context.Background(), f, snap, marks)
	if err == nil || !strings.Contains(err.Error(), "pane %7 (web/code/1)") || !strings.Contains(err.Error(), "moved or resized") {
		t.Fatalf("err %v", err)
	}
	if m := f.markers(); len(m) != 3 {
		t.Fatalf("markers %v", m)
	}
}

// A set that failed after tmux applied it is rolled back too.
func TestApplyRollsBackAFailedButAppliedSet(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	marks := Markers(captureWeb(t, snap))
	f.failSet, f.failSetApplied = 2, true
	if err := Apply(context.Background(), f, snap, marks); err == nil {
		t.Fatal("no error")
	}
	if m := f.markers(); len(m) != 3 {
		t.Fatalf("markers %v (calls %q)", m, f.calls)
	}
}

// Cancellation (Ctrl-C) between marks stops the adoption and the rollback
// still runs.
func TestApplyCancelledRollsBack(t *testing.T) {
	f := &fakeDriver{snap: liveServer(t)}
	snap := clone(f.snap)
	marks := Markers(captureWeb(t, snap))
	ctx, cancel := context.WithCancel(context.Background())
	f.afterSet = cancel
	err := Apply(ctx, f, snap, marks)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if m := f.markers(); len(m) != 3 || f.sets != 1 {
		t.Fatalf("markers %v, %d sets", m, f.sets)
	}
}
