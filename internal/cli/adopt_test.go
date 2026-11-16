package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
)

// liveMarkers lists every marker of the fake server as "id=value".
func liveMarkers(s *tmux.Snapshot) []string {
	var out []string
	for _, se := range s.Sessions {
		if se.Key != "" {
			out = append(out, se.ID+"="+se.Key)
		}
		for _, w := range se.Windows {
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

// apiOnly is the marker set of liveSnapshot before any adoption.
var apiOnly = []string{"$3=api", "@4=api/main"}

func TestAdoptMarksSessionWritesConfigAndPlansNothing(t *testing.T) {
	root, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	fd.idle = true
	out := mustRun(t, "", "adopt", "web", "--root", root)
	path := projectPath(t, "web")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# cairn adopt: tmux session \"web\" -> project web", "nothing to do", "Run: cairn plan web", path,
		"marked session web: 1 session, 2 windows, 3 panes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "is not managed by cairn") {
		t.Fatalf("name clash note for the adopted session itself:\n%s", out)
	}
	// Markers first, session -> windows -> panes, then nothing else.
	want := []string{
		"SetOption session $1 @cairn_s web",
		"SetOption window @1 @cairn_w web/code",
		"SetOption window @2 @cairn_w web/run",
		"SetOption pane %1 @cairn_p web/code/1",
		"SetOption pane %2 @cairn_p web/code/2",
		"SetOption pane %3 @cairn_p web/run/1",
	}
	if m := fd.mutations(); !slices.Equal(m, want) {
		t.Fatalf("mutations\n got %q\nwant %q", m, want)
	}
	p, err := workspace.Decode(data)
	if err != nil || p.ID != "web" || len(p.Windows) != 2 {
		t.Fatalf("file %v:\n%s", err, data)
	}
	evs := histEvents(t)
	if len(evs) != 1 || evs[0].Kind != "adopt" || len(evs[0].Files) != 1 || evs[0].Files[0].Path != path {
		t.Fatalf("history: %+v", evs)
	}
	// The adopted session is managed: plan and capture agree.
	if out := mustRun(t, "", "plan", "web"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan:\n%s", out)
	}
	if _, _, err := runSplit(t, "adopt", "web", "--id", "web2"); err == nil || !strings.Contains(err.Error(), "--release web") {
		t.Fatalf("second adopt: %v", err)
	}

	// Release removes the markers, keeps the file and touches nothing else.
	fd.calls = nil
	out = mustRun(t, "", "adopt", "--release", "web")
	if !strings.Contains(out, "removed 6 markers of project web") {
		t.Fatalf("release output:\n%s", out)
	}
	if m := liveMarkers(fd.live); !slices.Equal(m, apiOnly) {
		t.Fatalf("markers after release %v", m)
	}
	for _, c := range fd.mutations() {
		if !strings.HasPrefix(c, "UnsetOption ") {
			t.Fatalf("release did %q", c)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("release removed the project file: %v", err)
	}
	if out := mustRun(t, "", "plan", "web"); !strings.Contains(out, "rename it") {
		t.Fatalf("plan after release:\n%s", out)
	}
}

func TestAdoptDryRunChangesNothing(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	out := mustRun(t, "", "adopt", "web", "--dry-run")
	for _, want := range []string{`id = "web"`, "# mark: pane %2 @cairn_p = web/code/2", "# mark: session $1 @cairn_s = web", "dry run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("dry run touched tmux: %v", m)
	}
	if _, err := os.Stat(projectPath(t, "web")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the project: %v", err)
	}
	if evs := histEvents(t); len(evs) != 0 {
		t.Fatalf("history: %+v", evs)
	}
}

func TestAdoptRefusesExistingProjectBeforeTmux(t *testing.T) {
	root, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	mustRun(t, "", "project", "add", "web", "--root", root)
	before, _ := os.ReadFile(projectPath(t, "web"))
	_, _, err := runSplit(t, "adopt", "web", "--id", "web")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err %v", err)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("tmux called: %v", fd.calls)
	}
	// Without --id the id comes from the session: tmux is only read.
	if _, _, err := runSplit(t, "adopt", "web"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err %v", err)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("tmux changed: %v", m)
	}
	if after, _ := os.ReadFile(projectPath(t, "web")); string(after) != string(before) {
		t.Fatal("project overwritten")
	}
}

func TestAdoptRefusals(t *testing.T) {
	_, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"adopt"}, "session name"},
		{[]string{"adopt", "api"}, "already managed"},
		{[]string{"adopt", "nope"}, "no tmux session"},
		{[]string{"adopt", "web", "--id", "Bad Id"}, "invalid project id"},
		{[]string{"adopt", "web", "--release", "web"}, "--release"},
		{[]string{"adopt", "--release", "Bad Id"}, "invalid project id"},
		{[]string{"adopt", "--release", "web"}, "no live tmux session"},
	} {
		if _, _, err := runSplit(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err %v, want %q", tc.args, err, tc.want)
		}
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("tmux changed: %v", m)
	}
}

