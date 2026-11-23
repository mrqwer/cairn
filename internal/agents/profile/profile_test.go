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

// writeProfiles writes <Config>/profiles.toml under a temp root.
func writeProfiles(t *testing.T, text string) (fsx.Root, xdg.Dirs) {
	t.Helper()
	dir := t.TempDir()
	dirs := xdg.Dirs{Config: "/cfg", State: "/state", Home: "/home/u"}
	p := filepath.Join(dir, "cfg", "profiles.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return fsx.Root{Dir: dir}, dirs
}

func TestProfileArgsMappedPerTool(t *testing.T) {
	root, dirs := writeProfiles(t, `
# careful work
[profiles.careful]
model = "opus"
effort = "high"
permission_mode = "plan"
sandbox = "read-only"
`)
	ps, lerrs, err := profile.Load(root, dirs)
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("Load: %v %v", err, lerrs)
	}
	p, ok := ps["careful"]
	if !ok || p.Name != "careful" || p.Settings["model"] != "opus" {
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
	if want := []string{"--model", "opus", "--ask-for-approval", "plan", "--sandbox", "read-only"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("codex args %q want %q", args, want)
	}
	if !reflect.DeepEqual(unsupported, []string{"effort"}) {
		t.Fatalf("codex unsupported %q", unsupported)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	ps, lerrs, err := profile.Load(fsx.Root{Dir: t.TempDir()}, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 || len(ps) != 0 {
		t.Fatalf("got %v %v %v", ps, lerrs, err)
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
	// Only the tool's mapping counts: claude cannot express sandbox.
	if profile.Risky(as["claude"], open) {
		t.Fatal("an unmapped key cannot be risky")
	}
	safe := profile.Profile{Name: "careful", Settings: map[string]string{"permission_mode": "plan"}}
	if profile.Risky(as["claude"], safe) || profile.Authorize(safe, as["claude"], workspace.Project{ID: "api"}) != nil {
		t.Fatal("plan is not risky")
	}
	// A risky flag in the adapter's own commands counts too (a user
	// adapter cannot bypass the acknowledgement).
	a := as["claude"]
	a.Commands = map[string][]string{"start": {"claude", "--dangerously-skip-permissions", "{profile_args...}"}}
	if !profile.Risky(a, profile.Profile{}) || profile.Authorize(profile.Profile{}, a, workspace.Project{ID: "api"}) == nil {
		t.Fatal("a risky flag in commands.start must need the acknowledgement")
	}
	// The built-in list applies even when an adapter lists no risky flags.
	a = as["claude"]
	a.RiskyFlags = nil
	if !profile.Risky(a, yolo) {
		t.Fatal("known risky flags apply to every adapter")
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
		`flag: model: "--dangerously-skip-permissions" must not start with "-"`, "number: effort: must be a string", `"Bad Name"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("load errors lack %q:\n%s", want, all)
		}
	}
	for _, le := range lerrs {
		if !strings.HasSuffix(le.File, "profiles.toml") {
			t.Fatalf("load error names %q", le.File)
		}
	}
	// A risk acknowledgement is per project only: never at the top level.
	root, dirs = writeProfiles(t, "i_understand_the_risk = true\n")
	if _, lerrs, _ = profile.Load(root, dirs); len(lerrs) != 1 || !strings.Contains(lerrs[0].Error(), "i_understand_the_risk") {
		t.Fatalf("top-level acknowledgement must be refused: %v", lerrs)
	}
	// A key the tool cannot express is reported, not fatal.
	_, unsupported := profile.Args(builtins(t)["agy"], ps["ok"])
	if !reflect.DeepEqual(unsupported, []string{"mcp_set"}) {
		t.Fatalf("unsupported %q", unsupported)
	}
}

func TestSandboxDefault(t *testing.T) {
	as := builtins(t)
	args, _ := profile.Args(as["codex"], profile.Profile{})
	if want := []string{"--sandbox", "workspace-write"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("codex without a profile: %q want %q", args, want)
	}
	args, _ = profile.Args(as["codex"], profile.Profile{Name: "ro", Settings: map[string]string{"sandbox": "read-only"}})
	if want := []string{"--sandbox", "read-only"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("profile sandbox wins: %q", args)
	}
	if args, _ = profile.Args(as["claude"], profile.Profile{}); len(args) != 0 {
		t.Fatalf("claude has no sandbox flag: %q", args)
	}
	// An adapter with a sandbox flag must say what "on" is.
	a := as["codex"]
	a.ProfileDefaults = nil
	found := false
	for _, fe := range a.Validate() {
		if fe.Field == "profile_defaults.sandbox" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing profile_defaults.sandbox must be a validation error: %v", a.Validate())
	}
}
