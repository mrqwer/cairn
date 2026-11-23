package index_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/index"
	"github.com/mrqwer/cairn/internal/fsx"
)

const claudeLocator = "~/.claude/projects/*/{native_id}.jsonl"

func claudeAdapter() adapter.Adapter {
	return adapter.Adapter{Name: "claude", Binary: "claude", History: adapter.History{Locator: claudeLocator, Parser: "claude-jsonl@1"}}
}

// fakeAdapter reads claude-format files from another directory.
func fakeAdapter(parser string) adapter.Adapter {
	return adapter.Adapter{Name: "fake", Binary: "fake", History: adapter.History{Locator: "~/.fake/{native_id}.jsonl", Parser: parser}}
}

type env struct {
	home string
	ix   index.Index
}

func newEnv(t *testing.T) env {
	t.Helper()
	home := t.TempDir()
	return env{home: home, ix: index.Index{Dir: filepath.Join(home, "state/cairn/agents/index"), Root: fsx.Root{Dir: "/"}, Home: home}}
}

func line(typ, role, sid, cwd string, at time.Time, text string) string {
	m := map[string]any{"type": typ, "sessionId": sid, "cwd": cwd, "timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}}}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

// writeClaude writes a synthetic transcript and returns its path.
func (e env) writeClaude(t *testing.T, native, cwd string, at time.Time, texts ...string) string {
	t.Helper()
	p := filepath.Join(e.home, ".claude/projects/-home-u-demo", native+".jsonl")
	return writeLines(t, p, native, cwd, at, texts...)
}

func writeLines(t *testing.T, p, native, cwd string, at time.Time, texts ...string) string {
	t.Helper()
	var b strings.Builder
	for i, tx := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		b.WriteString(line(role, role, native, cwd, at.Add(time.Duration(i)*time.Second), tx))
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendLine(t *testing.T, p, text string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(line("user", "user", "x", "/w", time.Now(), text))
	_ = f.Close()
	// Make sure the mtime moves even on coarse clocks.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(p, future, future)
}

func uuid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func convFor(id, tool, native, project string) conv.Conversation {
	return conv.Conversation{ID: id, Tool: tool, NativeID: native, Project: project, State: conv.StateStopped,
		LastActivityAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func indexFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range ents {
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[de.Name()] = string(b)
	}
	return out
}

func TestIndexNeverContainsSecrets(t *testing.T) {
	e := newEnv(t)
	secrets := []string{
		"sk-ant-api03-SECRETSECRETSECRET",
		"ghp_0123456789abcdefABCDEF0123456789abcd",
		"hunter2hunter2",
		"MIIEpAIBAAKCAQEAsecretkeymaterial",
		"Zx9Qp2Lm7Vb4Nc8Rt1Ws5Yk3Hd6Jf0Ga",
		"declared-field-value",
	}
	e.writeClaude(t, uuid(1), "/home/u/projects/demo", t0,
		"please use "+secrets[0]+" and "+secrets[1],
		"set DB_PASSWORD="+secrets[2]+" in .env",
		"-----BEGIN RSA PRIVATE KEY-----\n"+secrets[3]+"\n-----END RSA PRIVATE KEY-----",
		"opaque "+secrets[4],
		"vault_otp: "+secrets[5],
	)
	a := claudeAdapter()
	a.History.SecretFields = []string{"vault_otp"}
	rep, err := e.ix.Update(context.Background(), []conv.Conversation{convFor("C1", "claude", uuid(1), "demo")}, index.Parsers(), []adapter.Adapter{a})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 {
		t.Fatalf("report %+v", rep)
	}
	files := indexFiles(t, e.ix.Dir)
	if !strings.Contains(files["C1.txt"], "please use") {
		t.Fatalf("index text missing: %q", files["C1.txt"])
	}
	for name, body := range files {
		for _, s := range secrets {
			if strings.Contains(body, s) {
				t.Errorf("%s contains secret %q", name, s)
			}
		}
	}
	hits, err := e.ix.Search("please", conv.Filter{}, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits %+v err %v", hits, err)
	}
	for _, s := range secrets {
		if strings.Contains(hits[0].Snippet, s) {
			t.Errorf("snippet contains secret %q", s)
		}
	}
}

func TestParserVersionUnknownSkipsTool(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "claude words")
	writeLines(t, filepath.Join(e.home, ".fake", uuid(2)+".jsonl"), uuid(2), "/w", t0, "fake words")
	convs := []conv.Conversation{convFor("C1", "claude", uuid(1), "demo"), convFor("F1", "fake", uuid(2), "demo")}
	rep, err := e.ix.Update(context.Background(), convs, index.Parsers(), []adapter.Adapter{claudeAdapter(), fakeAdapter("claude-jsonl@9")})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(rep.Skipped[0].Reason, "claude-jsonl@9") || !strings.Contains(rep.Skipped[0].Reason, index.ErrUnsupportedParser.Error()) {
		t.Fatalf("skip %+v", rep.Skipped[0])
	}
	if hits, _ := e.ix.Search("words", conv.Filter{}, 10); len(hits) != 1 || hits[0].ID != "C1" {
		t.Fatalf("hits %+v", hits)
	}
}

func TestUpdateSkipsGarbageFileAndContinues(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "good words")
	bad := filepath.Join(e.home, ".claude/projects/-home-u-demo", uuid(2)+".jsonl")
	if err := os.WriteFile(bad, []byte("\x00\x01garbage\nmore garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	convs := []conv.Conversation{convFor("C1", "claude", uuid(1), "demo"), convFor("C2", "claude", uuid(2), "demo")}
	rep, err := e.ix.Update(context.Background(), convs, index.Parsers(), []adapter.Adapter{claudeAdapter()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || len(rep.Skipped) != 1 || rep.Skipped[0].Path != bad {
		t.Fatalf("report %+v", rep)
	}
	// Reported again next time (not silently marked done), never fatal.
	rep, err = e.ix.Update(context.Background(), convs, index.Parsers(), []adapter.Adapter{claudeAdapter()})
	if err != nil || rep.Unchanged != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("second report %+v err %v", rep, err)
	}
}

func TestUpdateSkipsHugeFile(t *testing.T) {
	e := newEnv(t)
	e.ix.MaxFileSize = 512
	e.writeClaude(t, uuid(1), "/w", t0, strings.Repeat("big ", 200))
	rep, err := e.ix.Update(context.Background(), []conv.Conversation{convFor("C1", "claude", uuid(1), "demo")}, index.Parsers(), []adapter.Adapter{claudeAdapter()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 0 || len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Reason, "too large") {
		t.Fatalf("report %+v", rep)
	}
}

func TestUpdateRefusesSymlinkedTranscript(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(e.home, "private.txt")
	writeLines(t, target, uuid(1), "/w", t0, "private words")
	p := filepath.Join(e.home, ".claude/projects/-home-u-demo", uuid(1)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	rep, err := e.ix.Update(context.Background(), []conv.Conversation{convFor("C1", "claude", uuid(1), "demo")}, index.Parsers(), []adapter.Adapter{claudeAdapter()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 0 || len(rep.Skipped) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestUpdateIgnoresUnsafeIDs(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "words")
	convs := []conv.Conversation{
		convFor("../evil", "claude", uuid(1), "demo"),
		convFor("C2", "claude", "../../etc/passwd", "demo"),
		convFor("C3", "claude", "*", "demo"),
	}
	rep, err := e.ix.Update(context.Background(), convs, index.Parsers(), []adapter.Adapter{claudeAdapter()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 0 || len(rep.Skipped) != 3 {
		t.Fatalf("report %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(e.ix.Dir, "..", "evil.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote outside the index: %v", err)
	}
}

func TestIncrementalUpdate(t *testing.T) {
	e := newEnv(t)
	var convs []conv.Conversation
	var paths []string
	for i := 1; i <= 3; i++ {
		paths = append(paths, e.writeClaude(t, uuid(i), "/w", t0, fmt.Sprintf("conversation number%d", i)))
		convs = append(convs, convFor(fmt.Sprintf("C%d", i), "claude", uuid(i), "demo"))
	}
	ad := []adapter.Adapter{claudeAdapter()}
	rep, err := e.ix.Update(context.Background(), convs, index.Parsers(), ad)
	if err != nil || rep.Indexed != 3 || rep.Unchanged != 0 {
		t.Fatalf("first %+v %v", rep, err)
	}
	rep, err = e.ix.Update(context.Background(), convs, index.Parsers(), ad)
	if err != nil || rep.Indexed != 0 || rep.Unchanged != 3 {
		t.Fatalf("second %+v %v", rep, err)
	}
	appendLine(t, paths[1], "appended marmalade")
	rep, err = e.ix.Update(context.Background(), convs, index.Parsers(), ad)
	if err != nil || rep.Indexed != 1 || rep.Unchanged != 2 {
		t.Fatalf("third %+v %v", rep, err)
	}
	hits, err := e.ix.Search("MARMALADE", conv.Filter{}, 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "C2" {
		t.Fatalf("hits %+v %v", hits, err)
	}
	// A conversation no longer recorded leaves the index.
	rep, err = e.ix.Update(context.Background(), convs[:1], index.Parsers(), ad)
	if err != nil || rep.Unchanged != 1 {
		t.Fatalf("fourth %+v %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(e.ix.Dir, "C2.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dropped conversation still indexed: %v", err)
	}
}

func TestUpdateInterruptedKeepsDone(t *testing.T) {
	e := newEnv(t)
	var convs []conv.Conversation
	for i := 1; i <= 2; i++ {
		e.writeClaude(t, uuid(i), "/w", t0, "words")
		convs = append(convs, convFor(fmt.Sprintf("C%d", i), "claude", uuid(i), "demo"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.ix.Update(ctx, convs, index.Parsers(), []adapter.Adapter{claudeAdapter()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	rep, err := e.ix.Update(context.Background(), convs, index.Parsers(), []adapter.Adapter{claudeAdapter()})
	if err != nil || rep.Indexed+rep.Unchanged != 2 {
		t.Fatalf("after interrupt %+v %v", rep, err)
	}
}

func TestSearchFilters(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "deploy the Payment service", "ok")
	e.writeClaude(t, uuid(2), "/w", t0, "payment refactor deploy later")
	e.writeClaude(t, uuid(3), "/w", t0, "deploy only")
	writeLines(t, filepath.Join(e.home, ".fake", uuid(4)+".jsonl"), uuid(4), "/w", t0, "deploy payment from fake")
	c1 := convFor("C1", "claude", uuid(1), "demo")
	c1.Tags = []string{"infra"}
	c1.LastActivityAt = t0.Add(48 * time.Hour)
	c2 := convFor("C2", "claude", uuid(2), "api")
	c2.LastActivityAt = t0.Add(24 * time.Hour)
	c3 := convFor("C3", "claude", uuid(3), "demo")
	c4 := convFor("F4", "fake", uuid(4), "demo")
	c4.LastActivityAt = t0
	archivedAt := t0
	c5 := convFor("C5", "claude", uuid(1), "demo")
	c5.ArchivedAt = &archivedAt
	ad := []adapter.Adapter{claudeAdapter(), fakeAdapter("claude-jsonl@1")}
	if _, err := e.ix.Update(context.Background(), []conv.Conversation{c1, c2, c3, c4, c5}, index.Parsers(), ad); err != nil {
		t.Fatal(err)
	}
	ids := func(f conv.Filter, q string, limit int) string {
		hits, err := e.ix.Search(q, f, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.ID)
		}
		return strings.Join(out, ",")
	}
	// Recency order is by the record's last activity or the transcript time.
	for _, tc := range []struct {
		f     conv.Filter
		q     string
		limit int
		want  string
	}{
		{conv.Filter{}, "deploy payment", 10, ""},
		{conv.Filter{Project: "demo"}, "payment DEPLOY", 10, ""},
		{conv.Filter{Tool: "fake"}, "deploy", 10, "F4"},
		{conv.Filter{Tag: "infra"}, "deploy", 10, "C1"},
		{conv.Filter{Project: "api"}, "deploy", 10, "C2"},
		{conv.Filter{Archived: true, Project: "demo", Tool: "claude"}, "payment", 10, ""},
		{conv.Filter{Since: t0.Add(12 * time.Hour)}, "deploy", 10, "C1,C2"},
		{conv.Filter{}, "deploy", 1, "C1"},
		{conv.Filter{}, "nothing-matches-this", 10, ""},
	} {
		got := ids(tc.f, tc.q, tc.limit)
		if tc.want == "" {
			continue // checked below with set semantics
		}
		if got != tc.want {
			t.Errorf("Search(%q, %+v) = %s, want %s", tc.q, tc.f, got, tc.want)
		}
	}
	if got := ids(conv.Filter{}, "deploy payment", 10); got != "C1,C2,F4" {
		t.Errorf("AND over all: %s", got)
	}
	if got := ids(conv.Filter{Project: "demo"}, "payment DEPLOY", 10); got != "C1,F4" {
		t.Errorf("project demo: %s", got)
	}
	if got := ids(conv.Filter{Archived: true, Project: "demo", Tool: "claude"}, "payment", 10); got != "C1,C5" {
		t.Errorf("archived included: %s", got)
	}
	if got := ids(conv.Filter{}, "nothing-matches-this", 10); got != "" {
		t.Errorf("no match: %s", got)
	}
	hits, _ := e.ix.Search("payment", conv.Filter{Project: "api"}, 10)
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "payment refactor") || hits[0].Project != "api" {
		t.Fatalf("snippet %+v", hits)
	}
	if _, err := e.ix.Search("   ", conv.Filter{}, 10); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestPurgeProject(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "alpha words")
	e.writeClaude(t, uuid(2), "/w", t0, "alpha words")
	convs := []conv.Conversation{convFor("C1", "claude", uuid(1), "demo"), convFor("C2", "claude", uuid(2), "api")}
	if _, err := e.ix.Update(context.Background(), convs, index.Parsers(), []adapter.Adapter{claudeAdapter()}); err != nil {
		t.Fatal(err)
	}
	if err := e.ix.Purge("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.ix.Dir, "C1.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("C1 not purged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.ix.Dir, "C2.txt")); err != nil {
		t.Fatalf("C2 purged: %v", err)
	}
	m, _ := os.ReadFile(filepath.Join(e.ix.Dir, "manifest.json"))
	if strings.Contains(string(m), "C1") {
		t.Fatalf("manifest keeps C1: %s", m)
	}
	if hits, _ := e.ix.Search("alpha", conv.Filter{}, 10); len(hits) != 1 || hits[0].ID != "C2" {
		t.Fatalf("hits %+v", hits)
	}
	if err := e.ix.Purge(""); err == nil {
		t.Fatal("empty project purged")
	}
}

func TestIndexFilesMode0600(t *testing.T) {
	e := newEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "words")
	// A pre-existing, too open directory is tightened.
	if err := os.MkdirAll(e.ix.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ix.Update(context.Background(), []conv.Conversation{convFor("C1", "claude", uuid(1), "demo")}, index.Parsers(), []adapter.Adapter{claudeAdapter()}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(e.ix.Dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", fi.Mode(), err)
	}
	ents, _ := os.ReadDir(e.ix.Dir)
	if len(ents) < 2 {
		t.Fatalf("entries %v", ents)
	}
	for _, de := range ents {
		fi, err := os.Stat(filepath.Join(e.ix.Dir, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", de.Name(), fi.Mode())
		}
	}
}
