package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/fsx"
)

const testSelf = "/opt/cairn/bin/cairn"

// agentsTestEnv is wsTestEnv plus a fake tool adapter "fake" (pre-assigned
// ids, native delete) and project demo rooted in the temp dir.
func agentsTestEnv(t *testing.T) (dir string, fd *fakeDriver, tool string) {
	t.Helper()
	dir, fd = wsTestEnv(t)
	old := exePath
	t.Cleanup(func() { exePath = old })
	exePath = func() (string, error) { return testSelf, nil }
	tool = writeFakeAdapter(t, dir, "")
	addProject(t, "demo", dir)
	return dir, fd, tool
}

func writeFakeAdapter(t *testing.T, dir, extra string) string {
	t.Helper()
	bin := t.TempDir()
	tool := testtool.Write(t, bin, testtool.Spec{Name: "fake"})
	toml := `name = "fake"
binary = "fake"

[capabilities]
preassign_id = true
resume_by_id = true
delete = true
hooks = "none"

[commands]
start = ["` + tool + `", "start", "{id}", "--name", "{name}"]
resume = ["` + tool + `", "resume", "{native_id}"]
delete = ["` + tool + `", "delete", "{native_id}"]
` + extra
	p := filepath.Join(dir, "config/cairn/tools/fake.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return tool
}

func testStore(dir string) conv.Store {
	return conv.Store{Dir: filepath.Join(dir, "state/cairn/agents"), Root: fsx.Root{Dir: "/"}}
}

func onlyConv(t *testing.T, dir string) conv.Conversation {
	t.Helper()
	l, err := testStore(dir).List(conv.Filter{Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 1 {
		t.Fatalf("want 1 conversation, got %+v", l)
	}
	return l[0]
}

func projectText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config/cairn/projects/demo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func toolLog(t *testing.T, tool string) string {
	t.Helper()
	b, err := os.ReadFile(tool + ".log")
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewCreatesRecordAndPane(t *testing.T) {
	dir, fd, _ := agentsTestEnv(t)
	mustRun(t, "", "up")
	before := len(historyEvents(t))
	out := mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "fix login")
	c := onlyConv(t, dir)
	if c.Name != "fix login" || c.Tool != "fake" || c.Project != "demo" || c.Cwd != dir || c.Pane != "demo/agents/1" {
		t.Fatalf("record %+v", c)
	}
	if len(c.NativeID) != 36 || c.State != conv.StateStarting {
		t.Fatalf("native id %q state %q", c.NativeID, c.State)
	}
	if !strings.Contains(projectText(t, dir), `agent = "`+c.ID+`"`) {
		t.Fatalf("binding missing:\n%s", projectText(t, dir))
	}
	evs := historyEvents(t)
	if len(evs) != before+1 || evs[0].Kind != "agents.new" {
		t.Fatalf("history %+v", evs)
	}
	if !fd.called("NewWindow ") {
		t.Fatalf("agents window not created: %v", fd.calls)
	}
	p := fd.pane("demo/agents/1")
	if p == nil || !slices.Equal(p.argv, []string{testSelf, "agents", "pane", c.ID}) {
		t.Fatalf("pane %+v\ncalls %v", p, fd.calls)
	}
	if !strings.Contains(out, "fix login") {
		t.Fatalf("out:\n%s", out)
	}
	// A second agent splits the same window.
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "second")
	if fd.pane("demo/agents/2") == nil || !fd.called("SplitPane ") {
		t.Fatalf("second pane: %v", fd.calls)
	}
}

func TestNewWithoutRunningSessionOnlyRecords(t *testing.T) {
	dir, fd, _ := agentsTestEnv(t)
	out := mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--now")
	if m := fd.mutations(); len(m) != 0 {
		t.Fatalf("tmux changed without a running session: %v", m)
	}
	if !strings.Contains(out, "cairn open demo") {
		t.Fatalf("out:\n%s", out)
	}
	c := onlyConv(t, dir)
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/agents/start", c.ID)); err != nil {
		t.Fatalf("--now did not request a start: %v", err)
	}
}

func TestNewRejectsUnknownToolAndProject(t *testing.T) {
	agentsTestEnv(t)
	if _, err := runHost(t, "", "agents", "new", "nope", "--project", "demo"); err == nil {
		t.Fatal("unknown tool accepted")
	}
	if _, err := runHost(t, "", "agents", "new", "fake", "--project", "nope"); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestRmReversibleByUndo(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "keep me")
	c := onlyConv(t, dir)
	out := mustRun(t, "", "agents", "rm", c.ID)
	if !strings.Contains(out, "cairn undo") {
		t.Fatalf("out:\n%s", out)
	}
	if l, _ := testStore(dir).List(conv.Filter{Archived: true}); len(l) != 0 {
		t.Fatalf("still listed: %+v", l)
	}
	if strings.Contains(projectText(t, dir), c.ID) {
		t.Fatal("binding kept after rm")
	}
	if ls := mustRun(t, "", "agents", "ls"); strings.Contains(ls, "keep me") {
		t.Fatalf("ls shows removed:\n%s", ls)
	}
	mustRun(t, "", "undo")
	back := onlyConv(t, dir)
	if back.ID != c.ID || back.Name != "keep me" {
		t.Fatalf("after undo %+v", back)
	}
	if !strings.Contains(projectText(t, dir), c.ID) {
		t.Fatal("binding not restored by undo")
	}
}

func TestRmNativeNeedsConfirmation(t *testing.T) {
	dir, _, tool := agentsTestEnv(t)
	mustRun(t, "", "agents", "new", "fake", "--project", "demo")
	c := onlyConv(t, dir)
	out, err := runHost(t, "", "agents", "rm", c.ID, "--native")
	if err == nil {
		t.Fatalf("native delete ran without confirmation:\n%s", out)
	}
	if !strings.Contains(out, "not undoable") {
		t.Fatalf("not labelled:\n%s", out)
	}
	if strings.Contains(toolLog(t, tool), "delete") {
		t.Fatal("delete command ran")
	}
	onlyConv(t, dir) // still there
	if _, err := runHost(t, "", "agents", "rm", c.ID, "--transcript"); err == nil {
		t.Fatal("--transcript without --yes accepted")
	}
	out = mustRun(t, "", "agents", "rm", c.ID, "--native", "--yes")
	if !strings.Contains(toolLog(t, tool), "delete\n  "+c.NativeID) {
		t.Fatalf("delete not run:\n%s", toolLog(t, tool))
	}
	if !strings.Contains(out, "not undoable") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestRmNativeUnsupported(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	mustRun(t, "", "agents", "new", "codex", "--project", "demo")
	c := onlyConv(t, dir)
	if out, err := runHost(t, "", "agents", "rm", c.ID, "--native", "--yes"); err == nil || !strings.Contains(out+err.Error(), "native delete") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	onlyConv(t, dir)
}

func TestRmRefusesRunningConversation(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	mustRun(t, "", "agents", "new", "fake", "--project", "demo")
	c := onlyConv(t, dir)
	rel, err := testStore(dir).AcquireLock("fake", c.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	for _, args := range [][]string{{"agents", "rm", c.ID}, {"agents", "archive", c.ID}} {
		if out, err := runHost(t, "", args...); err == nil || !strings.Contains(out+err.Error(), "in use") {
			t.Fatalf("%v: err=%v out=%s", args, err, out)
		}
	}
	onlyConv(t, dir)
}

func putConvs(t *testing.T, dir string, cs ...conv.Conversation) {
	t.Helper()
	s := testStore(dir)
	for _, c := range cs {
		if c.CreatedAt.IsZero() {
			c.CreatedAt = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
		}
		if err := s.Put(c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLsFilters(t *testing.T) {
	dir, _ := wsTestEnv(t)
	arch := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	putConvs(t, dir,
		conv.Conversation{ID: "01A", Name: "alpha", Tool: "claude", Project: "demo", State: conv.StateWorking, Tags: []string{"bug"}},
		conv.Conversation{ID: "01B", Name: "beta", Tool: "codex", Project: "api", Task: "t1", State: conv.StateWaiting},
		conv.Conversation{ID: "01C", Name: "gamma", Tool: "claude", Project: "api", State: conv.StateIdle, ArchivedAt: &arch},
	)
	names := func(args ...string) string {
		out := mustRun(t, "", append([]string{"agents", "ls"}, args...)...)
		var got []string
		for _, n := range []string{"alpha", "beta", "gamma"} {
			if strings.Contains(out, n) {
				got = append(got, n)
			}
		}
		return strings.Join(got, ",")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "alpha,beta"},
		{[]string{"--project", "api"}, "beta"},
		{[]string{"--project", "api", "--archived"}, "beta,gamma"},
		{[]string{"--tool", "claude"}, "alpha"},
		{[]string{"--state", "waiting"}, "beta"},
		{[]string{"--tag", "bug"}, "alpha"},
		{[]string{"--task", "t1"}, "beta"},
	} {
		if got := names(c.args...); got != c.want {
			t.Errorf("ls %v = %q, want %q", c.args, got, c.want)
		}
	}
	var js []map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, "", "agents", "ls", "--json", "--archived")), &js); err != nil {
		t.Fatal(err)
	}
	if len(js) != 3 || js[0]["id"] != "01A" || js[2]["archived"] != true {
		t.Fatalf("json %v", js)
	}
	if out := mustRun(t, "", "agents", "ls", "--json", "--project", "none"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty json %q", out)
	}
}

func TestTagAddRemove(t *testing.T) {
	dir, _ := wsTestEnv(t)
	putConvs(t, dir, conv.Conversation{ID: "01A", Name: "alpha", Tool: "claude", Tags: []string{"old"}})
	mustRun(t, "", "agents", "tag", "alpha", "+bug", "+ui", "-old")
	if c := onlyConv(t, dir); !slices.Equal(c.Tags, []string{"bug", "ui"}) {
		t.Fatalf("tags %v", c.Tags)
	}
	mustRun(t, "", "agents", "tag", "01A", "-bug", "+ui")
	if c := onlyConv(t, dir); !slices.Equal(c.Tags, []string{"ui"}) {
		t.Fatalf("tags %v", c.Tags)
	}
	mustRun(t, "", "undo")
	if c := onlyConv(t, dir); !slices.Equal(c.Tags, []string{"bug", "ui"}) {
		t.Fatalf("after undo %v", c.Tags)
	}
	if _, err := runHost(t, "", "agents", "tag", "01A", "+a b"); err == nil {
		t.Fatal("tag with a space accepted")
	}
}

func TestNameSetAndUndo(t *testing.T) {
	dir, _ := wsTestEnv(t)
	putConvs(t, dir, conv.Conversation{ID: "01A", Name: "alpha", Tool: "claude"})
	mustRun(t, "", "agents", "name", "01A", "login fix", "--purpose", "fix the login redirect")
	if c := onlyConv(t, dir); c.Name != "login fix" || c.Purpose != "fix the login redirect" {
		t.Fatalf("%+v", c)
	}
	mustRun(t, "", "undo")
	if c := onlyConv(t, dir); c.Name != "alpha" || c.Purpose != "" {
		t.Fatalf("after undo %+v", c)
	}
}

func TestNameSuggestHeuristic(t *testing.T) {
	dir, _ := wsTestEnv(t)
	tr := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"summary","summary":"x"}
{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Please fix the login redirect loop on Safari. It started after the cookie change."}]}}
{"type":"assistant","message":{"role":"assistant","content":"ok"}}
`
	if err := os.WriteFile(tr, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	putConvs(t, dir, conv.Conversation{ID: "01A", Name: "claude-abc", Tool: "claude", TranscriptRef: tr})
	out := mustRun(t, "", "agents", "name", "01A")
	if !strings.Contains(out, "login redirect loop safari") || !strings.Contains(out, "--apply") {
		t.Fatalf("out:\n%s", out)
	}
	if c := onlyConv(t, dir); c.Name != "claude-abc" {
		t.Fatalf("applied without --apply: %+v", c)
	}
	mustRun(t, "", "agents", "name", "01A", "--apply")
	c := onlyConv(t, dir)
	if c.Name != "fix login redirect loop safari" || !strings.HasPrefix(c.Purpose, "fix the login redirect loop on Safari") {
		t.Fatalf("%+v", c)
	}
}

func TestArchiveHidesAndResumeRestores(t *testing.T) {
	dir, fd, _ := agentsTestEnv(t)
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "arch")
	c := onlyConv(t, dir)
	mustRun(t, "", "agents", "archive", "arch")
	if l, _ := testStore(dir).List(conv.Filter{}); len(l) != 0 {
		t.Fatalf("archived still listed: %+v", l)
	}
	if strings.Contains(projectText(t, dir), c.ID) {
		t.Fatal("binding kept after archive")
	}
	mustRun(t, "", "up")
	mustRun(t, "", "agents", "resume", "arch")
	back := onlyConv(t, dir)
	if back.ArchivedAt != nil || !strings.Contains(projectText(t, dir), c.ID) {
		t.Fatalf("resume did not unarchive/rebind: %+v", back)
	}
	if fd.pane(back.Pane) == nil || !fd.called("SelectPane ") || !fd.called("exec tmux attach-session") {
		t.Fatalf("no jump: %v", fd.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/agents/start", c.ID)); err != nil {
		t.Fatalf("resume did not request a start: %v", err)
	}
	mustRun(t, "", "undo")
	if l, _ := testStore(dir).List(conv.Filter{}); len(l) != 0 {
		t.Fatal("undo of resume did not re-archive")
	}
}

func TestRmUnbindsLivePaneAndRekeys(t *testing.T) {
	dir, fd, _ := agentsTestEnv(t)
	mustRun(t, "", "up")
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "one")
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "two")
	// Ids made in the same millisecond do not sort by creation: pick by name.
	l, _ := testStore(dir).List(conv.Filter{})
	first, second := l[0], l[1]
	if first.Name != "one" {
		first, second = second, first
	}
	p2 := fd.pane("demo/agents/2")
	mustRun(t, "", "agents", "rm", first.ID)
	if fd.pane("demo/agents/2") != nil || fd.pane("demo/agents/1") != p2 {
		t.Fatalf("panes not rekeyed: %v", fd.calls)
	}
	if out := mustRun(t, "", "plan", "demo"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("plan after rm:\n%s", out)
	}
	if c, _, _ := testStore(dir).Get(second.ID); c.Pane != "demo/agents/1" {
		t.Fatalf("record pane %q", c.Pane)
	}
}

// historyEvents lists the history newest first.
func historyEvents(t *testing.T) []struct{ Kind string } {
	t.Helper()
	s, err := openHistory()
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.List(0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]struct{ Kind string }, len(evs))
	for i, e := range evs {
		out[i].Kind = e.Kind
	}
	return out
}

func TestResumeBoundTwiceKeepsOneBinding(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "once")
	c := onlyConv(t, dir)
	mustRun(t, "", "agents", "resume", "once")
	mustRun(t, "", "agents", "resume", c.ID)
	if n := strings.Count(projectText(t, dir), c.ID); n != 1 {
		t.Fatalf("bound %d times:\n%s", n, projectText(t, dir))
	}
}