func TestAdoptSkippedLinesNeedForce(t *testing.T) {
	root, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	fd.live.Skipped = []string{"@9\t%9\tbroken"}
	_, _, err := runSplit(t, "adopt", "web", "--root", root)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err %v", err)
	}
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("tmux changed: %v", m)
	}
	mustRun(t, "", "adopt", "web", "--root", root, "--force")
	if _, err := os.Stat(projectPath(t, "web")); err != nil {
		t.Fatal(err)
	}
}

// A mark that fails midway leaves no marker, no project file and no history.
func TestAdoptMarkFailureRollsBack(t *testing.T) {
	root, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	fd.failOption = "%2" // the second pane of window code
	_, _, err := runSplit(t, "adopt", "web", "--root", root)
	if err == nil || !strings.Contains(err.Error(), "pane %2 (web/code/2)") || !strings.Contains(err.Error(), "were removed") {
		t.Fatalf("err %v", err)
	}
	if m := liveMarkers(fd.live); !slices.Equal(m, apiOnly) {
		t.Fatalf("markers left %v", m)
	}
	if _, err := os.Stat(projectPath(t, "web")); !os.IsNotExist(err) {
		t.Fatalf("project file: %v", err)
	}
	if evs := histEvents(t); len(evs) != 0 {
		t.Fatalf("history: %+v", evs)
	}
}

