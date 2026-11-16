package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

var socketSeq atomic.Int64

// holder is a session started with "-f /dev/null" so the test server never
// reads the user's tmux configuration; snap hides it.
const holder = "cairn-test-holder"

// testServer returns a Client on an isolated socket "cairn-test-<pid>-<n>"
// and kills that server (and only it) when the test ends. With start, the
// server is started without any configuration file.
func testServer(t *testing.T, start bool) Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	socket := fmt.Sprintf("cairn-test-%d-%d", os.Getpid(), socketSeq.Add(1))
	c := NewClient(socket)
	t.Cleanup(func() {
		tmux := func(args ...string) ([]byte, error) {
			cmd := exec.Command("tmux", append([]string{"-L", socket}, args...)...)
			cmd.Env = cleanEnv(os.Environ())
			return cmd.Output()
		}
		path, err := tmux("display-message", "-p", "#{socket_path}")
		_, _ = tmux("kill-server")
		// tmux leaves the socket file behind; remove only this test's socket.
		if p := strings.TrimSpace(string(path)); err == nil && filepath.Base(p) == socket {
			_ = os.Remove(p)
		}
	})
	if start {
		cmd := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", holder)
		cmd.Env = cleanEnv(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start test server: %v: %s", err, out)
		}
	}
	return c
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func snap(t *testing.T, c Client) Snapshot {
	t.Helper()
	s, err := c.Snapshot(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var out Snapshot
	for _, se := range s.Sessions {
		if se.Name != holder {
			out.Sessions = append(out.Sessions, se)
		}
	}
	return out
}

func findWindow(s Snapshot, key string) (Window, bool) {
	for _, se := range s.Sessions {
		for _, w := range se.Windows {
			if w.Key == key {
				return w, true
			}
		}
	}
	return Window{}, false
}

func findPane(s Snapshot, id string) (Pane, bool) {
	for _, se := range s.Sessions {
		for _, w := range se.Windows {
			for _, p := range w.Panes {
				if p.ID == id {
					return p, true
				}
			}
		}
	}
	return Pane{}, false
}

func TestIntegrationGeometryMatchesLayout(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	dir := t.TempDir()
	panes := []workspace.Pane{
		{},
		{Split: "h", Size: 40},
		{Split: "v", From: 1, Size: 30}, // split from a non-last pane
		{Split: "v", From: 2, Size: 50},
	}
	tree, err := layout.Build(panes)
	if err != nil {
		t.Fatal(err)
	}
	const w, h = 120, 40
	key := func(n int) string { return fmt.Sprintf("demo/main/%d", n) }
	_, wid, p1, err := c.NewSession(ctx, "demo", w, h, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: key(1), Cwd: dir}})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{p1}
	for i := 1; i < len(panes); i++ {
		from := panes[i].From
		if from == 0 {
			from = i
		}
		id, err := c.SplitPane(ctx, ids[from-1], panes[i].Split, panes[i].Size, PaneSpec{Key: key(i + 1), Cwd: dir})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ls, err := layout.TmuxString(tree, w, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SelectLayout(ctx, wid, ls); err != nil {
		t.Fatal(err)
	}

	s := snap(t, c)
	if len(s.Sessions) != 1 || s.Sessions[0].Key != "demo" || s.Sessions[0].Name != "demo" {
		t.Fatalf("sessions %+v", s.Sessions)
	}
	win, ok := findWindow(s, "demo/main")
	if !ok || win.ID != wid || win.W != w || win.H != h || win.Name != "main" {
		t.Fatalf("window %+v", win)
	}
	var listOrder []string
	byKey := map[string]Pane{}
	for _, p := range win.Panes {
		listOrder = append(listOrder, p.Key)
		byKey[p.Key] = p
	}
	if strings.Join(listOrder, ",") == strings.Join([]string{key(1), key(2), key(3), key(4)}, ",") {
		t.Fatalf("list order equals creation order %v; the test should exercise a differing order", listOrder)
	}
	for _, r := range layout.Geometry(tree, w, h) {
		p, ok := byKey[key(r.Pane)]
		if !ok {
			t.Fatalf("pane %s missing in %v", key(r.Pane), listOrder)
		}
		if p.ID != ids[r.Pane-1] {
			t.Errorf("pane %s id %s want %s", key(r.Pane), p.ID, ids[r.Pane-1])
		}
		if p.Left != r.X || p.Top != r.Y || p.W != r.W || p.H != r.H {
			t.Errorf("pane %s geometry %d,%d %dx%d want %d,%d %dx%d", key(r.Pane), p.Left, p.Top, p.W, p.H, r.X, r.Y, r.W, r.H)
		}
	}
}

func TestIntegrationMarkersSurviveRename(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	sid, wid, pid, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.run(ctx, "rename-session", "-t", sid, "other"); err != nil {
		t.Fatal(err)
	}
	if err := c.run(ctx, "rename-window", "-t", wid, "renamed"); err != nil {
		t.Fatal(err)
	}
	s := snap(t, c)
	se := s.Sessions[0]
	if se.Name != "other" || se.Key != "demo" || se.Windows[0].Name != "renamed" || se.Windows[0].Key != "demo/main" ||
		se.Windows[0].Panes[0].ID != pid || se.Windows[0].Panes[0].Key != "demo/main/1" {
		t.Fatalf("after rename %+v", se)
	}
	if se.Windows[0].W != DefaultWidth || se.Windows[0].H != DefaultHeight {
		t.Fatalf("default size %dx%d", se.Windows[0].W, se.Windows[0].H)
	}
}

// UnsetOption removes exactly the marker set on one object: the others,
// at every level, stay.
func TestIntegrationUnsetOption(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	sid, wid, pid, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UnsetOption(ctx, "pane", pid, "@cairn_p"); err != nil {
		t.Fatal(err)
	}
	se := snap(t, c).Sessions[0]
	if se.Key != "demo" || se.Windows[0].Key != "demo/main" || se.Windows[0].Panes[0].Key != "" {
		t.Fatalf("after pane unset %+v", se)
	}
	if err := c.UnsetOption(ctx, "window", wid, "@cairn_w"); err != nil {
		t.Fatal(err)
	}
	if err := c.UnsetOption(ctx, "session", sid, "@cairn_s"); err != nil {
		t.Fatal(err)
	}
	se = snap(t, c).Sessions[0]
	if se.Key != "" || se.Windows[0].Key != "" || se.Windows[0].Panes[0].Key != "" {
		t.Fatalf("after unset %+v", se)
	}
	if err := c.SetOption(ctx, "pane", pid, "@cairn_p", "demo/main/1"); err != nil {
		t.Fatal(err)
	}
	if got := snap(t, c).Sessions[0].Windows[0].Panes[0].Key; got != "demo/main/1" {
		t.Fatalf("pane marker %q", got)
	}
}

func TestIntegrationArgvReachesProgramUnchanged(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	args := []string{"a b", "it's", `"q"`, "$HOME", "ü"}
	argv := append([]string{"sh", "-c", `printf '%s|' "$@" > "$0.tmp" && mv "$0.tmp" "$0"`, out}, args...)
	if _, _, _, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: dir, Argv: argv}}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	waitFor(t, "program output", func() bool {
		b, err := os.ReadFile(out)
		got = b
		return err == nil
	})
	if want := strings.Join(args, "|") + "|"; string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestIntegrationDeadPaneAndRespawn(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	dir := t.TempDir()
	sid, _, _, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: dir}})
	if err != nil {
		t.Fatal(err)
	}
	// The delay keeps the program alive until remain-on-exit is set (see
	// remainOnExit: a program exiting immediately can race the option).
	_, pid, err := c.NewWindow(ctx, sid, WindowSpec{Key: "demo/job", Name: "job",
		PaneSpec: PaneSpec{Key: "demo/job/1", Cwd: dir, Argv: []string{"sh", "-c", "sleep 0.5; exit 3"}}})
	if err != nil {
		t.Fatal(err)
	}
	var p Pane
	waitFor(t, "dead pane", func() bool {
		var ok bool
		p, ok = findPane(snap(t, c), pid)
		return ok && p.Dead
	})
	if p.DeadStatus != 3 || p.Key != "demo/job/1" {
		t.Fatalf("dead pane %+v", p)
	}
	if err := c.RespawnPane(ctx, pid, []string{"sleep", "30"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "respawned pane", func() bool {
		p, _ = findPane(snap(t, c), pid)
		return !p.Dead && p.Cmd == "sleep"
	})
	// Without -k tmux refuses to respawn a live pane.
	if err := c.RespawnPane(ctx, pid, []string{"sleep", "31"}); err == nil {
		t.Fatal("a live pane was respawned")
	}
	if p2, _ := findPane(snap(t, c), pid); p2.PID != p.PID {
		t.Fatalf("live pane restarted: pid %d -> %d", p.PID, p2.PID)
	}
}

