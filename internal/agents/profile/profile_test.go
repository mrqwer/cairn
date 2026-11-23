package profile_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/profile"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

func builtins(t *testing.T) map[string]adapter.Adapter {
	t.Helper()
	as, lerrs, err := adapter.Load(fsx.Root{Dir: t.TempDir()}, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("Load: %v %v", err, lerrs)
	}
	m := map[string]adapter.Adapter{}
	for _, a := range as {
		m[a.Name] = a
	}
	return m
}

// writeAt writes text to rel under a temp root.
func writeAt(t *testing.T, rel, text string) (fsx.Root, xdg.Dirs) {
	t.Helper()
	dir := t.TempDir()
	dirs := xdg.Dirs{Config: "/cfg", State: "/state", Home: "/home/u"}
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return fsx.Root{Dir: dir}, dirs
}

// writeProfiles writes <Config>/agents/profiles.toml under a temp root.
func writeProfiles(t *testing.T, text string) (fsx.Root, xdg.Dirs) {
	t.Helper()
	return writeAt(t, "cfg/agents/profiles.toml", text)
}

const careful = `
# careful work
[profiles.careful]
effort = "high"
sandbox = "read-only"

[profiles.careful.claude]
model = "opus"
permission_mode = "plan"

[profiles.careful.codex]
permission_mode = "on-request"
`

func TestProfileArgsMappedPerTool(t *testing.T) {
	root, dirs := writeProfiles(t, careful)
	ps, lerrs, err := profile.Load(root, dirs)
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("Load: %v %v", err, lerrs)
	}
	p, ok := ps["careful"]
	if !ok || p.Name != "careful" || p.Settings["sandbox"] != "read-only" || p.Tools["claude"]["model"] != "opus" {
		t.Fatalf("profiles %+v", ps)
	}
	as := builtins(t)
	args, unsupported := profile.Args(as["claude"], p)
	if want := []string{"--effort", "high", "--model", "opus", "--permission-mode", "plan"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("claude args %q want %q", args, want)
	}
	if !reflect.DeepEqual(unsupported, []string{"sandbox"}) {
		t.Fatalf("claude unsupported %q", unsupported)
	}
	args, unsupported = profile.Args(as["codex"], p)
	if want := []string{"--ask-for-approval", "on-request", "--sandbox", "read-only"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("codex args %q want %q", args, want)
	}
	if !reflect.DeepEqual(unsupported, []string{"effort"}) {
		t.Fatalf("codex unsupported %q", unsupported)
	}
	for _, n := range []string{"claude", "codex"} {
		if err := profile.Check(as[n], p); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
}

