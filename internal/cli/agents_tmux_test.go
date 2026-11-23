package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// TestMain lets the test binary act as cairn itself when an agent pane in a
// real-tmux test runs "<self> agents pane <id>" (self is a wrapper script
// setting cairnTestMainEnv).
func TestMain(m *testing.M) {
	if os.Getenv(cairnTestMainEnv) == "1" {
		cmd := NewRootCmd()
		cmd.SetArgs(os.Args[1:])
		if err := cmd.Execute(); err != nil {
			var ee *ExitError
			if errors.As(err, &ee) {
				os.Exit(ee.Code)
			}
			fmt.Fprintln(os.Stderr, "cairn:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Agent probes must never run a real tool: only fake tools, which
	// tests name by absolute path, are found.
	agentLookPath = func(name string) (string, error) {
		if !filepath.IsAbs(name) {
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	}
	os.Exit(m.Run())
}

const cairnTestMainEnv = "CAIRN_CLI_TEST_MAIN"

// selfScript writes an executable that runs this test binary as cairn.
func selfScript(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "cairn")
	script := "#!/bin/sh\n" + cairnTestMainEnv + "=1 exec '" + strings.ReplaceAll(exe, "'", `'\''`) + "' \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestRebootResume: an agent pane survives a simulated reboot (tmux server
// killed, cairn up on an empty server): the pane shows the conversation
// name, the tool is not started while no client is attached, and on the
// first attach (a control-mode client) it is started with resume, not
// start. Runs on an isolated socket; the default server is never used.
func TestRebootResume(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	dir, _ := wsTestEnv(t)
	t.Setenv("SHELL", "/bin/sh")
	socket := fmt.Sprintf("cairn-test-agents-%d-%d", os.Getpid(), cliSocketSeq.Add(1))
	useRealTmux(t, socket)
	self := selfScript(t)
	oldExe := exePath
	t.Cleanup(func() { exePath = oldExe })
	exePath = func() (string, error) { return self, nil }
	tool := writeFakeAdapter(t, dir, "")
	addProject(t, "demo", dir)

	tm := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() {
		path, err := tm("display-message", "-p", "#{socket_path}")
		_, _ = tm("kill-server")
		if err == nil && filepath.Base(path) == socket {
			_ = os.Remove(path)
		}
	})
	start := func() {
		t.Helper()
		if out, err := tm("-f", "/dev/null", "new-session", "-d", "-s", "cairn-test-holder"); err != nil {
			t.Fatalf("start test server: %v: %s", err, out)
		}
	}
	starts := func() []string {
		var out []string
		lines := strings.Split(toolLog(t, tool), "\n")
		for i, l := range lines {
			if l == "ARGV:" && i+2 < len(lines) {
				out = append(out, strings.TrimSpace(lines[i+1])+" "+strings.TrimSpace(lines[i+2]))
			}
		}
		return out
	}
	paneText := func() string {
		ids, err := tm("list-panes", "-a", "-F", "#{pane_id} #{@cairn_p}")
		if err != nil {
			return ""
		}
		for _, l := range strings.Split(ids, "\n") {
			if id, key, _ := strings.Cut(l, " "); key == "demo/agents/1" {
				out, _ := tm("capture-pane", "-p", "-t", id)
				return out
			}
		}
		return ""
	}

	start()
	mustRun(t, "", "--socket", socket, "up")
	mustRun(t, "", "--socket", socket, "agents", "new", "fake", "--project", "demo", "--name", "reboot me", "--now")
	c := onlyConv(t, dir)
	waitFor(t, "the first start", 10*time.Second, func() bool { return len(starts()) == 1 })
	if got := starts()[0]; got != "start "+c.NativeID {
		t.Fatalf("first launch %q", got)
	}
	waitFor(t, "the menu after exit", 10*time.Second, func() bool { return strings.Contains(paneText(), "r resume") })

	// Reboot.
	_, _ = tm("kill-server")
	waitFor(t, "the server to exit", 5*time.Second, func() bool { _, err := tm("has-session"); return err != nil })
	start()
	mustRun(t, "", "--socket", socket, "up")
	waitFor(t, "the pane to show the name", 10*time.Second, func() bool { return strings.Contains(paneText(), "reboot me") })
	time.Sleep(1500 * time.Millisecond) // three attach polls
	if got := starts(); len(got) != 1 {
		t.Fatalf("tool started without an attached client: %v", got)
	}

	// First attach: a control-mode client.
	cl := exec.Command("tmux", "-L", socket, "-C", "attach-session", "-t", "=demo")
	stdin, err := cl.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cl.Process.Kill(); _ = cl.Wait() })
	waitFor(t, "the resume after attach", 10*time.Second, func() bool { return len(starts()) == 2 })
	if got := starts()[1]; got != "resume "+c.NativeID {
		t.Fatalf("after reboot the tool ran %q, want resume", got)
	}
}

// Review I2 on a real server: rm one agent pane, undo, up. Every pane's
// position marker must belong to the conversation it shows.
func TestUndoRmRealTmuxKeepsIdentity(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	dir, _ := wsTestEnv(t)
	t.Setenv("SHELL", "/bin/sh")
	socket := fmt.Sprintf("cairn-test-agents-%d-%d", os.Getpid(), cliSocketSeq.Add(1))
	useRealTmux(t, socket)
	self := selfScript(t)
	oldExe := exePath
	t.Cleanup(func() { exePath = oldExe })
	exePath = func() (string, error) { return self, nil }
	writeFakeAdapter(t, dir, "")
	addProject(t, "demo", dir)
	tm := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() {
		path, err := tm("display-message", "-p", "#{socket_path}")
		_, _ = tm("kill-server")
		if err == nil && filepath.Base(path) == socket {
			_ = os.Remove(path)
		}
	})
	if out, err := tm("-f", "/dev/null", "new-session", "-d", "-s", "cairn-test-holder"); err != nil {
		t.Fatalf("start test server: %v: %s", err, out)
	}
	mustRun(t, "", "--socket", socket, "up")
	mustRun(t, "", "--socket", socket, "agents", "new", "fake", "--project", "demo", "--name", "one")
	mustRun(t, "", "--socket", socket, "agents", "new", "fake", "--project", "demo", "--name", "two")
	ids := map[string]string{}
	l, _ := testStore(dir).List(conv.Filter{})
	for _, c := range l {
		ids[c.Name] = c.ID
	}
	mustRun(t, "", "--socket", socket, "agents", "rm", "one")
	mustRun(t, "", "--socket", socket, "undo")
	mustRun(t, "", "--socket", socket, "up")
	out, err := tm("list-panes", "-a", "-F", "#{@cairn_p} #{@cairn_agent}")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, a, _ := strings.Cut(line, " "); strings.HasPrefix(k, "demo/agents/") {
			got[k] = a
		}
	}
	want := map[string]string{"demo/agents/1": ids["one"], "demo/agents/2": ids["two"]}
	if len(got) != 2 || got["demo/agents/1"] != want["demo/agents/1"] || got["demo/agents/2"] != want["demo/agents/2"] {
		t.Fatalf("panes %v, want %v", got, want)
	}
	if out := mustRun(t, "", "--socket", socket, "plan", "demo"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan:\n%s", out)
	}
}
