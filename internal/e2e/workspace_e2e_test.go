//go:build e2e

// Package e2e drives the built cairn binary against an isolated tmux server.
// Every tmux command uses the private socket cairn-e2e-<pid> and a temporary
// HOME, XDG and TMUX_TMPDIR; the default tmux server is only ever listed
// (read-only) to prove it is left alone.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	binPath string
	sockDir string
	sock    = fmt.Sprintf("cairn-e2e-%d", os.Getpid())
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Println("tmux not installed: skipping e2e")
		return 0
	}
	dir, err := os.MkdirTemp("", "cairn-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	sockDir = filepath.Join(dir, "tmux")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	binPath = filepath.Join(dir, "cairn")
	build := exec.Command("go", "build", "-o", binPath, "github.com/mrqwer/cairn/cmd/cairn")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		return 1
	}
	defer func() {
		c := exec.Command("tmux", "-L", sock, "kill-server")
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + sockDir}
		_ = c.Run()
	}()
	return m.Run()
}

// env is an isolated environment: temp HOME/XDG/TMUX_TMPDIR, no $TMUX.
type env struct {
	t    *testing.T
	home string
	root string // project root directory
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, home: home, root: filepath.Join(home, "root")}
	if err := os.MkdirAll(e.root, 0o755); err != nil {
		t.Fatal(err)
	}
	kill := func() { e.cmd("tmux", "-L", sock, "kill-server") } // private socket only
	kill()
	t.Cleanup(kill)
	return e
}

func (e *env) environ() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"XDG_CONFIG_HOME=" + filepath.Join(e.home, "config"),
		"XDG_STATE_HOME=" + filepath.Join(e.home, "state"),
		"XDG_RUNTIME_DIR=" + filepath.Join(e.home, "run"),
		"TMUX_TMPDIR=" + sockDir,
		"CAIRN_TMUX_SOCKET=" + sock,
		"TERM=xterm",
	}
}

func (e *env) cmd(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Env = e.environ()
	c.Dir = e.home
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	c.Stdin = nil
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(60 * time.Second):
		c.Process.Kill()
		e.t.Fatalf("%s %v timed out", name, args)
		return "", nil
	}
}

// cairn runs the binary and fails the test when it exits non-zero.
func (e *env) cairn(args ...string) string {
	e.t.Helper()
	out, err := e.cmd(binPath, args...)
	if err != nil {
		e.t.Fatalf("cairn %v: %v\n%s", args, err, out)
	}
	return out
}