// A config write or history failure after the markers removes them again
// (and the file).
func TestAdoptWriteFailureRemovesMarkers(t *testing.T) {
	root, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	d := testDirs(t)
	if err := os.MkdirAll(d.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Config, "projects"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSplit(t, "adopt", "web", "--root", root); err == nil {
		t.Fatal("adopt succeeded without a projects directory")
	}
	if m := liveMarkers(fd.live); !slices.Equal(m, apiOnly) {
		t.Fatalf("markers left %v", m)
	}
	if err := os.Remove(filepath.Join(d.Config, "projects")); err != nil {
		t.Fatal(err)
	}

	// History cannot be written: the file goes too.
	if err := os.MkdirAll(filepath.Join(d.State, "history.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := runSplit(t, "adopt", "web", "--root", root)
	if err == nil || !strings.Contains(err.Error(), "history") {
		t.Fatalf("err %v", err)
	}
	if m := liveMarkers(fd.live); !slices.Equal(m, apiOnly) {
		t.Fatalf("markers left %v", m)
	}
	if _, err := os.Stat(projectPath(t, "web")); !os.IsNotExist(err) {
		t.Fatalf("project file left: %v", err)
	}
}

// Adoption through the real tmux client issues only list commands and
// set-option: nothing is renamed, moved, resized, killed or restarted.
func TestAdoptIssuesOnlyListAndSetOption(t *testing.T) {
	root, _ := wsTestEnv(t)
	r := &modelRunner{snap: liveSnapshot()}
	newDriver = func(socket string) (wsDriver, error) { return tmux.Client{R: r, Socket: socket}, nil }
	mustRun(t, "", "--socket", "s", "adopt", "web", "--root", root)
	mustRun(t, "", "--socket", "s", "adopt", "--release", "web")
	sets := 0
	for _, c := range r.calls {
		if c[0] != "tmux" || c[1] != "-L" || c[2] != "s" {
			t.Fatalf("call %q", c)
		}
		switch c[3] {
		case "list-sessions", "list-windows", "list-panes", "display-message":
		case "set":
			sets++
		default:
			t.Fatalf("adopt ran tmux %q", c[3:])
		}
	}
	if sets != 12 { // 6 marks set, 6 removed
		t.Fatalf("%d set-option calls: %q", sets, r.calls)
	}
	if m := liveMarkers(r.snap); !slices.Equal(m, apiOnly) {
		t.Fatalf("markers %v", m)
	}
}

// modelRunner answers the snapshot list commands from snap and applies
// set-option (set and -u) to its markers; any other command fails.
type modelRunner struct {
	snap  *tmux.Snapshot
	calls [][]string
}

func (m *modelRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, append([]string{name}, args...))
	if len(args) < 3 {
		return nil, fmt.Errorf("unexpected %q", args)
	}
	args = args[2:] // -L s
	var b strings.Builder
	switch args[0] {
	case "list-sessions":
		for _, s := range m.snap.Sessions {
			fmt.Fprintf(&b, "%s\t%s\t%s\t0\n", s.ID, s.Name, s.Key)
		}
	case "list-windows":
		for _, s := range m.snap.Sessions {
			for _, w := range s.Windows {
				fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t%s\t%d\t%d\n", s.ID, w.ID, w.Index, w.Name, w.Key, w.W, w.H)
			}
		}
	case "list-panes":
		for _, s := range m.snap.Sessions {
			for _, w := range s.Windows {
				for _, p := range w.Panes {
					fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t\t%d\t%d\t%d\t%d\t0\t\t%s\t%s\n", w.ID, p.ID, p.Index, p.Key, p.Left, p.Top, p.W, p.H, p.Cwd, p.Cmd)
				}
			}
		}
	case "set":
		unset := slices.Contains(args, "-u")
		scope := "session"
		switch {
		case slices.Contains(args, "-w"):
			scope = "window"
		case slices.Contains(args, "-p"):
			scope = "pane"
		}
		i := slices.Index(args, "-t")
		target := args[i+1]
		fd := &fakeDriver{live: m.snap}
		k := fd.liveKey(scope, target)
		if k == nil {
			return []byte("no such object"), fmt.Errorf("exit status 1")
		}
		if unset {
			*k = ""
		} else {
			*k = args[i+3]
		}
	default:
		return nil, fmt.Errorf("unexpected tmux %q", args)
	}
	return []byte(b.String()), nil
}

// Real tmux (isolated socket, no configuration): a hand-built session with
// the dev shape is adopted without changing it, behaves as managed (a
// killed pane is re-created in place by apply) and is unmanaged again after
// --release.
func TestAdoptRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	_, _ = wsTestEnv(t)
	newDriver = realDriver
	t.Setenv("SHELL", "/bin/sh")
	// A stand-in "nvim" (a copy of cat) so no real editor is started.
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

	socket := fmt.Sprintf("cairn-test-adopt-%d-%d", os.Getpid(), cliSocketSeq.Add(1))
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

	// Session "demo" in the dev shape, built by hand: no cairn markers.
	root := t.TempDir()
	size := []string{"-x", fmt.Sprint(tmux.DefaultWidth), "-y", fmt.Sprint(tmux.DefaultHeight)}
	editor := tm(append(append([]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "demo", "-n", "code"}, size...), "-c", root, "nvim")...)
	shell := tm("split-window", "-P", "-F", "#{pane_id}", "-h", "-l", "35%", "-t", editor, "-c", root)
	tm("new-window", "-t", "=demo:", "-n", "run", "-c", root)
	client := tmux.NewClient(socket)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if tm("display-message", "-p", "-t", editor, "#{pane_current_command}") == "nvim" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("editor never started")
		}
	}
	snapDemo := func() tmux.Session {
		t.Helper()
		snap, err := client.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range snap.Sessions {
			if s.Name == "demo" {
				return s
			}
		}
		t.Fatal("session demo gone")
		return tmux.Session{}
	}
	before := snapDemo()

	out := mustRun(t, "", "--socket", socket, "adopt", "demo")
	if !strings.Contains(out, "nothing to do") || !strings.Contains(out, "Run: cairn plan demo") {
		t.Fatalf("adopt:\n%s", out)
	}
	after := snapDemo()
	if after.Key != "demo" || after.ID != before.ID || after.Name != before.Name || len(after.Windows) != 2 {
		t.Fatalf("after adopt %+v", after)
	}
	wantKeys := map[string]string{editor: "demo/code/1", shell: "demo/code/2"}
	for wi, w := range after.Windows {
		bw := before.Windows[wi]
		if w.ID != bw.ID || w.Name != bw.Name || len(w.Panes) != len(bw.Panes) {
			t.Fatalf("window %d changed: %+v -> %+v", wi, bw, w)
		}
		for pi, p := range w.Panes {
			bp := bw.Panes[pi]
			if p.ID != bp.ID || p.PID != bp.PID || p.Left != bp.Left || p.W != bp.W || p.Cmd != bp.Cmd {
				t.Fatalf("pane changed: %+v -> %+v", bp, p)
			}
			if want, ok := wantKeys[p.ID]; ok && p.Key != want {
				t.Fatalf("pane %s marker %q, want %q", p.ID, p.Key, want)
			}
		}
	}
	if remain := tm("show-options", "-p", "-t", editor, "-v", "remain-on-exit"); remain == "on" {
		t.Fatal("adopt changed remain-on-exit")
	}
	if out := mustRun(t, "", "--socket", socket, "plan", "demo"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after adopt:\n%s", out)
	}

	// Kill the shell pane: apply re-creates it beside the editor.
	tm("kill-pane", "-t", shell)
	if out := mustRun(t, "", "--socket", socket, "plan", "demo"); !strings.Contains(out, "create-pane") {
		t.Fatalf("plan after kill:\n%s", out)
	}
	mustRun(t, "", "--socket", socket, "apply", "demo", "--yes")
	code := snapDemo().Windows[0]
	if len(code.Panes) != 2 || code.Panes[0].ID != editor || code.Panes[1].Key != "demo/code/2" {
		t.Fatalf("code window after apply %+v", code)
	}
	near := func(a, b int) bool { return a-b <= 2 && b-a <= 2 }
	for i, p := range code.Panes {
		bp := before.Windows[0].Panes[i]
		if !near(p.Left, bp.Left) || !near(p.W, bp.W) || !near(p.Top, bp.Top) || !near(p.H, bp.H) {
			t.Fatalf("pane %d geometry %d,%d %dx%d, was %d,%d %dx%d", i, p.Left, p.Top, p.W, p.H, bp.Left, bp.Top, bp.W, bp.H)
		}
	}
	if out := mustRun(t, "", "--socket", socket, "plan", "demo"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after apply:\n%s", out)
	}

	// Release: the objects stay, the markers go, the name clash is back.
	out = mustRun(t, "", "--socket", socket, "adopt", "--release", "demo")
	if !strings.Contains(out, "removed 6 markers") {
		t.Fatalf("release:\n%s", out)
	}
	rel := snapDemo()
	if rel.ID != before.ID || len(rel.Windows) != 2 || len(rel.Windows[0].Panes) != 2 || rel.Key != "" {
		t.Fatalf("after release %+v", rel)
	}
	for _, w := range rel.Windows {
		for _, p := range w.Panes {
			if w.Key != "" || p.Key != "" {
				t.Fatalf("marker left: %+v", w)
			}
		}
	}
	if out := mustRun(t, "", "--socket", socket, "plan", "demo"); !strings.Contains(out, "rename it") {
		t.Fatalf("plan after release:\n%s", out)
	}
}

// cairn undo reverts an adoption's config only; the markers stay until
// adopt --release.
func TestAdoptUndoRemovesConfigOnly(t *testing.T) {
	root, fd := wsTestEnv(t)
	fd.live = liveSnapshot()
	mustRun(t, "", "adopt", "web", "--root", root)
	mustRun(t, "", "undo")
	if _, err := os.Stat(projectPath(t, "web")); !os.IsNotExist(err) {
		t.Fatalf("undo left the config: %v", err)
	}
	if m := liveMarkers(fd.live); len(m) != len(apiOnly)+6 {
		t.Fatalf("markers %v", m)
	}
	if out := mustRun(t, "", "adopt", "--release", "web"); !strings.Contains(out, "removed 6 markers") || strings.Contains(out, "config stays") {
		t.Fatalf("release:\n%s", out)
	}
}
