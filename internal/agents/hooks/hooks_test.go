package hooks_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/hooks"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

const (
	self   = "/opt/cairn/bin/cairn"
	convID = "01JABCDEFGHJKMNPQRSTVWXYZ0"
)

var dirs = xdg.Dirs{Home: "/home/u", Config: "/home/u/.config/cairn", State: "/home/u/.local/state/cairn"}

func builtin(t *testing.T, name string) adapter.Adapter {
	t.Helper()
	as, lerrs, err := adapter.Load(fsx.Root{Dir: t.TempDir()}, dirs)
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("load: %v %v", err, lerrs)
	}
	for _, a := range as {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no built-in %s", name)
	return adapter.Adapter{}
}

type settingsFile struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func TestInjectClaudeWritesSettingsFile0600(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	args, err := hooks.Inject(builtin(t, "claude"), convID, self, dirs, root)
	if err != nil {
		t.Fatal(err)
	}
	path := root.Path(dirs.State + "/agents/hooks/" + convID + ".json")
	if !reflect.DeepEqual(args, []string{"--settings", path}) {
		t.Fatalf("args %q", args)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
	b, _ := os.ReadFile(path)
	var sf settingsFile
	if err := json.Unmarshal(b, &sf); err != nil {
		t.Fatalf("settings: %v\n%s", err, b)
	}
	want := map[string]string{"SessionStart": "idle", "UserPromptSubmit": "working", "Notification": "waiting",
		"Stop": "idle", "SessionEnd": "stopped"}
	if len(sf.Hooks) != len(want) {
		t.Fatalf("events %v", sf.Hooks)
	}
	for ev, kind := range want {
		hs := sf.Hooks[ev]
		if len(hs) != 1 || len(hs[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, hs)
		}
		h := hs[0].Hooks[0]
		cmd := "'" + self + "' event --conversation '" + convID + "' --kind " + kind + " --stdin-json"
		if h.Type != "command" || h.Command != cmd || h.Timeout <= 0 {
			t.Fatalf("%s hook %+v, want command %s", ev, h, cmd)
		}
	}
}

// TestInjectCommandSurvivesShell: the tool runs hook commands through a
// shell; a cairn path with spaces, quotes and $ reaches cairn as one argv
// element, and nothing else runs.
func TestInjectCommandSurvivesShell(t *testing.T) {
	tmp := t.TempDir()
	odd := filepath.Join(tmp, `it's a "cairn" $(touch pwned) ;x`)
	log := filepath.Join(tmp, "argv.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + log + "'\n"
	if err := os.WriteFile(odd, []byte(script), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	root := fsx.Root{Dir: "/"}
	d := dirs
	d.State = filepath.Join(tmp, "state")
	args, err := hooks.Inject(builtin(t, "claude"), convID, odd, d, root)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(args[1])
	var sf settingsFile
	if err := json.Unmarshal(b, &sf); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", sf.Hooks["Notification"][0].Hooks[0].Command)
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, _ := os.ReadFile(log)
	want := "event\n--conversation\n" + convID + "\n--kind\nwaiting\n--stdin-json\n"
	if string(got) != want {
		t.Fatalf("argv:\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(tmp, "pwned")); err == nil {
		t.Fatal("command substitution ran")
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		v := fi.Mode().String() + fi.ModTime().String()
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += string(b)
		}
		m[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInjectNeverTouchesUserSettings(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	user := map[string]string{
		"/home/u/.claude/settings.json":       `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "mine"}]}]}}`,
		"/home/u/.claude/settings.local.json": `{"model": "x"}`,
		"/home/u/.codex/config.toml":          "notify = [\"mine\"]\n",
	}
	for p, body := range user {
		if err := os.MkdirAll(filepath.Dir(root.Path(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root.Path(p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := dirs
	d.State = "/var/state/cairn"
	homeOutsideState := func() map[string]string { return snapshot(t, root.Path("/home/u")) }
	before := homeOutsideState()
	for _, n := range []string{"claude", "codex", "agy"} {
		if _, err := hooks.Inject(builtin(t, n), convID, self, d, root); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if after := homeOutsideState(); !reflect.DeepEqual(before, after) {
		t.Fatalf("user files changed:\nbefore %v\nafter  %v", before, after)
	}
	ents, err := os.ReadDir(root.Path(d.State + "/agents/hooks"))
	if err != nil || len(ents) != 1 || ents[0].Name() != convID+".json" {
		t.Fatalf("hooks dir: %v %v", ents, err)
	}
}

func TestInjectConfigOverrideCodex(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	args, err := hooks.Inject(builtin(t, "codex"), convID, `/opt/c "x"/cairn`, dirs, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "-c" || !strings.HasPrefix(args[1], "notify=") {
		t.Fatalf("args %q", args)
	}
	var v struct{ V []string }
	if err := toml.Unmarshal([]byte("v = "+strings.TrimPrefix(args[1], "notify=")), &v); err != nil {
		t.Fatalf("override value is not TOML: %v (%s)", err, args[1])
	}
	want := []string{`/opt/c "x"/cairn`, "event", "--conversation", convID, "--kind", "idle"}
	if !reflect.DeepEqual(v.V, want) {
		t.Fatalf("notify argv %q want %q", v.V, want)
	}
	if _, err := os.Stat(root.Path(dirs.State + "/agents/hooks")); err == nil {
		t.Fatal("config-override must not write a file")
	}
}

func TestInjectNoneReturnsNil(t *testing.T) {
	args, err := hooks.Inject(builtin(t, "agy"), convID, self, dirs, fsx.Root{Dir: t.TempDir()})
	if err != nil || args != nil {
		t.Fatalf("args %q err %v", args, err)
	}
}

func TestInjectRejectsUnsafeInput(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	a := builtin(t, "claude")
	for _, id := range []string{"", "../x", "-x", "a/b", "a b", strings.Repeat("A", 64)} {
		if _, err := hooks.Inject(a, id, self, dirs, root); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	for _, s := range []string{"", "/a\nb", "/a\x00b", "/a\xffb"} {
		if _, err := hooks.Inject(a, convID, s, dirs, root); err == nil {
			t.Errorf("self %q accepted", s)
		}
	}
	if _, err := os.Stat(root.Path(dirs.State + "/agents/hooks")); err == nil {
		t.Fatal("rejected input must not write a file")
	}
}
