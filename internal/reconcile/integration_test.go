package reconcile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

var socketSeq atomic.Int64

// holder keeps the isolated test server alive; it is started with
// "-f /dev/null" (no user configuration) and SHELL=/bin/sh, so the server
// never reads the owner's tmux or shell setup.
const holder = "cairn-test-holder"

type testTmux struct {
	t      *testing.T
	socket string
	c      tmux.Client
}

func testEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") || strings.HasPrefix(kv, "SHELL=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "SHELL=/bin/sh")
}

// newTestTmux starts a server on an isolated socket "cairn-test-<pid>-<n>"
// and kills that server (and only it) when the test ends. The default socket
// is never used.
func newTestTmux(t *testing.T) *testTmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	tt := &testTmux{t: t, socket: fmt.Sprintf("cairn-test-%d-%d", os.Getpid(), socketSeq.Add(1))}
	tt.c = tmux.NewClient(tt.socket)
	t.Cleanup(func() {
		path, err := tt.tmuxOut("display-message", "-p", "#{socket_path}")
		_, _ = tt.tmuxOut("kill-server")
		if p := strings.TrimSpace(path); err == nil && filepath.Base(p) == tt.socket {
			_ = os.Remove(p)
		}
	})
	tt.start()
	return tt
}

func (tt *testTmux) tmuxOut(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-L", tt.socket}, args...)...)
	cmd.Env = testEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (tt *testTmux) tmux(args ...string) {
	tt.t.Helper()
	if out, err := tt.tmuxOut(args...); err != nil {
		tt.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
}

func (tt *testTmux) start() {
	tt.t.Helper()
	tt.tmux("-f", "/dev/null", "new-session", "-d", "-s", holder)
}

// reboot kills the test server and starts a fresh one.
func (tt *testTmux) reboot() {
	tt.t.Helper()
	tt.tmux("kill-server")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := tt.tmuxOut("has-session"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			tt.t.Fatal("server did not exit")
		}
	}
	tt.start()
}

func (tt *testTmux) snap() tmux.Snapshot {
	tt.t.Helper()
	s, err := tt.c.Snapshot(context.Background())
	if err != nil {
		tt.t.Fatal(err)
	}
	return s
}

func (tt *testTmux) plan(ps []workspace.Project) []Op {
	tt.t.Helper()
	return Plan(ps, tt.snap(), Options{PaneIdle: tt.c.PaneIdle})
}

func (tt *testTmux) apply(ps []workspace.Project) {
	tt.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, r := range Apply(ctx, tt.c, ps, tt.plan(ps)) {
		if r.Err != nil {
			tt.t.Fatalf("%s %s: %v", r.Op.Kind, r.Op.Target, r.Err)
		}
	}
}

func (tt *testTmux) waitFor(what string, cond func(tmux.Snapshot) bool) tmux.Snapshot {
	tt.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s := tt.snap(); cond(s) {
			return s
		}
	}
	tt.t.Fatalf("timed out waiting for %s", what)
	return tmux.Snapshot{}
}

func paneByKey(s tmux.Snapshot, key string) (tmux.Pane, bool) {
	for _, se := range s.Sessions {
		for _, w := range se.Windows {
			for _, p := range w.Panes {
				if p.Key == key {
					return p, true
				}
			}
		}
	}
	return tmux.Pane{}, false
}

// shape returns "window key: pane keys" lines of the managed sessions.
func shape(s tmux.Snapshot) string {
	var out []string
	for _, se := range s.Sessions {
		if se.Key == "" {
			continue
		}
		for _, w := range se.Windows {
			var keys []string
			for _, p := range w.Panes {
				keys = append(keys, p.Key)
			}
			out = append(out, fmt.Sprintf("%s %s %s: %s", se.Key, se.Name, w.Key, strings.Join(keys, ",")))
		}
	}
	return strings.Join(out, "\n")
}

// checkGeometry asserts that every pane of every multi-pane window sits at
// layout.Geometry for the window's size, matched by pane key.
func checkGeometry(t *testing.T, p workspace.Project, s tmux.Snapshot) {
	t.Helper()
	for _, w := range p.Windows {
		tree, err := layout.Build(w.Panes)
		if err != nil {
			t.Fatal(err)
		}
		var tw tmux.Window
		for _, se := range s.Sessions {
			for _, x := range se.Windows {
				if x.Key == p.ID+"/"+w.Key {
					tw = x
				}
			}
		}
		if tw.ID == "" {
			t.Fatalf("window %s missing:\n%s", w.Key, shape(s))
		}
		if len(tw.Panes) != len(w.Panes) {
			t.Fatalf("window %s has %d panes, want %d", w.Key, len(tw.Panes), len(w.Panes))
		}
		for _, r := range layout.Geometry(tree, tw.W, tw.H) {
			key := fmt.Sprintf("%s/%s/%d", p.ID, w.Key, r.Pane)
			tp, ok := paneByKey(s, key)
			if !ok {
				t.Fatalf("pane %s missing", key)
			}
			if tp.Left != r.X || tp.Top != r.Y || tp.W != r.W || tp.H != r.H {
				t.Errorf("pane %s at %d,%d %dx%d want %d,%d %dx%d", key, tp.Left, tp.Top, tp.W, tp.H, r.X, r.Y, r.W, r.H)
			}
		}
	}
}

