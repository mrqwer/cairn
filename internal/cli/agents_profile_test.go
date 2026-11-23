package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/execx"
)

// profileTestEnv is agentsTestEnv with an adapter "pfake" that splices
// profile arguments and maps model, permission_mode and sandbox, plus a
// profiles file with careful, yolo and open.
func profileTestEnv(t *testing.T) (dir, tool string) {
	t.Helper()
	dir, _, _ = agentsTestEnv(t)
	tool = testtool.Write(t, t.TempDir(), testtool.Spec{Name: "pfake"})
	writeFile(t, filepath.Join(dir, "config/cairn/tools/pfake.toml"), `name = "pfake"
binary = "pfake"

[capabilities]
preassign_id = true
resume_by_id = true
hooks = "none"

[commands]
start = ["`+tool+`", "start", "{id}", "{profile_args...}"]
resume = ["`+tool+`", "resume", "{native_id}", "{profile_args...}"]

[profile_flags]
model = ["--model", "{value}"]
permission_mode = ["--permission-mode", "{value}"]
sandbox = ["--sandbox", "{value}"]

[profile_defaults]
sandbox = "workspace-write"
`)
	writeFile(t, filepath.Join(dir, "config/cairn/profiles.toml"), `[profiles.careful]
model = "opus"
effort = "high"

[profiles.yolo]
permission_mode = "bypassPermissions"

[profiles.open]
sandbox = "danger-full-access"
`)
	return dir, tool
}

func appendProject(t *testing.T, dir, text string) {
	t.Helper()
	p := filepath.Join(dir, "config/cairn/projects/demo.toml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, string(b)+text)
}

// launchArgs runs the pane of c once (--now) and returns the argv the fake
// tool logged ("" when it did not start) and the pane output.
func launchArgs(t *testing.T, tool string, c conv.Conversation) (string, string) {
	t.Helper()
	_ = os.Remove(tool + ".log")
	out, err := runHost(t, "", "agents", "pane", c.ID, "--now")
	if err != nil {
		t.Fatalf("pane: %v\n%s", err, out)
	}
	log := toolLog(t, tool)
	argv, _, _ := strings.Cut(log, "ENV:")
	return argv, out
}

func TestAgentsNewProfileAppliedAtLaunch(t *testing.T) {
	dir, tool := profileTestEnv(t)
	out := mustRun(t, "", "agents", "new", "pfake", "--project", "demo", "--profile", "careful")
	if !strings.Contains(out, "effort") {
		t.Fatalf("a key the tool cannot express is reported:\n%s", out)
	}
	c := onlyConv(t, dir)
	if c.Profile != "careful" || c.Risk {
		t.Fatalf("record %+v", c)
	}
	argv, _ := launchArgs(t, tool, c)
	for _, want := range []string{"  --model\n  opus\n", "  --sandbox\n  workspace-write\n"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("launch argv lacks %q:\n%s", want, argv)
		}
	}
	// Unknown and malformed profile names create nothing.
	for _, bad := range []string{"nope", "-x", "Bad Name"} {
		if o, err := runHost(t, "", "agents", "new", "pfake", "--project", "demo", "--profile", bad); err == nil {
			t.Fatalf("--profile %q accepted:\n%s", bad, o)
		}
	}
	if l, _ := testStore(dir).List(conv.Filter{Archived: true}); len(l) != 1 {
		t.Fatalf("refused profiles must not create records: %d", len(l))
	}
}

func TestAgentsProjectDefaultProfile(t *testing.T) {
	dir, tool := profileTestEnv(t)
	appendProject(t, dir, "# keep this comment\n")
	mustRun(t, "", "agents", "profile", "default", "careful", "--project", "demo")
	text := projectText(t, dir)
	if !strings.Contains(text, "# keep this comment") || !strings.Contains(text, `default_profile = "careful"`) {
		t.Fatalf("project file:\n%s", text)
	}
	mustRun(t, "", "agents", "new", "pfake", "--project", "demo")
	argv, _ := launchArgs(t, tool, onlyConv(t, dir))
	if !strings.Contains(argv, "  --model\n  opus\n") {
		t.Fatalf("project default not applied:\n%s", argv)
	}
	mustRun(t, "", "undo") // the new agent
	mustRun(t, "", "undo") // the default
	if strings.Contains(projectText(t, dir), "default_profile") {
		t.Fatalf("undo must restore the project file:\n%s", projectText(t, dir))
	}
	for _, bad := range []string{"nope", "-x"} {
		if o, err := runHost(t, "", "agents", "profile", "default", bad, "--project", "demo"); err == nil {
			t.Fatalf("default %q accepted:\n%s", bad, o)
		}
	}
	mustRun(t, "", "agents", "profile", "default", "careful", "--project", "demo")
	mustRun(t, "", "agents", "profile", "default", "--clear", "--project", "demo")
	if text := projectText(t, dir); strings.Contains(text, "default_profile") || !strings.Contains(text, "# keep this comment") {
		t.Fatalf("--clear:\n%s", text)
	}
}