func TestProfileValuesCheckedPerTool(t *testing.T) {
	as := builtins(t)
	p := profile.Profile{Name: "careful", Settings: map[string]string{"permission_mode": "plan"}}
	if err := profile.Check(as["claude"], p); err != nil {
		t.Fatalf("plan is a claude mode: %v", err)
	}
	err := profile.Check(as["codex"], p)
	if err == nil {
		t.Fatal("plan is not a codex approval policy")
	}
	for _, want := range []string{"careful", "codex", "permission_mode", `"plan"`, "on-request", "[profiles.careful.codex]"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	p.Tools = map[string]map[string]string{"codex": {"permission_mode": "never"}}
	if err := profile.Check(as["codex"], p); err != nil {
		t.Fatalf("a per-tool value wins: %v", err)
	}
	// An adapter's allowed values must belong to a mapped key.
	a := as["codex"]
	a.ProfileValues = map[string][]string{"effort": {"x"}}
	found := false
	for _, fe := range a.Validate() {
		found = found || fe.Field == "profile_values.effort"
	}
	if !found {
		t.Fatalf("profile_values for an unmapped key must be invalid: %v", a.Validate())
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	ps, lerrs, err := profile.Load(fsx.Root{Dir: t.TempDir()}, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 || len(ps) != 0 {
		t.Fatalf("got %v %v %v", ps, lerrs, err)
	}
}

func TestLoadReadsOldPath(t *testing.T) {
	root, dirs := writeAt(t, "cfg/profiles.toml", "[profiles.old]\nmodel = \"m\"\n")
	if profile.Path(dirs) != "/cfg/agents/profiles.toml" || profile.OldPath(dirs) != "/cfg/profiles.toml" {
		t.Fatalf("paths %s %s", profile.Path(dirs), profile.OldPath(dirs))
	}
	ps, lerrs, err := profile.Load(root, dirs)
	if err != nil || len(lerrs) != 0 || ps["old"].Settings["model"] != "m" {
		t.Fatalf("old path not read: %v %v %v", ps, lerrs, err)
	}
	if !profile.UsesOldPath(root, dirs) {
		t.Fatal("UsesOldPath")
	}
}

func TestResolutionOrder(t *testing.T) {
	for _, c := range []struct{ conv, task, project, want, src string }{
		{"a", "b", "c", "a", "conversation"},
		{"", "b", "c", "b", "task"},
		{"", "", "c", "c", "project"},
		{"", "", "", "", ""},
	} {
		got, src := profile.Resolve(c.conv, c.task, c.project)
		if got != c.want || src != c.src {
			t.Errorf("Resolve(%q,%q,%q) = %q,%q want %q,%q", c.conv, c.task, c.project, got, src, c.want, c.src)
		}
	}
}

func TestRiskyRefusedWithoutAck(t *testing.T) {
	as := builtins(t)
	yolo := profile.Profile{Name: "yolo", Settings: map[string]string{"permission_mode": "bypassPermissions"}}
	if !profile.Risky(as["claude"], yolo) {
		t.Fatal("claude bypassPermissions must be risky")
	}
	err := profile.Authorize(yolo, as["claude"], workspace.Project{ID: "api"})
	if err == nil {
		t.Fatal("a risky profile must be refused without i_understand_the_risk")
	}
	for _, want := range []string{"yolo", "--permission-mode bypassPermissions", "i_understand_the_risk", "api"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	open := profile.Profile{Name: "open", Settings: map[string]string{"sandbox": "danger-full-access"}}
	if !profile.Risky(as["codex"], open) || profile.Authorize(open, as["codex"], workspace.Project{ID: "api"}) == nil {
		t.Fatal("codex danger-full-access must be risky and refused")
	}
	if profile.Risky(as["claude"], open) {
		t.Fatal("an unmapped key cannot be risky")
	}
	safe := profile.Profile{Name: "careful", Settings: map[string]string{"permission_mode": "plan"}}
	if profile.Risky(as["claude"], safe) || profile.Authorize(safe, as["claude"], workspace.Project{ID: "api"}) != nil {
		t.Fatal("plan is not risky")
	}
	a := as["claude"]
	a.Commands = map[string][]string{"start": {"claude", "--dangerously-skip-permissions", "{profile_args...}"}}
	if !profile.Risky(a, profile.Profile{}) || profile.Authorize(profile.Profile{}, a, workspace.Project{ID: "api"}) == nil {
		t.Fatal("a risky flag in commands.start must need the acknowledgement")
	}
	a = as["claude"]
	a.RiskyFlags = nil
	if !profile.Risky(a, yolo) {
		t.Fatal("known risky flags apply to every adapter")
	}
}

func TestRiskySpellings(t *testing.T) {
	a := adapter.Adapter{Name: "x"}
	for _, args := range [][]string{
		{"x", "-s", "danger-full-access"},
		{"x", "-sdanger-full-access"},
		{"x", "-s=danger-full-access"},
		{"x", "--sandbox=DANGER-FULL-ACCESS"},
		{"x", "-c", `sandbox_mode="danger-full-access"`},
		{"x", "-csandbox_mode=danger-full-access"},
		{"x", "--config", "sandbox_mode='danger-full-access'"},
		{"x", "--config=sandbox_mode=danger-full-access"},
		{"x", "-c", "sandbox_mode = \"danger-full-access\""},
		{"x", "--approve-for-me"},
		{"x", "--dangerously-do-anything"},
		{"x", "--permission-mode=bypasspermissions"},
		{"x", "--yolo"},
	} {
		if len(profile.RiskyArgs(a, args)) == 0 {
			t.Errorf("not risky: %q", args)
		}
	}
	for _, args := range [][]string{
		{"x", "-s", "read-only"},
		{"x", "--sandbox", "workspace-write"},
		{"x", "-c", "notify=[\"cairn\"]"},
		{"x", "--name", "danger-full-access"},
		{"x", "--permission-mode", "plan"},
	} {
		if hits := profile.RiskyArgs(a, args); len(hits) != 0 {
			t.Errorf("risky %q: %v", args, hits)
		}
	}
	// An adapter entry in short or config form is normalised the same way.
	a.RiskyFlags = []string{"-m wild", "-c approval_policy=never"}
	for _, args := range [][]string{{"x", "-mwild"}, {"x", "-c", "approval_policy=\"never\""}} {
		if len(profile.RiskyArgs(a, args)) == 0 {
			t.Errorf("adapter entry missed %q", args)
		}
	}
}

func TestRiskyAllowedWithAckShowsBadge(t *testing.T) {
	as := builtins(t)
	yolo := profile.Profile{Name: "yolo", Settings: map[string]string{"permission_mode": "bypassPermissions"}}
	p := workspace.Project{ID: "api", Agents: workspace.ProjectAgents{IUnderstandTheRisk: true}}
	if err := profile.Authorize(yolo, as["claude"], p); err != nil {
		t.Fatalf("acknowledged project: %v", err)
	}
	if profile.Badge(true) != "!" || profile.Badge(false) != "" {
		t.Fatalf("badge %q %q", profile.Badge(true), profile.Badge(false))
	}
}

func TestUnsupportedKeyReported(t *testing.T) {
	root, dirs := writeProfiles(t, `
[profiles.ok]
model = "opus"
mcp_set = "web"

[profiles.typo]
modle = "opus"

[profiles.global]
i_understand_the_risk = "true"

[profiles.flag]
model = "--dangerously-skip-permissions"

[profiles.number]
effort = 3

[profiles."Bad Name"]
model = "x"

[profiles.badtool.codex]
modle = "x"

[profiles.badtool2."-x"]
model = "x"
`)
	ps, lerrs, err := profile.Load(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ps["ok"]; !ok || len(ps) != 1 {
		t.Fatalf("only ok loads: %+v", ps)
	}
	var msgs []string
	for _, le := range lerrs {
		msgs = append(msgs, le.Error())
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"typo: unknown key modle", "global: unknown key i_understand_the_risk",
		`flag: model: "--dangerously-skip-permissions" must not start with "-"`, "number: effort: must be a string", `"Bad Name"`,
		"badtool: codex.modle: unknown key", `badtool2: "-x"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("load errors lack %q:\n%s", want, all)
		}
	}
	root, dirs = writeProfiles(t, "i_understand_the_risk = true\n")
	if _, lerrs, _ = profile.Load(root, dirs); len(lerrs) != 1 || !strings.Contains(lerrs[0].Error(), "i_understand_the_risk") {
		t.Fatalf("top-level acknowledgement must be refused: %v", lerrs)
	}
	_, unsupported := profile.Args(builtins(t)["agy"], ps["ok"])
	if !reflect.DeepEqual(unsupported, []string{"mcp_set"}) {
		t.Fatalf("unsupported %q", unsupported)
	}
}

// The sandbox flag is passed only when a profile sets it: otherwise the
// tool's own configuration (and its per-project trust) decides.
func TestSandboxOnlyWhenProfileSets(t *testing.T) {
	as := builtins(t)
	if args, _ := profile.Args(as["codex"], profile.Profile{}); len(args) != 0 {
		t.Fatalf("codex without a profile: %q", args)
	}
	args, _ := profile.Args(as["codex"], profile.Profile{Name: "ro", Settings: map[string]string{"sandbox": "read-only"}})
	if want := []string{"--sandbox", "read-only"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("profile sandbox: %q", args)
	}
}

func TestToolSandboxReported(t *testing.T) {
	as := builtins(t)
	root, dirs := writeAt(t, "home/u/.codex/config.toml", "model = \"x\"\nsandbox_mode = \"danger-full-access\"\n\n[projects.\"/home/u/a\"]\ntrust_level = \"untrusted\"\n")
	s, ok, err := profile.ToolSandbox(root, dirs, as["codex"])
	if err != nil || !ok || s.Value != "danger-full-access" || !s.Risky || !strings.HasSuffix(s.File, ".codex/config.toml") {
		t.Fatalf("got %+v %v %v", s, ok, err)
	}
	root, dirs = writeAt(t, "home/u/.codex/config.toml", "model = \"x\"\n")
	if _, ok, err := profile.ToolSandbox(root, dirs, as["codex"]); ok || err != nil {
		t.Fatalf("unset sandbox_mode: %v %v", ok, err)
	}
	if _, ok, err := profile.ToolSandbox(root, dirs, as["claude"]); ok || err != nil {
		t.Fatalf("claude declares no sandbox config: %v %v", ok, err)
	}
	root, dirs = writeAt(t, "home/u/.codex/config.toml", "sandbox_mode = \"read-only\"\n")
	if s, ok, _ := profile.ToolSandbox(root, dirs, as["codex"]); !ok || s.Risky || s.Value != "read-only" {
		t.Fatalf("read-only %+v", s)
	}
}
