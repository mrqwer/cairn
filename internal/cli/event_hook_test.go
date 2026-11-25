package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// hookToolScript is a fake tool that behaves like a tool with settings-file
// hooks: it reads the file given with --settings and runs the commands of
// UserPromptSubmit and then Notification through sh, as the real tool does,
// copying the record after each.
const hookToolScript = `#!/bin/sh
settings=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--settings" ]; then settings="$2"; shift; fi
  shift
done
[ -n "$settings" ] || { echo "no --settings" >&2; exit 3; }
cmd_for() {
  grep -o "\"$1\":\[{\"hooks\":\[{\"type\":\"command\",\"command\":\"[^\"]*\"" "$settings" | sed 's/.*"command":"//; s/"$//'
}
rec="$XDG_STATE_HOME/cairn/agents/conversations.jsonl"
c=$(cmd_for UserPromptSubmit)
echo '{"prompt":"fix it"}' | sh -c "$c"
tail -n 1 "$rec" > "$OUT/working.json"
c=$(cmd_for Notification)
echo '{"message":"needs your permission to use Bash"}' | sh -c "$c"
tail -n 1 "$rec" > "$OUT/waiting.json"
`

// TestHookFileFlipsWorkingToWaiting: a tool started by the pane wrapper
// runs cairn event through the hook file cairn gave it, and the record
// moves working → waiting with the hook's reason.
func TestHookFileFlipsWorkingToWaiting(t *testing.T) {
	dir, _ := wsTestEnv(t)
	self := selfScript(t)
	oldExe := exePath
	t.Cleanup(func() { exePath = oldExe })
	exePath = func() (string, error) { return self, nil }
	out := t.TempDir()
	t.Setenv("OUT", out)
	tool := filepath.Join(t.TempDir(), "hooky")
	if err := os.WriteFile(tool, []byte(hookToolScript), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	adapterTOML := `name = "hooky"
binary = "hooky"

[capabilities]
preassign_id = true
resume_by_id = true
hooks = "settings-file"

[commands]
start = ["` + tool + `", "start", "{id}"]
resume = ["` + tool + `", "resume", "{native_id}"]

[hooks]
flag = "--settings"
format = "claude-settings@1"

[hooks.events]
UserPromptSubmit = "working"
Notification = "waiting"
`
	p := filepath.Join(dir, "config/cairn/tools/hooky.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(adapterTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	addProject(t, "demo", dir)
	mustRun(t, "", "agents", "new", "hooky", "--project", "demo", "--name", "hooked")
	c := onlyConv(t, dir)
	if o, err := runHost(t, "", "agents", "pane", c.ID, "--now"); err != nil {
		t.Fatalf("pane: %v\n%s", err, o)
	}
	read := func(name string) conv.Conversation {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		var r conv.Conversation
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%s: %v %q", name, err, b)
		}
		return r
	}
	w := read("working.json")
	if w.State != conv.StateWorking || w.StateSource != conv.SourceHook {
		t.Fatalf("after UserPromptSubmit: %+v", w)
	}
	wt := read("waiting.json")
	if wt.State != conv.StateWaiting || wt.StateSource != conv.SourceHook || wt.WaitingReason != "needs your permission to use Bash" {
		t.Fatalf("after Notification: %+v", wt)
	}
	hookFile := filepath.Join(dir, "state/cairn/agents/hooks", c.ID+".json")
	if fi, err := os.Stat(hookFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("hook file: %v %v", fi, err)
	}
	final := onlyConv(t, dir)
	if final.State != conv.StateStopped || !strings.Contains(strings.Join(final.LaunchArgv, " "), "--settings "+hookFile) {
		t.Fatalf("final %+v", final)
	}
}
