package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mustRun runs cairn and fails the test on an error.
func mustRun(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := runHost(t, stdin, args...)
	if err != nil {
		t.Fatalf("cairn %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func addProject(t *testing.T, id, root string, extra ...string) {
	t.Helper()
	mustRun(t, "", append([]string{"project", "add", id, "--root", root}, extra...)...)
}

func TestPlanEmptySnapshotListsCreates(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "web", dir)
	addProject(t, "api", dir)
	out := mustRun(t, "", "plan")
	for _, want := range []string{"api", "web", "+", "create-session"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "api") > strings.Index(out, "web") {
		t.Fatalf("projects not in id order:\n%s", out)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("plan changed tmux: %v", m)
	}

	js := mustRun(t, "", "plan", "--json")
	var got planJSON
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("plan --json: %v\n%s", err, js)
	}
	var kinds []string
	for _, op := range got.Ops {
		kinds = append(kinds, op.Project+":"+op.Kind)
		if op.Destructive {
			t.Fatalf("destructive op on empty snapshot: %+v", op)
		}
	}
	if want := []string{"api:create-session", "web:create-session"}; !slices.Equal(kinds, want) {
		t.Fatalf("ops = %v, want %v", kinds, want)
	}
	if got.LoadErrors == nil || len(got.LoadErrors) != 0 {
		t.Fatalf("load_errors must be an empty array: %s", js)
	}
}

func TestPlanUnknownProject(t *testing.T) {
	_, _ = wsTestEnv(t)
	out, err := runHost(t, "", "plan", "nope")
	if err == nil || !strings.Contains(err.Error()+out, "cairn project add") {
		t.Fatalf("plan nope: %v\n%s", err, out)
	}
}

func TestApplyYesRunsReconcilerOrderAndSecondApplyIsNoop(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "web", dir, "--template", "dev")
	addProject(t, "api", dir)
	out := mustRun(t, "", "apply", "--yes")
	if strings.Contains(out, "Apply?") {
		t.Fatalf("non-destructive apply asked:\n%s", out)
	}
	var news []string
	for _, c := range fd.mutations() {
		if strings.HasPrefix(c, "New") || strings.HasPrefix(c, "SplitPane") {
			news = append(news, c)
		}
	}
	want := []string{
		"NewSession api api/main",
		"NewSession web web/code",
		"SplitPane %5 h 35 web/code/2",
		"NewWindow $4 web/run",
	}
	if !slices.Equal(news, want) {
		t.Fatalf("driver calls = %v, want %v", news, want)
	}

	fd.calls = nil
	out = mustRun(t, "", "apply")
	if !strings.Contains(out, "nothing to do") || len(fd.mutations()) != 0 {
		t.Fatalf("second apply: %v\n%s", fd.mutations(), out)
	}
	if out := mustRun(t, "", "plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after apply:\n%s", out)
	}
}

// Review Focus 3: a project whose root does not exist is reported; the
// other project still applies and the command succeeds.
func TestMissingRootReportedWhileOtherProjectApplies(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "api", dir)
	gone := filepath.Join(dir, "unmounted", "web")
	addProject(t, "web", gone)

	out, err := runHost(t, "", "apply", "--yes")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "⊘") || !strings.Contains(out, gone) || !strings.Contains(out, "does not exist") {
		t.Fatalf("missing root not reported:\n%s", out)
	}
	if !fd.called("NewSession api") || fd.called("NewSession web") {
		t.Fatalf("calls: %v", fd.calls)
	}

	fd.sessions = nil
	out, err = runHost(t, "", "up")
	if err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2 projects · 1 created · 0 unchanged · 1 skipped · 0 failed") ||
		!strings.Contains(out, "web") || !strings.Contains(out, gone) {
		t.Fatalf("up summary:\n%s", out)
	}
}

// up never removes anything, even where apply --prune would.
func TestUpIsNeverDestructive(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "up")
	s := fd.session(t, "demo")
	s.windows = append(s.windows, &fWindow{id: "@90", key: "demo/old", name: "old",
		panes: []*fPane{{id: "%90", key: "demo/old/1", pid: 990, cmd: "bash"}}})
	fd.idle = true

	fd.calls = nil
	out, err := runHost(t, "", "up", "--prune")
	if err != nil || fd.called("Kill") {
		t.Fatalf("up --prune: %v %v\n%s", err, fd.calls, out)
	}
	if !strings.Contains(out, "demo/old") {
		t.Fatalf("extra window not reported:\n%s", out)
	}

	out, err = runHost(t, "y\n", "apply", "--prune")
	if exitCode(err) != 1 || !strings.Contains(out, "destructive changes need confirmation: re-run with --yes") ||
		strings.Contains(out, "Apply?") || fd.called("Kill") {
		t.Fatalf("apply --prune without a terminal: %v %v\n%s", err, fd.calls, out)
	}

	interactive = func(io.Reader) bool { return true }
	out, err = runHost(t, "n\n", "apply", "--prune")
	if err != nil || !strings.Contains(out, "Apply? [y/N]") || !strings.Contains(out, "nothing changed") || fd.called("Kill") {
		t.Fatalf("apply --prune answered no: %v %v\n%s", err, fd.calls, out)
	}
	interactive = func(io.Reader) bool { return false }

	out, err = runHost(t, "", "apply", "--prune", "--yes")
	if err != nil || !fd.called("KillWindow @90") || strings.Contains(out, "Apply?") {
		t.Fatalf("apply --prune --yes: %v %v\n%s", err, fd.calls, out)
	}
}

