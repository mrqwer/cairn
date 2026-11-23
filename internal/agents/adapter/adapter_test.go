package adapter_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/adapter"
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

func okAdapter() adapter.Adapter {
	return adapter.Adapter{Name: "x", Capabilities: adapter.Capabilities{Discovery: "list"},
		Commands: map[string][]string{"start": {"x"}}}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*adapter.Adapter)
		want string
	}{
		{"id without preassign", func(a *adapter.Adapter) { a.Commands["start"] = []string{"x", "{id}"} },
			"commands.start: {id} requires capabilities.preassign_id"},
		{"unknown placeholder", func(a *adapter.Adapter) { a.Commands["start"] = []string{"x", "{foo}"} },
			`commands.start: unknown placeholder {foo} in "{foo}"`},
		{"embedded splice", func(a *adapter.Adapter) { a.Commands["start"] = []string{"x", "--a={profile_args...}"} },
			`commands.start: {profile_args...} must be a whole element, got "--a={profile_args...}"`},
		{"value in command", func(a *adapter.Adapter) { a.Commands["start"] = []string{"x", "{value}"} },
			`commands.start: {value} is only valid in profile_flags, got "{value}"`},
		{"command placeholder in profile flag", func(a *adapter.Adapter) {
			a.ProfileFlags = map[string][]string{"model": {"--m", "{name}"}}
		}, `profile_flags.model: {name} is not valid in profile_flags (only {value}), got "{name}"`},
		{"splice in profile flag", func(a *adapter.Adapter) {
			a.ProfileFlags = map[string][]string{"model": {"{profile_args...}"}}
		}, `profile_flags.model: {profile_args...} is not valid in profile_flags (only {value}), got "{profile_args...}"`},
		{"unknown placeholder in profile flag", func(a *adapter.Adapter) {
			a.ProfileFlags = map[string][]string{"model": {"{nope}"}}
		}, `profile_flags.model: unknown placeholder {nope} in "{nope}"`},
		{"discovery missing", func(a *adapter.Adapter) { a.Capabilities.Discovery = "" },
			"capabilities.discovery: required when neither preassign_id nor name_at_start is set"},
		{"discovery bad enum", func(a *adapter.Adapter) { a.Capabilities.Discovery = "magic" },
			`capabilities.discovery: must be none, dir-diff, list or state-file, got "magic"`},
		{"hooks bad enum", func(a *adapter.Adapter) { a.Capabilities.Hooks = "magic" },
			`capabilities.hooks: must be none, settings-file or config-override, got "magic"`},
		{"start missing", func(a *adapter.Adapter) { delete(a.Commands, "start") }, "commands.start: required"},
		{"resume missing", func(a *adapter.Adapter) { a.Capabilities.ResumeByID = true },
			"commands.resume: required when resume_by_id or resume_by_name is set"},
		{"fork command missing", func(a *adapter.Adapter) { a.Capabilities.Fork = true },
			"commands.fork: required when capabilities.fork is set"},
		{"list command missing", func(a *adapter.Adapter) { a.Capabilities.ListJSON = true },
			"commands.list: required when capabilities.list_json is set"},
		{"delete command missing", func(a *adapter.Adapter) { a.Capabilities.Delete = true },
			"commands.delete: required when capabilities.delete is set"},
		{"history half set", func(a *adapter.Adapter) { a.History.Locator = "~/x/*.jsonl" },
			"history.parser: required when history.locator is set"},
		{"history parser only", func(a *adapter.Adapter) { a.History.Parser = "p@1" },
			"history.locator: required when history.parser is set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := okAdapter()
			c.mod(&a)
			errs := a.Validate()
			if len(errs) != 1 || errs[0].Error() != c.want {
				t.Fatalf("want exactly [%s], got %v", c.want, errs)
			}
		})
	}
	if errs := okAdapter().Validate(); len(errs) != 0 {
		t.Fatalf("baseline must be valid: %v", errs)
	}
}

func TestDuplicateUserNameIsLoadError(t *testing.T) {
	root := writeTool(t, "/cfg", "a.toml", "name = \"dup\"\n[capabilities]\ndiscovery = \"list\"\n[commands]\nstart = [\"dup\"]\n")
	dir := root.Path("/cfg/tools")
	body := "name = \"dup\"\nbinary = \"other\"\n[capabilities]\ndiscovery = \"list\"\n[commands]\nstart = [\"dup\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "b.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	as, lerrs, err := adapter.Load(root, xdg.Dirs{Config: "/cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lerrs) != 1 || !strings.HasSuffix(lerrs[0].File, "b.toml") ||
		lerrs[0].Err.Error() != `duplicate adapter name "dup" (already defined in a.toml)` {
		t.Fatalf("errs: %v", lerrs)
	}
	for _, a := range as {
		if a.Name == "dup" && a.Binary != "" {
			t.Fatalf("first file must win: %+v", a)
		}
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