// tmux runs tmux on the private socket only.
func (e *env) tmux(args ...string) string {
	e.t.Helper()
	out, err := e.cmd("tmux", append([]string{"-L", sock}, args...)...)
	if err != nil {
		e.t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return out
}

func (e *env) projectFile(id string) string {
	return filepath.Join(e.home, "config", "cairn", "projects", id+".toml")
}

func (e *env) editProject(id, old, new string) {
	e.t.Helper()
	b, err := os.ReadFile(e.projectFile(id))
	if err != nil {
		e.t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		e.t.Fatalf("%q not in %s:\n%s", old, id, b)
	}
	if err := os.WriteFile(e.projectFile(id), []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// useSleepEditor swaps the template's nvim for sleep so the test does not
// depend on an editor being installed.
func (e *env) useSleepEditor(id string) {
	e.editProject(id, `run = ["nvim"]`, "run = [\"sleep\", \"600\"]\nrestart = \"on-failure\"")
}

const paneFmt = "#{session_name} #{window_name} #{pane_index} #{pane_left} #{pane_top} #{pane_width}x#{pane_height}"

func (e *env) geometry(session string) string {
	return e.tmux("list-panes", "-s", "-t", session, "-F", paneFmt)
}

func (e *env) paneIDs(session string) string {
	return e.tmux("list-panes", "-s", "-t", session, "-F", "#{pane_id} #{pane_pid}")
}

func contains(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func defaultSessions() (string, bool) {
	// Read-only listing of the owner's default server, to prove it is untouched.
	c := exec.Command("tmux", "list-sessions", "-F", "#{session_id} #{session_name}")
	out, err := c.Output()
	return string(out), err == nil
}

func TestWorkspaceLifecycle(t *testing.T) {
	before, hadDefault := defaultSessions()
	e := newEnv(t)

	e.cairn("project", "add", "demo", "--root", e.root, "--template", "dev")
	e.useSleepEditor("demo")

	contains(t, e.cairn("plan"), "create-session")
	contains(t, e.cairn("apply", "--yes"), "demo")
	if out := e.cairn("plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("second plan:\n%s", out)
	}
	geo := e.geometry("demo")
	if n := strings.Count(geo, "\n"); n != 3 {
		t.Fatalf("want 3 panes, got:\n%s", geo)
	}

	// simulated crash: the whole server dies, up rebuilds the same layout
	e.tmux("kill-server")
	e.cairn("up")
	if got := e.geometry("demo"); got != geo {
		t.Fatalf("geometry after crash+up\n got:\n%s\nwant:\n%s", got, geo)
	}

	// the editor pane's process dies: up respawns it (restart = on-failure)
	pid := strings.TrimSpace(e.tmux("display", "-p", "-t", "demo:code.0", "#{pane_pid}"))
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("pane pid %q", pid)
	}
	if out, err := e.cmd("kill", "-9", pid); err != nil {
		t.Fatalf("kill: %v %s", err, out)
	}
	waitFor(t, "editor pane dead", func() bool {
		return strings.TrimSpace(e.tmux("display", "-p", "-t", "demo:code.0", "#{pane_dead}")) == "1"
	})
	contains(t, e.cairn("up"), "1 updated")
	waitFor(t, "editor pane respawned", func() bool {
		return strings.TrimSpace(e.tmux("display", "-p", "-t", "demo:code.0", "#{pane_dead} #{pane_current_command}")) == "0 sleep"
	})
	if got := e.geometry("demo"); got != geo {
		t.Fatalf("geometry after respawn\n got:\n%s\nwant:\n%s", got, geo)
	}

	// capture a hand-made session, save it, and up creates an equivalent one
	e.tmux("new-session", "-d", "-s", "handmade", "-c", e.root)
	e.tmux("split-window", "-t", "handmade", "-h", "-c", e.root)
	e.cairn("capture", "handmade", "--id", "copy", "--write")
	e.editProject("copy", `session = "handmade"`, `session = "copy-session"`)
	e.cairn("up")
	// same windows, same panes, split the same way (sizes differ: the hand-made
	// session was detached at 80x24, cairn creates sessions at its default size)
	shape := func(sess string) string {
		return e.tmux("list-panes", "-s", "-t", sess, "-F", "#{window_name} #{pane_index} #{?#{==:#{pane_left},0},left,right}")
	}
	if a, b := shape("handmade"), shape("copy-session"); a != b {
		t.Fatalf("captured session differs\n handmade:\n%s\n copy:\n%s", a, b)
	}
	if n := strings.Count(shape("copy-session"), "\n"); n != 2 {
		t.Fatalf("copy-session panes: %d, want 2", n)
	}

	// undo restores a removed project file, byte for byte
	e.cairn("project", "archive", "copy")
	want, err := os.ReadFile(e.projectFile("copy"))
	if err != nil {
		t.Fatal(err)
	}
	e.cairn("project", "rm", "copy", "--yes")
	if _, err := os.Stat(e.projectFile("copy")); err == nil {
		t.Fatal("project rm left the file")
	}
	e.cairn("undo")
	if got, err := os.ReadFile(e.projectFile("copy")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("undo did not restore the file (err %v)\n got:\n%s\nwant:\n%s", err, got, want)
	}

	assertDefaultUntouched(t, before, hadDefault)
}

func TestAdoptKeepsPanes(t *testing.T) {
	e := newEnv(t)
	e.tmux("new-session", "-d", "-s", "hand", "-c", e.root)
	e.tmux("split-window", "-t", "hand", "-v", "-c", e.root)
	ids := e.paneIDs("hand")
	e.cairn("adopt", "hand", "--id", "hand", "--root", e.root)
	if out := e.cairn("plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after adopt:\n%s", out)
	}
	e.cairn("apply", "--yes")
	if got := e.paneIDs("hand"); got != ids {
		t.Fatalf("adopt restarted panes\n got:\n%s\nwant:\n%s", got, ids)
	}
}

func TestPruneKillsIdleKeepsBusy(t *testing.T) {
	e := newEnv(t)
	e.cairn("project", "add", "demo", "--root", e.root, "--template", "dev")
	e.useSleepEditor("demo")
	e.cairn("apply", "--yes")
	// the split pane (code.1) is an idle shell; make the run window's pane busy
	e.tmux("send-keys", "-t", "demo:run.0", "sleep 600", "Enter")
	waitFor(t, "run pane busy", func() bool {
		return strings.TrimSpace(e.tmux("display", "-p", "-t", "demo:run.0", "#{pane_current_command}")) == "sleep"
	})
	// drop both panes from the config: run window and the split pane
	b, _ := os.ReadFile(e.projectFile("demo"))
	s := string(b)
	i := strings.Index(s, "[[windows.panes]]\nsplit")
	if i < 0 {
		t.Fatalf("no split pane in:\n%s", s)
	}
	s = s[:i]
	if err := os.WriteFile(e.projectFile("demo"), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	out := e.cairn("apply", "--prune", "--yes")
	t.Log(out)
	panes := e.tmux("list-panes", "-s", "-t", "demo", "-F", "#{window_name}.#{pane_index}")
	if !strings.Contains(panes, "code.0") {
		t.Errorf("editor pane missing:\n%s", panes)
	}
	if strings.Contains(panes, "code.1") {
		t.Errorf("idle shell pane was not pruned:\n%s", panes)
	}
	if !strings.Contains(panes, "run.0") {
		t.Errorf("busy pane was killed:\n%s", panes)
	}
}

func TestDoctorWorkspaceReadOnly(t *testing.T) {
	e := newEnv(t)
	e.cairn("project", "add", "demo", "--root", e.root, "--template", "dev")
	e.useSleepEditor("demo")
	e.cairn("apply", "--yes")
	e.tmux("new-session", "-d", "-s", "foreign", "-c", e.root)
	snap := func() string {
		return e.tmux("list-panes", "-a", "-F", "#{session_id} #{window_id} #{pane_id} #{pane_pid} #{pane_width}x#{pane_height} #{@cairn_p}") +
			e.tmux("list-sessions", "-F", "#{session_id} #{session_name} #{@cairn_s}")
	}
	before := snap()
	out, _ := e.cmd(binPath, "doctor", "workspace") // exit code reflects findings, not a failure
	contains(t, out, "tmux")
	if after := snap(); after != before {
		t.Fatalf("doctor workspace changed tmux state\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func assertDefaultUntouched(t *testing.T, before string, had bool) {
	t.Helper()
	if !had {
		t.Log("no default tmux server: default-socket assertion skipped")
		return
	}
	after, ok := defaultSessions()
	if !ok || after != before {
		t.Fatalf("default tmux server changed\nbefore:\n%s\nafter (ok=%v):\n%s", before, ok, after)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