// Review Focus 5: inside tmux, open switches the client to the session id
// and never attaches (no nesting).
func TestOpenInsideTmuxSwitchesClient(t *testing.T) {
	dir, fd := wsTestEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-0/lab,1,0")
	t.Setenv("TMUX_PANE", "%0")
	fd.socketPath = "/tmp/tmux-0/lab"
	addProject(t, "demo", dir)
	out := mustRun(t, "", "--socket", "lab", "open", "demo")
	sid := fd.session(t, "demo").id
	want := "SwitchClientHere " + sid + " TMUX=/tmp/tmux-0/lab,1,0 TMUX_PANE=%0"
	if !fd.called(want) || fd.called("exec") {
		t.Fatalf("calls: %v\n%s", fd.calls, out)
	}
	// Already running: no new objects, still a switch.
	fd.calls = nil
	mustRun(t, "", "--socket", "lab", "open", "demo")
	if fd.called("New") || !fd.called(want) || fd.called("exec") {
		t.Fatalf("second open: %v", fd.calls)
	}
}

// Inside a client of another tmux server, open touches no client at all.
func TestOpenInsideOtherServerRefuses(t *testing.T) {
	dir, fd := wsTestEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
	t.Setenv("TMUX_PANE", "%0")
	fd.socketPath = "/tmp/tmux-0/lab"
	addProject(t, "demo", dir)
	out, err := runHost(t, "", "--socket", "lab", "open", "demo")
	if exitCode(err) != 1 || fd.called("SwitchClient") || fd.called("exec") ||
		!strings.Contains(out, "you are inside another tmux server (/tmp/tmux-0/default); run: tmux -L lab attach -t =demo") {
		t.Fatalf("open: %v %v\n%s", err, fd.calls, out)
	}
}

func TestOpenOutsideTmuxExecsAttach(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "--socket", "lab", "open", "demo")
	sid := fd.session(t, "demo").id
	if fd.socket != "lab" || !fd.called("exec tmux -L lab attach-session -t "+sid) || fd.called("SwitchClient") {
		t.Fatalf("socket %q calls %v", fd.socket, fd.calls)
	}
}

func TestOpenRefusesInactiveMissingRootAndUnknown(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "project", "archive", "demo")
	out, err := runHost(t, "", "open", "demo")
	if exitCode(err) != 1 || !strings.Contains(out, "archived") || fd.called("exec") || fd.called("SwitchClient") {
		t.Fatalf("open archived: %v %v\n%s", err, fd.calls, out)
	}

	addProject(t, "web", filepath.Join(dir, "missing"))
	out, err = runHost(t, "", "open", "web")
	if exitCode(err) != 1 || !strings.Contains(out, "does not exist") || fd.called("exec") {
		t.Fatalf("open missing root: %v %v\n%s", err, fd.calls, out)
	}

	out, err = runHost(t, "", "open", "nope")
	if err == nil || !strings.Contains(err.Error()+out, "cairn project add nope") {
		t.Fatalf("open unknown: %v\n%s", err, out)
	}
}

func TestStatusJSON(t *testing.T) {
	dir, fd := wsTestEnv(t)
	addProject(t, "api", dir, "--template", "dev")
	addProject(t, "web", dir)
	mustRun(t, "", "apply", "--yes", "api")
	s := fd.session(t, "api")
	s.windows = s.windows[:1]         // window "run" missing
	s.windows[0].panes[0].dead = true // nvim exited
	s.windows = append(s.windows, &fWindow{id: "@80", key: "api/old", panes: []*fPane{{id: "%80", key: "api/old/1", cmd: "bash"}}})

	out := mustRun(t, "", "status", "--json")
	var raw map[string][]map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	rows := raw["projects"]
	if len(rows) != 2 {
		t.Fatalf("rows: %s", out)
	}
	keys := []string{"id", "session", "running", "windows", "missing_windows", "missing_panes", "dead_panes", "extra", "problem"}
	for _, k := range keys {
		if _, ok := rows[0][k]; !ok {
			t.Fatalf("row lacks %q: %s", k, out)
		}
	}
	want := fmt.Sprint(map[string]any{"id": "api", "session": "api", "running": true, "windows": 2.0,
		"missing_windows": 1.0, "missing_panes": 0.0, "dead_panes": 1.0, "extra": 1.0, "problem": ""})
	if got := fmt.Sprint(rows[0]); got != want {
		t.Fatalf("api row = %s\nwant %s", got, want)
	}
	if rows[1]["id"] != "web" || rows[1]["running"] != false || rows[1]["missing_windows"] != 1.0 {
		t.Fatalf("web row: %v", rows[1])
	}
	if plain := mustRun(t, "", "status"); !strings.Contains(plain, "api") || !strings.Contains(plain, "stopped") {
		t.Fatalf("status:\n%s", plain)
	}
}

