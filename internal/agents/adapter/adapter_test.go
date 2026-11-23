package adapter_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

func loadBuiltins(t *testing.T) map[string]adapter.Adapter {
	t.Helper()
	dir := t.TempDir()
	as, lerrs, err := adapter.Load(fsx.Root{Dir: dir}, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("Load: %v %v", err, lerrs)
	}
	m := map[string]adapter.Adapter{}
	for _, a := range as {
		m[a.Name] = a
	}
	return m
}

func TestRenderClaudeStart(t *testing.T) {
	a := loadBuiltins(t)["claude"]
	args, unsupported := a.ProfileArgs(map[string]string{"model": "opus"})
	if len(unsupported) != 0 {
		t.Fatalf("unsupported: %v", unsupported)
	}
	got, err := a.Render("start", adapter.Vars{ID: "u-1", Name: "fix bug", ProfileArgs: args})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--session-id", "u-1", "--name", "fix bug", "--model", "opus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	got, _ = a.Render("start", adapter.Vars{ID: "u-1", Name: "n"})
	if len(got) != 5 {
		t.Fatalf("empty profile args must splice nothing: %q", got)
	}
}

func TestRenderPreservesSpacesAndQuotes(t *testing.T) {
	a := adapter.Adapter{Name: "x", Commands: map[string][]string{"start": {"x", "--name", "n={name}"}}}
	got, err := a.Render("start", adapter.Vars{Name: `a "b" c`})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2] != `n=a "b" c` {
		t.Fatalf("got %q", got)
	}
	// substituted text is not rescanned
	got, _ = a.Render("start", adapter.Vars{Name: "{id}"})
	if got[2] != "n={id}" {
		t.Fatalf("rescanned: %q", got)
	}
}

func TestProfileArgsReportsUnsupported(t *testing.T) {
	a := loadBuiltins(t)["agy"]
	args, unsupported := a.ProfileArgs(map[string]string{"model": "m", "effort": "high", "zzz": "1"})
	if !reflect.DeepEqual(args, []string{"--model", "m"}) {
		t.Fatalf("args %q", args)
	}
	if !reflect.DeepEqual(unsupported, []string{"effort", "zzz"}) {
		t.Fatalf("unsupported %q", unsupported)
	}
}

func writeTool(t *testing.T, cfg, name, body string) fsx.Root {
	t.Helper()
	root := fsx.Root{Dir: t.TempDir()}
	dir := root.Path(filepath.Join(cfg, "tools"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestUserOverridesBuiltin(t *testing.T) {
	root := writeTool(t, "/cfg", "claude.toml", `name = "claude"
binary = "myclaude"
[capabilities]
hooks = "none"
discovery = "list"
[commands]
start = ["myclaude"]
`)
	as, lerrs, err := adapter.Load(root, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("%v %v", err, lerrs)
	}
	var names []string
	for _, a := range as {
		names = append(names, a.Name)
		if a.Name == "claude" && a.Binary != "myclaude" {
			t.Fatalf("not overridden: %+v", a)
		}
	}
	if !reflect.DeepEqual(names, []string{"agy", "claude", "codex"}) {
		t.Fatalf("names %v", names)
	}
}

func TestStrictUnknownKey(t *testing.T) {
	root := writeTool(t, "/cfg", "bad.toml", "name = \"bad\"\nbogus = 1\n[commands]\nstart = [\"bad\"]\n")
	as, lerrs, err := adapter.Load(root, xdg.Dirs{Config: "/cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 3 || len(lerrs) != 1 {
		t.Fatalf("adapters %d errs %v", len(as), lerrs)
	}
	if !strings.Contains(lerrs[0].File, "bad.toml") || !strings.Contains(lerrs[0].Error(), "bogus") {
		t.Fatalf("error must name file and key: %v", lerrs[0])
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		a    adapter.Adapter
		want string
	}{
		{"id without preassign", adapter.Adapter{Name: "x", Capabilities: adapter.Capabilities{Discovery: "list"},
			Commands: map[string][]string{"start": {"x", "{id}"}}}, "preassign_id"},
		{"unknown placeholder", adapter.Adapter{Name: "x", Capabilities: adapter.Capabilities{Discovery: "list"},
			Commands: map[string][]string{"start": {"x", "{foo}"}}}, "{foo}"},
		{"embedded splice", adapter.Adapter{Name: "x", Capabilities: adapter.Capabilities{Discovery: "list"},
			Commands: map[string][]string{"start": {"x", "--a={profile_args...}"}}}, "whole element"},
		{"discovery missing", adapter.Adapter{Name: "x",
			Commands: map[string][]string{"start": {"x"}}}, "capabilities.discovery"},
		{"start missing", adapter.Adapter{Name: "x", Capabilities: adapter.Capabilities{Discovery: "list"}}, "commands.start"},
		{"resume missing", adapter.Adapter{Name: "x", Capabilities: adapter.Capabilities{ResumeByID: true, Discovery: "list"},
			Commands: map[string][]string{"start": {"x"}}}, "commands.resume"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := c.a.Validate()
			for _, e := range errs {
				if strings.Contains(e.Error(), c.want) {
					return
				}
			}
			t.Fatalf("want error containing %q, got %v", c.want, errs)
		})
	}
}

func TestBuiltinsValidate(t *testing.T) {
	m := loadBuiltins(t)
	for _, n := range []string{"claude", "codex", "agy"} {
		a, ok := m[n]
		if !ok {
			t.Fatalf("missing built-in %s", n)
		}
		if errs := a.Validate(); len(errs) != 0 {
			t.Errorf("%s: %v", n, errs)
		}
	}
	if m["codex"].Capabilities.Hooks != "config-override" || m["codex"].Capabilities.Discovery != "dir-diff" {
		t.Errorf("codex caps: %+v", m["codex"].Capabilities)
	}
}

func TestForkNeedsCapability(t *testing.T) {
	m := loadBuiltins(t)
	if _, err := m["claude"].Render("fork", adapter.Vars{ID: "a", NewID: "b"}); err != nil {
		t.Fatal(err)
	}
	a := m["claude"]
	a.Capabilities.Fork = false
	if _, err := a.Render("fork", adapter.Vars{}); err == nil {
		t.Fatal("fork without capability must fail")
	}
	if _, err := a.Render("nope", adapter.Vars{}); err == nil {
		t.Fatal("unknown command must fail")
	}
}

func TestTesttoolWrite(t *testing.T) {
	dir := t.TempDir()
	p := testtool.Write(t, dir, testtool.Spec{Name: "fake", Version: "1.2.3", ListJSON: `[{"id":"x"}]`, ExitCode: 3})
	out := runTool(t, p, 0, "--version")
	if strings.TrimSpace(out) != "1.2.3" {
		t.Fatalf("version %q", out)
	}
	if out := runTool(t, p, 0, "agents", "--json"); out != `[{"id":"x"}]` {
		t.Fatalf("list %q", out)
	}
	runTool(t, p, 3, "start", "a b")
	log, _ := os.ReadFile(filepath.Join(dir, "fake.log"))
	if !strings.Contains(string(log), "  a b\n") || !strings.Contains(string(log), "ENV:") {
		t.Fatalf("log %q", log)
	}
}

func runTool(t *testing.T, path string, wantExit int, args ...string) string {
	t.Helper()
	cmd := exec.Command(path, args...)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code != wantExit {
		t.Fatalf("exit %d want %d", code, wantExit)
	}
	return string(out)
}
