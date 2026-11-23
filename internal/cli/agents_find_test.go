package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// writeClaudeTranscript writes a synthetic Claude transcript under the test
// HOME (dir) for the built-in claude adapter's locator.
func writeClaudeTranscript(t *testing.T, dir, native, cwd string, at time.Time, texts ...string) string {
	t.Helper()
	p := filepath.Join(dir, ".claude/projects/-w", native+".jsonl")
	var b strings.Builder
	for i, tx := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		m := map[string]any{"type": role, "sessionId": native, "cwd": cwd, "timestamp": at.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			"message": map[string]any{"role": role, "content": tx}}
		line, _ := json.Marshal(m)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func nativeUUID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

var findT0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func TestAgentsImportCreatesUnnamedRecords(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	writeClaudeTranscript(t, dir, nativeUUID(1), filepath.Join(dir, "sub"), findT0, "fix the login flow", "done")
	cfgBefore := projectText(t, dir)
	evBefore := len(historyEvents(t))

	out := mustRun(t, "", "agents", "import", "--dry-run")
	if !strings.Contains(out, "would import 1") {
		t.Fatalf("dry run output:\n%s", out)
	}
	if l, _ := testStore(dir).List(conv.Filter{Archived: true}); len(l) != 0 {
		t.Fatalf("dry run recorded %+v", l)
	}

	out = mustRun(t, "", "agents", "import")
	if !strings.Contains(out, "imported 1") {
		t.Fatalf("output:\n%s", out)
	}
	c := onlyConv(t, dir)
	if c.Tool != "claude" || c.NativeID != nativeUUID(1) || c.Name != "" || c.State != conv.StateStopped || c.Project != "demo" {
		t.Fatalf("record %+v", c)
	}
	// Records only: no desired state, no history event.
	if projectText(t, dir) != cfgBefore || len(historyEvents(t)) != evBefore {
		t.Fatal("import touched desired state or history")
	}
	out = mustRun(t, "", "agents", "import")
	if !strings.Contains(out, "imported 0") || !strings.Contains(out, "1 already recorded") {
		t.Fatalf("second import:\n%s", out)
	}
	if l, _ := testStore(dir).List(conv.Filter{Archived: true}); len(l) != 1 {
		t.Fatalf("not idempotent: %+v", l)
	}
}

func TestAgentsImportUnknownTool(t *testing.T) {
	agentsTestEnv(t)
	if out, err := runHost(t, "", "agents", "import", "--tool", "nope"); err == nil {
		t.Fatalf("unknown tool accepted:\n%s", out)
	}
}

func TestAgentsFind(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	writeClaudeTranscript(t, dir, nativeUUID(1), dir, findT0, "fix the Login flow with key sk-ant-api03-SECRETSECRETSECRET", "patched login")
	writeClaudeTranscript(t, dir, nativeUUID(2), "/elsewhere", findT0.Add(time.Hour), "write release notes")
	bad := filepath.Join(dir, ".claude/projects/-w", nativeUUID(3)+".jsonl")
	if err := os.WriteFile(bad, []byte("\x00\x01 not a transcript\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "", "agents", "import")
	// The garbage file was skipped by import; make it a recorded
	// conversation so find meets it too.
	if err := testStore(dir).Put(conv.Conversation{ID: "01BADBADBADBADBADBADBADBAD", Tool: "claude", NativeID: nativeUUID(3), State: conv.StateStopped}); err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, "", "agents", "find", "login", "FLOW")
	if !strings.Contains(out, "fix the Login flow") || strings.Contains(out, "release notes") {
		t.Fatalf("find output:\n%s", out)
	}
	if strings.Contains(out, "SECRETSECRET") {
		t.Fatalf("secret in output:\n%s", out)
	}
	if !strings.Contains(out, "1 skipped") || !strings.Contains(out, bad) {
		t.Fatalf("skipped file not listed:\n%s", out)
	}
	if !strings.Contains(out, "cairn agents resume") {
		t.Fatalf("no open hint:\n%s", out)
	}

	out = mustRun(t, "", "agents", "find", "login", "--json")
	var res struct {
		Hits []struct {
			ID, Tool, Project, Snippet string
		} `json:"hits"`
		Skipped []struct{ Path, Reason string } `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(res.Hits) != 1 || res.Hits[0].Project != "demo" || res.Hits[0].Tool != "claude" || len(res.Skipped) != 1 {
		t.Fatalf("json %+v", res)
	}

	if out := mustRun(t, "", "agents", "find", "release", "--project", "demo"); !strings.Contains(out, "no matches") {
		t.Fatalf("project filter:\n%s", out)
	}
	if out := mustRun(t, "", "agents", "find", "release", "--since", "2026-10-01"); !strings.Contains(out, "release notes") {
		t.Fatalf("since filter:\n%s", out)
	}
	if out := mustRun(t, "", "agents", "find", "notes", "--tool", "codex", "--reindex"); !strings.Contains(out, "no matches") {
		t.Fatalf("tool filter:\n%s", out)
	}
	if _, err := runHost(t, "", "agents", "find", "x", "--since", "yesterday-ish"); err == nil {
		t.Fatal("bad --since accepted")
	}
	if _, err := runHost(t, "", "agents", "find"); err == nil {
		t.Fatal("empty query accepted")
	}

	// The index lives in the state dir, private.
	idx := filepath.Join(dir, "state/cairn/agents/index")
	fi, err := os.Stat(idx)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("index dir %v %v", fi, err)
	}
	ents, _ := os.ReadDir(idx)
	for _, de := range ents {
		b, _ := os.ReadFile(filepath.Join(idx, de.Name()))
		if strings.Contains(string(b), "SECRETSECRET") {
			t.Fatalf("%s holds the secret", de.Name())
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":         now.Add(-7 * 24 * time.Hour),
		"36h":        now.Add(-36 * time.Hour),
		"90m":        now.Add(-90 * time.Minute),
		"2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-3d", "soon", "0x1d"} {
		if _, err := parseSince(in, now); err == nil {
			t.Errorf("parseSince(%q) accepted", in)
		}
	}
}