func TestSocketFromFlagAndEnv(t *testing.T) {
	_, fd := wsTestEnv(t)
	t.Setenv("CAIRN_TMUX_SOCKET", "fromenv")
	mustRun(t, "", "plan")
	if fd.socket != "fromenv" {
		t.Fatalf("socket = %q", fd.socket)
	}
	mustRun(t, "", "--socket", "fromflag", "plan")
	if fd.socket != "fromflag" {
		t.Fatalf("socket = %q", fd.socket)
	}
}

func TestMissingTmuxBinary(t *testing.T) {
	dir, _ := wsTestEnv(t)
	newDriver = realDriver
	addProject(t, "demo", dir)
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{{"plan"}, {"apply", "--yes"}, {"up"}, {"open", "demo"}, {"status"}} {
		out, err := runHost(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "tmux not found in PATH") {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out := mustRun(t, "", "project", "ls")
	if !strings.Contains(out, "demo") || !strings.Contains(out, "?") {
		t.Fatalf("project ls without tmux:\n%s", out)
	}
}

var cliSocketSeq atomic.Int64

// TestRealTmuxRoundTrip runs the commands against a real tmux server on an
// isolated socket started with "-f /dev/null" and SHELL=/bin/sh; the
// default socket is never used.
func TestRealTmuxRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	_, _ = wsTestEnv(t)
	newDriver = realDriver
	t.Setenv("SHELL", "/bin/sh")
	socket := fmt.Sprintf("cairn-test-cli-%d-%d", os.Getpid(), cliSocketSeq.Add(1))
	tm := func(args ...string) (string, error) {
		c := exec.Command("tmux", append([]string{"-L", socket}, args...)...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	start := func() {
		t.Helper()
		if out, err := tm("-f", "/dev/null", "new-session", "-d", "-s", "cairn-test-holder"); err != nil {
			t.Fatalf("start test server: %v: %s", err, out)
		}
	}
	t.Cleanup(func() {
		path, err := tm("display-message", "-p", "#{socket_path}")
		_, _ = tm("kill-server")
		if p := strings.TrimSpace(path); err == nil && filepath.Base(p) == socket {
			_ = os.Remove(p)
		}
	})
	start()

	root := t.TempDir()
	mustRun(t, "", "project", "add", "demo", "--root", root, "--template", "service")
	if out := mustRun(t, "", "--socket", socket, "plan"); !strings.Contains(out, "create-session") {
		t.Fatalf("plan:\n%s", out)
	}
	mustRun(t, "", "--socket", socket, "apply", "--yes")
	if out := mustRun(t, "", "--socket", socket, "plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after apply:\n%s", out)
	}

	// "Reboot": kill the test server, start an empty one, restore with up.
	_, _ = tm("kill-server")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := tm("has-session"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test server did not exit")
		}
	}
	start()
	out := mustRun(t, "", "--socket", socket, "up")
	if !strings.Contains(out, "1 project · 1 created · 0 unchanged · 0 failed") {
		t.Fatalf("up:\n%s", out)
	}
	if out := mustRun(t, "", "--socket", socket, "plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after up:\n%s", out)
	}
	js := mustRun(t, "", "--socket", socket, "status", "--json")
	if !strings.Contains(js, `"running": true`) {
		t.Fatalf("status: %s", js)
	}
}

// --json writes exactly one JSON document to stdout, also for an inactive
// project and with a broken project file.
func TestJSONOutputIsOneDocument(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "project", "archive", "demo")
	bad := filepath.Join(testDirs(t).Config, "projects", "broken.toml")
	if err := os.WriteFile(bad, []byte("id = ["), 0o644); err != nil {
		t.Fatal(err)
	}
	decode := func(args ...string) map[string]json.RawMessage {
		t.Helper()
		stdout, _, err := runSplit(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		dec := json.NewDecoder(strings.NewReader(stdout))
		var m map[string]json.RawMessage
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stdout)
		}
		if dec.More() {
			t.Fatalf("%v: more than one document:\n%s", args, stdout)
		}
		return m
	}
	m := decode("plan", "demo", "--json")
	if string(m["ops"]) != "[]" || !strings.Contains(string(m["notes"]), "project demo is archived") ||
		!strings.Contains(string(m["load_errors"]), "broken.toml") {
		t.Fatalf("plan demo --json: %v", m)
	}
	m = decode("plan", "--json")
	if string(m["notes"]) != "[]" || string(m["ops"]) != "[]" {
		t.Fatalf("plan --json: %s %s", m["notes"], m["ops"])
	}
	m = decode("status", "--json")
	if string(m["projects"]) != "[]" || !strings.Contains(string(m["load_errors"]), "broken.toml") {
		t.Fatalf("status --json: %v", m)
	}
}
