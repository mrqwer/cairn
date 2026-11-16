package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
)

// liveSnapshot is a server with an unmanaged session "web" (editor beside a
// running npm), a session "my_proj" (tmux's spelling of "my.proj") and a
// managed session "api".
func liveSnapshot() *tmux.Snapshot {
	pane := func(idx, left, w int, cwd, cmd string) tmux.Pane {
		return tmux.Pane{ID: fmt.Sprintf("%%%d", idx+1), Index: idx, PID: 100 + idx, Left: left, W: w, H: 50, Cwd: cwd, Cmd: cmd}
	}
	return &tmux.Snapshot{Sessions: []tmux.Session{
		{ID: "$1", Name: "web", Windows: []tmux.Window{
			{ID: "@1", Name: "code", W: 200, H: 50, Panes: []tmux.Pane{
				pane(0, 0, 130, "/home/u/web", "nvim"), pane(1, 131, 69, "/home/u/web/app", "npm")}},
			{ID: "@2", Name: "run", W: 200, H: 50, Panes: []tmux.Pane{pane(0, 0, 200, "/home/u/web", "zsh")}},
		}},
		{ID: "$2", Name: "my_proj", Windows: []tmux.Window{
			{ID: "@3", Name: "main", W: 200, H: 50, Panes: []tmux.Pane{pane(0, 0, 200, "/home/u/p", "zsh")}},
		}},
		{ID: "$3", Name: "api", Key: "api", Windows: []tmux.Window{
			{ID: "@4", Name: "main", Key: "api/main", W: 200, H: 50, Panes: []tmux.Pane{pane(0, 0, 200, "/home/u/api", "zsh")}},
		}},
	}}
}

func TestCapturePrintsProposalAndNeverTouchesTmux(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	fd.idle = true
	out, _, err := runSplit(t, "capture", "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id = "web"`, `root = "/home/u/web"`, `role = "editor"`, `run = ["nvim"]`,
		`cwd = "app"`, `size = 35`, `window code pane 2: running "npm"`, "--write"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	// Plain output is a valid project file as it stands (header and notes
	// are comments).
	p, err := workspace.Decode([]byte(out))
	if err != nil {
		t.Fatalf("plain output is not TOML: %v\n%s", err, out)
	}
	if p.ID != "web" || len(p.Windows) != 2 {
		t.Fatalf("decoded: %+v", p)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("capture touched tmux: %v", m)
	}
	if _, err := os.Stat(projectPath(t, "web")); !os.IsNotExist(err) {
		t.Fatalf("project file written without --write: %v", err)
	}
	if evs := histEvents(t); len(evs) != 0 {
		t.Fatalf("history: %+v", evs)
	}
}

func TestCaptureWriteRecordsHistoryAndRefusesExisting(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	fd.idle = true
	out := mustRun(t, "", "capture", "web", "--id", "web-copy", "--write")
	path := projectPath(t, "web-copy")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Run: cairn plan web-copy") || !strings.Contains(out, path) {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(string(data), `session = "web"`) || strings.HasPrefix(string(data), "#") {
		t.Fatalf("file:\n%s", data)
	}
	// The live session keeps the name the managed one will get.
	if !strings.Contains(out, `a live tmux session named "web" exists and is not managed by cairn; cairn will not touch it. Rename it (tmux rename-session -t web <new>) before running cairn apply, or adopt it (cairn adopt web, coming next). Closing it would kill its running programs.`) {
		t.Fatalf("no clash note:\n%s", out)
	}
	if probs := checkProjectData("web-copy", data, "/home/u"); len(probs) != 0 {
		t.Fatalf("written file invalid: %v", probs)
	}
	evs := histEvents(t)
	if len(evs) != 1 || evs[0].Kind != "capture" || len(evs[0].Files) != 1 {
		t.Fatalf("history: %+v", evs)
	}
	fc := evs[0].Files[0]
	if fc.Path != path || !fc.AfterExists || fc.BeforeExists || string(fc.After) != string(data) {
		t.Fatalf("file change: %+v", fc)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("capture touched tmux: %v", m)
	}

	_, err = runHost(t, "", "capture", "web", "--id", "web-copy", "--write")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second write: %v", err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(data) {
		t.Fatal("existing project overwritten")
	}

	// Undo removes the captured file.
	mustRun(t, "", "undo")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("undo left %s: %v", path, err)
	}
}