func TestAgentsRiskyProfileRefusedWithoutAck(t *testing.T) {
	dir, tool := profileTestEnv(t)
	for _, p := range []string{"yolo", "open"} {
		out, err := runHost(t, "", "agents", "new", "pfake", "--project", "demo", "--profile", p)
		if err == nil || !strings.Contains(err.Error(), "i_understand_the_risk") {
			t.Fatalf("%s accepted: %v\n%s", p, err, out)
		}
	}
	if l, _ := testStore(dir).List(conv.Filter{Archived: true}); len(l) != 0 {
		t.Fatalf("nothing may be created: %+v", l)
	}
	// The task's profile is checked before any git work.
	out, err := runHost(t, "", "task", "new", "Risky work", "--project", "demo", "--tool", "pfake", "--here", "--profile", "yolo")
	if err == nil || !strings.Contains(err.Error(), "i_understand_the_risk") {
		t.Fatalf("task new accepted a risky profile: %v\n%s", err, out)
	}
	// Acknowledged, then withdrawn: the launch refuses and the tool never
	// starts.
	appendProject(t, dir, "\n[agents]\ni_understand_the_risk = true\n")
	mustRun(t, "", "agents", "new", "pfake", "--project", "demo", "--profile", "yolo")
	c := onlyConv(t, dir)
	p := filepath.Join(dir, "config/cairn/projects/demo.toml")
	writeFile(t, p, strings.Replace(projectText(t, dir), "i_understand_the_risk = true", "i_understand_the_risk = false", 1))
	argv, paneOut := launchArgs(t, tool, c)
	if argv != "" || !strings.Contains(paneOut, "i_understand_the_risk") {
		t.Fatalf("withdrawn acknowledgement must stop the launch: %q\n%s", argv, paneOut)
	}
}