// failOnStop exits 3 when a STOP file appears in its cwd (and removes it).
const failOnStop = `while :; do if [ -e STOP ]; then rm -f STOP; exit 3; fi; sleep 0.05; done`

func integrationDev(t *testing.T) workspace.Project {
	p, _ := workspace.Template("dev")
	p.ID = "demo"
	p.Root = t.TempDir()
	p.Windows[0].Panes[0].Run = []string{"sh", "-c", failOnStop}
	p.Windows[0].Panes[0].Restart = workspace.RestartOnFailure
	p.ApplyDefaults("/home/u")
	if errs := p.Validate(); len(errs) > 0 {
		t.Fatal(errs)
	}
	return p
}

func TestIntegrationApplyRespawnAndRestore(t *testing.T) {
	tt := newTestTmux(t)
	p := integrationDev(t)
	ps := []workspace.Project{p}

	tt.apply(ps)
	s := tt.snap()
	if ops := Plan(ps, s, Options{PaneIdle: tt.c.PaneIdle}); len(ops) != 0 {
		t.Fatalf("second plan not empty: %s", kinds(ops))
	}
	tt.apply(ps) // a no-op apply
	if got := tt.snap(); shape(got) != shape(s) {
		t.Fatalf("second apply changed tmux:\n%s\nvs\n%s", shape(got), shape(s))
	}
	checkGeometry(t, p, s)
	for _, se := range s.Sessions {
		if se.Key == "demo" && (se.Windows[0].W != 200 || se.Windows[0].H != 50) {
			t.Errorf("new session window size %dx%d", se.Windows[0].W, se.Windows[0].H)
		}
	}
	before := shape(s)

	// The editor program fails: on-failure respawns it in the same pane.
	ed, _ := paneByKey(s, "demo/code/1")
	if err := os.WriteFile(filepath.Join(p.Root, "STOP"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tt.waitFor("editor to die with status 3", func(s tmux.Snapshot) bool {
		x, ok := paneByKey(s, "demo/code/1")
		return ok && x.Dead && x.DeadStatus == 3
	})
	if got := kinds(tt.plan(ps)); got != "demo:respawn-pane:code:demo/code/1" {
		t.Fatalf("ops %s", got)
	}
	tt.apply(ps)
	s = tt.waitFor("editor to run again", func(s tmux.Snapshot) bool {
		x, ok := paneByKey(s, "demo/code/1")
		return ok && !x.Dead
	})
	if x, _ := paneByKey(s, "demo/code/1"); x.ID != ed.ID || x.PID == ed.PID {
		t.Fatalf("respawn: pane %+v before %+v", x, ed)
	}
	if ops := tt.plan(ps); len(ops) != 0 {
		t.Fatalf("plan after respawn: %s", kinds(ops))
	}

	// "Reboot": the server is gone; apply restores the same keys and layout.
	tt.reboot()
	if ops := tt.plan(ps); kinds(ops) != "demo:create-session:code" {
		t.Fatalf("after reboot: %s", kinds(ops))
	}
	tt.apply(ps)
	s = tt.snap()
	if shape(s) != before {
		t.Fatalf("restored shape\n%s\nwant\n%s", shape(s), before)
	}
	checkGeometry(t, p, s)
	if ops := tt.plan(ps); len(ops) != 0 {
		t.Fatalf("plan after restore: %s", kinds(ops))
	}
}

func TestIntegrationMiddlePaneRecreated(t *testing.T) {
	tt := newTestTmux(t)
	p := fourPanes("demo")
	p.Root = t.TempDir()
	for i := range p.Windows[0].Panes {
		p.Windows[0].Panes[i].Cwd = p.Root
	}
	ps := []workspace.Project{p}
	tt.apply(ps)
	checkGeometry(t, p, tt.snap())

	gone, _ := paneByKey(tt.snap(), "demo/main/2")
	tt.tmux("kill-pane", "-t", gone.ID)
	tt.waitFor("pane 2 to disappear", func(s tmux.Snapshot) bool {
		_, ok := paneByKey(s, "demo/main/2")
		return !ok
	})
	if got := kinds(tt.plan(ps)); got != "demo:create-pane:main:demo/main/2 demo:set-layout:main" {
		t.Fatalf("ops %s", got)
	}
	tt.apply(ps)
	s := tt.snap()
	checkGeometry(t, p, s)
	var order []string
	for _, se := range s.Sessions {
		for _, w := range se.Windows {
			if w.Key == "demo/main" {
				for _, x := range w.Panes {
					order = append(order, x.Key)
				}
			}
		}
	}
	if want := []string{"demo/main/1", "demo/main/3", "demo/main/2", "demo/main/4"}; !slices.Equal(order, want) {
		t.Fatalf("pane list order %v want %v", order, want)
	}
	if ops := tt.plan(ps); len(ops) != 0 {
		t.Fatalf("plan after recreate: %s", kinds(ops))
	}
}

func TestIntegrationNameClashLeavesUserSession(t *testing.T) {
	tt := newTestTmux(t)
	p := integrationDev(t)
	tt.tmux("new-session", "-d", "-s", "demo")
	ops := tt.plan([]workspace.Project{p})
	if kinds(ops) != "demo:skip-name-clash" {
		t.Fatalf("ops %s", kinds(ops))
	}
	tt.apply([]workspace.Project{p})
	for _, se := range tt.snap().Sessions {
		if se.Key != "" {
			t.Fatalf("managed session created despite the clash: %+v", se)
		}
	}
}