func TestCaptureRefusals(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"capture", "api"}, "already managed"},
		{[]string{"capture", "we"}, `no tmux session named "we"`},
		{[]string{"capture", "my.proj"}, `"my_proj"`},
		{[]string{"capture", "web", "--id", "Bad/id"}, "invalid project id"},
		{[]string{"capture"}, "session name"},
		{[]string{"capture", "--list", "web"}, "--list"},
		{[]string{"capture", "--list", "--write"}, "--list"},
	} {
		out, err := runHost(t, "", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("capture touched tmux: %v", m)
	}
	if entries, _ := os.ReadDir(filepath.Dir(projectPath(t, "x"))); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

func TestCaptureRootFlag(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	out := mustRun(t, "", "capture", "web", "--root", "/home/u")
	if !strings.Contains(out, `root = "/home/u"`) || !strings.Contains(out, `cwd = "web/app"`) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestCaptureList(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	out := mustRun(t, "", "capture", "--list")
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		rows = append(rows, strings.Join(strings.Fields(l), " "))
	}
	for _, want := range []string{"web 2 3", "my_proj 1 1"} {
		if !strings.Contains(strings.Join(rows, "\n"), want) {
			t.Fatalf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "api") {
		t.Fatalf("managed session listed:\n%s", out)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("list touched tmux: %v", m)
	}
	fd.live = &tmux.Snapshot{}
	if out := mustRun(t, "", "capture", "--list"); !strings.Contains(out, "no unmanaged tmux sessions") {
		t.Fatalf("empty list:\n%s", out)
	}
}

// TestCaptureRealTmuxRoundTrip builds an unmanaged session by hand on an
// isolated server ("-f /dev/null", SHELL=/bin/sh; the default socket is
// never used), captures it, applies the capture and compares the two
// sessions pane by pane.
func TestCaptureRealTmuxRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	_, _ = wsTestEnv(t)
	newDriver = realDriver
	t.Setenv("SHELL", "/bin/sh")
	// A stand-in "nvim" (a copy of cat, waiting on the terminal) so no real
	// editor or user configuration is started.
	bin := t.TempDir()
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat not in PATH")
	}
	catBin, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "nvim"), catBin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	socket := fmt.Sprintf("cairn-test-capture-%d-%d", os.Getpid(), cliSocketSeq.Add(1))
	tm := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		c := func(args ...string) (string, error) {
			out, err := exec.Command("tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
		path, err := c("display-message", "-p", "#{socket_path}")
		_, _ = c("kill-server")
		if err == nil && filepath.Base(path) == socket {
			_ = os.Remove(path)
		}
	})
	tm("-f", "/dev/null", "new-session", "-d", "-s", "cairn-test-holder")

	root := t.TempDir()
	for _, d := range []string{"api", "web", "with space"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, "", "project", "add", "a", "--root", root, "--template", "dev")
	mustRun(t, "", "--socket", socket, "apply", "--yes")

	// Session b mirrors the dev template by hand, plus a window "work"
	// whose third pane is split from the first (not the last) pane.
	size := []string{"-x", fmt.Sprint(tmux.DefaultWidth), "-y", fmt.Sprint(tmux.DefaultHeight)}
	editor := tm(append(append([]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "b", "-n", "code"}, size...), "-c", root, "nvim")...)
	tm("split-window", "-h", "-l", "35%", "-t", editor, "-c", root)
	tm("new-window", "-t", "=b:", "-n", "run", "-c", filepath.Join(root, "api"))
	first := tm("new-window", "-P", "-F", "#{pane_id}", "-t", "=b:", "-n", "work", "-c", root)
	sleeper := tm("split-window", "-P", "-F", "#{pane_id}", "-v", "-l", "40%", "-t", first, "-c", filepath.Join(root, "web"), "sleep 1000")
	tm("split-window", "-h", "-l", "30%", "-t", first, "-c", filepath.Join(root, "with space"))
	for pane, want := range map[string]string{editor: "nvim", sleeper: "sleep"} {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if tm("display-message", "-p", "-t", pane, "#{pane_current_command}") == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pane %s never ran %s", pane, want)
			}
		}
	}

	// A login shell is busy (profile scripts) for a moment after it starts;
	// wait until every shell pane sits at its prompt.
	client := tmux.NewClient(socket)
	for _, l := range strings.Split(tm("list-panes", "-s", "-t", "=b", "-F", "#{pane_pid} #{pane_current_command}"), "\n") {
		var pid int
		var cmd string
		if _, err := fmt.Sscan(l, &pid, &cmd); err != nil || !tmux.IsShell(cmd) {
			continue
		}
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if idle, err := client.PaneIdle(pid); err == nil && idle {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("shell %d never became idle", pid)
			}
		}
	}

	out := mustRun(t, "", "--socket", socket, "capture", "b", "--id", "b-copy", "--write")
	for _, want := range []string{`session = "b"`, `window work pane 2: running "sleep"`, `a live tmux session named "b" exists and is not managed by cairn`, "Run: cairn plan b-copy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("capture output lacks %q:\n%s", want, out)
		}
	}
	data, err := os.ReadFile(projectPath(t, "b-copy"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := workspace.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != root || len(p.Windows) != 3 || p.Windows[0].Key != "code" || p.Windows[2].Key != "work" {
		t.Fatalf("captured:\n%s", data)
	}
	code, work := p.Windows[0].Panes, p.Windows[2].Panes
	if code[0].Role != workspace.RoleEditor || !reflect.DeepEqual(code[0].Run, []string{"nvim"}) || code[1].Role != "" {
		t.Fatalf("code roles:\n%s", data)
	}
	if d := code[1].Size - 35; d < -1 || d > 1 {
		t.Fatalf("code split size %d:\n%s", code[1].Size, data)
	}
	if work[1].Role != workspace.RoleCommand || work[1].Run != nil || work[1].Cwd != "web" || work[2].Cwd != "with space" {
		t.Fatalf("work panes:\n%s", data)
	}

	// While the unmanaged b exists, the plan reports the name clash.
	if out := mustRun(t, "", "--socket", socket, "plan", "b-copy"); !strings.Contains(out, "rename it") {
		t.Fatalf("plan with clash:\n%s", out)
	}
	tm("rename-session", "-t", "=b", "b-orig")
	mustRun(t, "", "--socket", socket, "apply", "--yes")

	snap, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var orig, managed tmux.Session
	for _, s := range snap.Sessions {
		switch {
		case s.Name == "b-orig":
			orig = s
		case s.Key == "b-copy":
			managed = s
		}
	}
	if managed.Name != "b" || len(orig.Windows) != 3 || len(managed.Windows) != 3 {
		t.Fatalf("sessions: orig %+v\nmanaged %+v", orig, managed)
	}
	near := func(a, b int) bool { return a-b <= 2 && b-a <= 2 }
	for wi, ow := range orig.Windows {
		mw := managed.Windows[wi]
		if mw.Name != ow.Name || mw.Key != "b-copy/"+p.Windows[wi].Key || len(mw.Panes) != len(ow.Panes) {
			t.Fatalf("window %d: %+v vs %+v", wi, mw, ow)
		}
		// pane_index order is traversal order in both sessions, so the
		// panes pair up by index.
		for i, op := range ow.Panes {
			mp := mw.Panes[i]
			if !near(mp.Left, op.Left) || !near(mp.Top, op.Top) || !near(mp.W, op.W) || !near(mp.H, op.H) {
				t.Fatalf("window %s pane %d geometry %d,%d %dx%d vs %d,%d %dx%d", ow.Name, i,
					mp.Left, mp.Top, mp.W, mp.H, op.Left, op.Top, op.W, op.H)
			}
			if mp.Cwd != op.Cwd {
				t.Fatalf("window %s pane %d cwd %q vs %q", ow.Name, i, mp.Cwd, op.Cwd)
			}
		}
	}
	if out := mustRun(t, "", "--socket", socket, "plan"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after apply:\n%s", out)
	}
}

func TestCaptureSkippedLinesNeedForce(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	fd.live.Skipped = []string{"@9\t%9\t0\tbroken line one", "line two", "line three", "line four"}
	stdout, stderr, err := runSplit(t, "capture", "web")
	if err != nil {
		t.Fatal(err)
	}
	const note = "4 tmux lines could not be parsed"
	if !strings.Contains(stdout, "# note: "+note) || !strings.Contains(stdout, "the capture may be incomplete") ||
		!strings.Contains(stderr, note) || strings.Contains(stdout, "line four") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	_, _, err = runSplit(t, "capture", "web", "--write")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("write without --force: %v", err)
	}
	if _, err := os.Stat(projectPath(t, "web")); !os.IsNotExist(err) {
		t.Fatalf("written without --force: %v", err)
	}
	stdout, _, err = runSplit(t, "capture", "web", "--write", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, note) {
		t.Fatalf("stdout:\n%s", stdout)
	}
	if _, err := os.Stat(projectPath(t, "web")); err != nil {
		t.Fatalf("not written with --force: %v", err)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("capture touched tmux: %v", m)
	}
}