func TestIntegrationSwapPanes(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	dir := t.TempDir()
	_, wid, a, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: dir, Argv: []string{"sleep", "60"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.SplitPane(ctx, a, "h", 50, PaneSpec{Key: "demo/main/2", Cwd: dir, Argv: []string{"sleep", "61"}})
	if err != nil {
		t.Fatal(err)
	}
	before := snap(t, c)
	pa, _ := findPane(before, a)
	pb, _ := findPane(before, b)
	if pa.Index >= pb.Index {
		t.Fatalf("before swap %+v %+v", pa, pb)
	}
	if err := c.SwapPanes(ctx, a, b); err != nil {
		t.Fatal(err)
	}
	after := snap(t, c)
	qa, _ := findPane(after, a)
	qb, _ := findPane(after, b)
	if qa.Index != pb.Index || qb.Index != pa.Index {
		t.Fatalf("after swap %+v %+v", qa, qb)
	}
	if qa.PID != pa.PID || qb.PID != pb.PID || qa.Key != "demo/main/1" || qb.Key != "demo/main/2" {
		t.Fatalf("ids did not keep their processes: %+v %+v", qa, qb)
	}
	win, _ := findWindow(after, "demo/main")
	if win.ID != wid || win.Panes[0].ID != b {
		t.Fatalf("window order %+v", win.Panes)
	}
}

func TestIntegrationNoServerSnapshotIsEmpty(t *testing.T) {
	c := testServer(t, false) // never started
	s, err := c.Snapshot(ctxT(t))
	if err != nil || len(s.Sessions) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
}

// display prints a format for target.
func display(t *testing.T, c Client, target, format string) string {
	t.Helper()
	out, err := c.output(ctxT(t), "display-message", "-p", "-t", target, format)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(out, "\n")
}

func TestIntegrationUserValuesReachTmuxIntact(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	dir := filepath.Join(t.TempDir(), "c#proj;")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sleep := []string{"sleep", "60"}
	sid, _, _, err := c.NewSession(ctx, "s#1;", 0, 0, WindowSpec{Key: "demo/main", Name: "main", PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: dir, Argv: sleep}})
	if err != nil {
		t.Fatal(err)
	}
	if got := display(t, c, sid, "#{session_name}"); got != "s#1;" {
		t.Errorf("session name %q", got)
	}
	for i, name := range []string{"a;", "a#{session_name}", "-dash", "x#(echo hi)", ";", "p##q"} {
		key := fmt.Sprintf("demo/w%d", i)
		wid, pid, err := c.NewWindow(ctx, sid, WindowSpec{Key: key, Name: name, PaneSpec: PaneSpec{Key: key + "/1", Cwd: dir, Argv: sleep}})
		if err != nil {
			t.Fatalf("window %q: %v", name, err)
		}
		if got := display(t, c, wid, "#{window_name}"); got != name {
			t.Errorf("window name %q arrived as %q", name, got)
		}
		if got := display(t, c, pid, "#{pane_current_path}"); got != dir {
			t.Errorf("cwd %q arrived as %q", dir, got)
		}
	}
	for _, v := range []string{"x;", `x\;`, "#{session_name}"} {
		if err := c.SetOption(ctx, "session", sid, "@v", v); err != nil {
			t.Fatal(err)
		}
		if got := display(t, c, sid, "#{@v}"); got != v {
			t.Errorf("option value %q arrived as %q", v, got)
		}
	}

	// The commands keep sleeping: a pane whose program exits at once can take
	// itself away before cairn marks it (a documented race), which is not what
	// this test checks.
	out1, out2 := filepath.Join(dir, "out1"), filepath.Join(dir, "out2")
	args := []string{"hi;", `x\;`, ";", "#{session_name}"}
	argv := append([]string{"sh", "-c", `printf '%s|' "$K" "$@" > "$0"; sleep 60`, out1}, args...)
	if _, _, err := c.NewWindow(ctx, sid, WindowSpec{Key: "demo/argv", Name: "argv", PaneSpec: PaneSpec{
		Key: "demo/argv/1", Cwd: dir, Argv: argv, Env: map[string]string{"K": "v#{x};"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.NewWindow(ctx, sid, WindowSpec{Key: "demo/shell", Name: "shell", PaneSpec: PaneSpec{
		Key: "demo/shell/1", Cwd: dir, Argv: []string{`printf '%s' 'hi;' > out2; sleep 60`}}}); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		out1: "v#{x};|" + strings.Join(args, "|") + "|",
		out2: "hi;",
	} {
		var got []byte
		waitFor(t, file, func() bool {
			b, err := os.ReadFile(file)
			got = b
			return err == nil && len(b) > 0
		})
		if string(got) != want {
			t.Errorf("%s: got %q want %q", filepath.Base(file), got, want)
		}
	}
}

func TestIntegrationSessionIDByKey(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	sid, _, _, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main"})
	if err != nil {
		t.Fatal(err)
	}
	id, ok, err := c.SessionIDByKey(ctx, "demo")
	if err != nil || !ok || id != sid {
		t.Fatalf("%q %v %v want %q", id, ok, err, sid)
	}
}

func TestIntegrationPaneIdle(t *testing.T) {
	c := testServer(t, true)
	ctx := ctxT(t)
	dir := t.TempDir()
	sid, _, sleepPane, err := c.NewSession(ctx, "demo", 0, 0, WindowSpec{Key: "demo/main", Name: "main",
		PaneSpec: PaneSpec{Key: "demo/main/1", Cwd: dir, Argv: []string{"sleep", "60"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, shPane, err := c.NewWindow(ctx, sid, WindowSpec{Key: "demo/sh", Name: "sh",
		PaneSpec: PaneSpec{Key: "demo/sh/1", Cwd: dir, Argv: []string{"sh", "-i"}}})
	if err != nil {
		t.Fatal(err)
	}
	pidOf := func(id string) int {
		p, ok := findPane(snap(t, c), id)
		if !ok {
			t.Fatalf("pane %s missing", id)
		}
		return p.PID
	}
	if idle, err := c.PaneIdle(pidOf(sleepPane)); err != nil || idle {
		t.Fatalf("exec'd sleep pane idle=%v err=%v", idle, err)
	}
	waitFor(t, "idle shell", func() bool {
		idle, err := c.PaneIdle(pidOf(shPane))
		return err == nil && idle
	})
}