func TestAgentsRiskyAllowedWithAckShowsBadge(t *testing.T) {
	dir, tool := profileTestEnv(t)
	appendProject(t, dir, "\n[agents]\ni_understand_the_risk = true\n")
	out := mustRun(t, "", "agents", "new", "pfake", "--project", "demo", "--profile", "yolo", "--name", "wild")
	if !strings.Contains(out, "!") || !strings.Contains(out, "bypassPermissions") {
		t.Fatalf("new must warn:\n%s", out)
	}
	c := onlyConv(t, dir)
	if !c.Risk {
		t.Fatalf("record must store risk: %+v", c)
	}
	argv, _ := launchArgs(t, tool, c)
	if !strings.Contains(argv, "  --permission-mode\n  bypassPermissions\n") {
		t.Fatalf("acknowledged launch:\n%s", argv)
	}
	ls := mustRun(t, "", "agents", "ls")
	line := ""
	for _, l := range strings.Split(ls, "\n") {
		if strings.Contains(l, "wild") {
			line = l
		}
	}
	if !strings.HasPrefix(line, "!") {
		t.Fatalf("ls badge:\n%s", ls)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, "", "agents", "ls", "--json")), &rows); err != nil || len(rows) != 1 || rows[0]["risk"] != true || rows[0]["profile"] != "yolo" {
		t.Fatalf("ls --json %v %+v", err, rows)
	}
	st := mustRun(t, "", "status")
	if !strings.Contains(st, "! wild") {
		t.Fatalf("status badge:\n%s", st)
	}
	var sj struct {
		Agents struct {
			Risky []map[string]any `json:"risky"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "", "status", "--json")), &sj); err != nil || len(sj.Agents.Risky) != 1 {
		t.Fatalf("status --json risky %v %+v", err, sj)
	}
	fs := decodeFindings(t, mustRun(t, "", "agents", "doctor", "--json"))
	f, ok := findingOf(fs, "profiles", "risk")
	if !ok || f.Level != "warn" || !strings.Contains(f.Detail, "demo") {
		t.Fatalf("doctor risk finding %+v in %+v", f, fs)
	}
}

func TestAgentsProfileLs(t *testing.T) {
	dir, _ := profileTestEnv(t)
	writeFile(t, filepath.Join(dir, "config/cairn/profiles.toml"), `[profiles.careful]
model = "opus"

[profiles.typo]
modle = "x"
`)
	out := mustRun(t, "", "agents", "profile", "ls")
	for _, want := range []string{"careful", "pfake", "--model opus", "typo", "modle"} {
		if !strings.Contains(out, want) {
			t.Fatalf("profile ls lacks %q:\n%s", want, out)
		}
	}
	fs := decodeFindings(t, mustRun(t, "", "agents", "doctor", "--json"))
	if f, ok := findingOf(fs, "profiles", "load"); !ok || f.Level != "warn" {
		t.Fatalf("doctor load finding %+v", f)
	}
}

func TestAgentsUsage(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	s := testStore(dir)
	for _, c := range []conv.Conversation{
		{ID: "01A", Tool: "claude", NativeID: "0b6f3b1e-6f5c-4c43-9d0a-1d2b3c4d5e6f", Project: "demo", Task: "T1"},
		{ID: "01B", Tool: "codex", NativeID: "7c1d2e3f-0a1b-4c2d-8e3f-405162738495", Project: "demo"},
	} {
		if err := s.Put(c); err != nil {
			t.Fatal(err)
		}
	}
	oldLook, oldRun := usageLookPath, usageRunner
	t.Cleanup(func() { usageLookPath, usageRunner = oldLook, oldRun })
	usageLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	usageRunner = &execx.FakeRunner{}
	out := mustRun(t, "", "agents", "usage")
	if !strings.Contains(out, "unavailable") {
		t.Fatalf("missing ccusage:\n%s", out)
	}
	usageLookPath = func(string) (string, error) { return "/usr/bin/ccusage", nil }
	usageRunner = &execx.FakeRunner{Responses: map[string]execx.Response{
		"/usr/bin/ccusage session --json --offline": {Out: []byte(`{"sessions":[{"sessionId":"0b6f3b1e-6f5c-4c43-9d0a-1d2b3c4d5e6f","totalTokens":1000,"totalCost":1.5}]}`)},
	}}
	out = mustRun(t, "", "agents", "usage", "--by", "tool")
	if !strings.Contains(out, "claude") || !strings.Contains(out, "1000") || !strings.Contains(out, "~$1.50") || !strings.Contains(out, "estimate") {
		t.Fatalf("usage by tool:\n%s", out)
	}
	var js struct {
		Estimate bool `json:"estimate"`
		Groups   []struct {
			Key     string  `json:"key"`
			Tokens  int64   `json:"tokens"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "", "agents", "usage", "--by", "task", "--json")), &js); err != nil || !js.Estimate {
		t.Fatalf("usage --json %v %+v", err, js)
	}
	if len(js.Groups) != 2 || js.Groups[0].Key != "(no task)" && js.Groups[0].Key != "T1" {
		t.Fatalf("groups %+v", js.Groups)
	}
	usageRunner = &execx.FakeRunner{Responses: map[string]execx.Response{
		"/usr/bin/ccusage session --json --offline": {Err: errors.New("exit status 1")},
	}}
	out, err := runHost(t, "", "agents", "usage")
	if err != nil || !strings.Contains(out, "warning") {
		t.Fatalf("a failing ccusage is a warning: %v\n%s", err, out)
	}
	if o, err := runHost(t, "", "agents", "usage", "--by", "nope"); err == nil {
		t.Fatalf("--by nope accepted:\n%s", o)
	}
}

func TestSetDefaultProfileTextKeepsLines(t *testing.T) {
	src := "id = \"demo\" # mine\n\n[agents] # risky stuff\ni_understand_the_risk = false\n\n[[windows]]\nkey = \"main\"\n"
	got := setDefaultProfileText(src, "careful")
	want := "id = \"demo\" # mine\n\n[agents] # risky stuff\ndefault_profile = \"careful\"\ni_understand_the_risk = false\n\n[[windows]]\nkey = \"main\"\n"
	if got != want {
		t.Fatalf("set:\n%s", got)
	}
	if again := setDefaultProfileText(got, "fast"); again != strings.Replace(want, "careful", "fast", 1) {
		t.Fatalf("replace:\n%s", again)
	}
	if cleared := setDefaultProfileText(got, ""); cleared != src {
		t.Fatalf("clear:\n%s", cleared)
	}
	// A default_profile key in another table is not touched.
	other := "[[windows]]\ndefault_profile = \"x\"\n"
	if got := setDefaultProfileText(other, "careful"); !strings.HasPrefix(got, other) || !strings.HasSuffix(got, "[agents]\ndefault_profile = \"careful\"\n") {
		t.Fatalf("other table:\n%s", got)
	}
}

func TestStatusTmuxMarksRiskyWork(t *testing.T) {
	dir := hostTestEnv(t)
	noTmux(t)
	putLive(t, testStore(dir), conv.Conversation{ID: "01A", State: conv.StateWorking, Risk: true}, true)
	out, _, err := runSplit(t, "status", "--tmux")
	if err != nil || out != "! 1 working\n" {
		t.Fatalf("tmux badge %q %v", out, err)
	}
}
