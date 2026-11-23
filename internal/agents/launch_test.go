package agents_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

func builtin(t *testing.T, name string) adapter.Adapter {
	t.Helper()
	dir := t.TempDir()
	as, _, err := adapter.Load(fsx.Root{Dir: dir}, xdg.Dirs{Config: "/config", Home: "/home/u"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no builtin %s", name)
	return adapter.Adapter{}
}

const uuid = "4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5b"

func TestPaneArgv(t *testing.T) {
	got := agents.PaneArgv("/usr/bin/cairn", "01JX")
	if !slices.Equal(got, []string{"/usr/bin/cairn", "agents", "pane", "01JX"}) {
		t.Fatalf("%v", got)
	}
}

func TestBuildStartClaude(t *testing.T) {
	a := builtin(t, "claude")
	c := conv.Conversation{ID: "01JX", Tool: "claude", NativeID: uuid, Name: `fix "login" flow`, Cwd: "/home/u/demo"}
	argv, disc, err := agents.BuildStart(a, c, []string{"--model", "opus"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--session-id", uuid, "--name", `fix "login" flow`, "--model", "opus"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv %q, want %q", argv, want)
	}
	if disc != nil {
		t.Fatalf("preassigned id needs no discovery: %+v", disc)
	}
	if _, _, err := agents.BuildStart(a, conv.Conversation{ID: "01JX", Tool: "claude"}, nil); err == nil {
		t.Fatal("preassign without native id must fail")
	}
}

func TestBuildStartDiscoveryTool(t *testing.T) {
	a := builtin(t, "codex")
	argv, disc, err := agents.BuildStart(a, conv.Conversation{ID: "01JX", Tool: "codex", Name: "n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"codex"}) || disc == nil || disc.Mode != "dir-diff" {
		t.Fatalf("argv %v disc %+v", argv, disc)
	}
}

func TestNewNativeIDIsUUID(t *testing.T) {
	id := agents.NewNativeID()
	if len(id) != 36 || strings.Count(id, "-") != 4 || id[14] != '4' {
		t.Fatalf("not a v4 uuid: %q", id)
	}
}

func TestBuildResumeUsesNativeID(t *testing.T) {
	codex := builtin(t, "codex")
	argv, err := agents.BuildResume(codex, conv.Conversation{ID: "01JX", Tool: "codex", NativeID: "n-1"}, []string{"--model", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"codex", "resume", "n-1", "--model", "x"}) {
		t.Fatalf("argv %v", argv)
	}
	claude := builtin(t, "claude")
	argv, err = agents.BuildResume(claude, conv.Conversation{ID: "01JX", Tool: "claude", NativeID: uuid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"claude", "--resume", uuid}) {
		t.Fatalf("argv %v", argv)
	}
	if _, err := agents.BuildResume(codex, conv.Conversation{ID: "01JX", Tool: "codex"}, nil); err == nil {
		t.Fatal("resume without native id must fail")
	}
}

// dirDiffAdapter locates sessions under <home>/sessions/<date>/rollout-<ts>-<id>.jsonl.
func dirDiffAdapter() adapter.Adapter {
	return adapter.Adapter{Name: "fake", Capabilities: adapter.Capabilities{Discovery: "dir-diff", ResumeByID: true},
		Commands: map[string][]string{"start": {"fake"}, "resume": {"fake", "resume", "{native_id}"}},
		History:  adapter.History{Locator: "~/sessions/*/rollout-*-{native_id}.jsonl", Parser: "x@1"}}
}

func writeSession(t *testing.T, home, name, cwd string) {
	t.Helper()
	p := filepath.Join(home, "sessions", "2026-11-23", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"session_meta","payload":{"cwd":"`+cwd+`"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverDirDiff(t *testing.T) {
	home := t.TempDir()
	root := fsx.Root{Dir: "/"}
	a := dirDiffAdapter()
	writeSession(t, home, "rollout-2026-11-23T10-00-00-11111111-1111-4111-8111-111111111111.jsonl", "/home/u/demo")
	before, err := agents.TakeSnapshot(context.Background(), root, a, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Second)
	writeSession(t, home, "rollout-2026-11-23T10-05-00-"+uuid+".jsonl", "/home/u/demo")
	writeSession(t, home, "rollout-2026-11-23T10-05-01-22222222-2222-4222-8222-222222222222.jsonl", "/home/u/other")
	id, err := agents.Discover(context.Background(), root, a, nil, before, started, "/home/u/demo")
	if err != nil {
		t.Fatal(err)
	}
	if id != uuid {
		t.Fatalf("id %q", id)
	}
}

func TestDiscoverDirDiffAmbiguousIsError(t *testing.T) {
	home := t.TempDir()
	root := fsx.Root{Dir: "/"}
	a := dirDiffAdapter()
	before, err := agents.TakeSnapshot(context.Background(), root, a, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Second)
	writeSession(t, home, "rollout-a-"+uuid+".jsonl", "/home/u/demo")
	writeSession(t, home, "rollout-b-22222222-2222-4222-8222-222222222222.jsonl", "/home/u/demo")
	_, err = agents.Discover(context.Background(), root, a, nil, before, started, "/home/u/demo")
	if !errors.Is(err, agents.ErrAmbiguous) {
		t.Fatalf("err=%v, want ErrAmbiguous", err)
	}
	if !strings.Contains(err.Error(), uuid) {
		t.Fatalf("error should name the candidates: %v", err)
	}
	// nothing new: not discovered (yet)
	before2, _ := agents.TakeSnapshot(context.Background(), root, a, nil, home)
	if _, err := agents.Discover(context.Background(), root, a, nil, before2, started, "/home/u/demo"); !errors.Is(err, agents.ErrNotDiscovered) {
		t.Fatalf("err=%v, want ErrNotDiscovered", err)
	}
}

func TestDiscoverListParses(t *testing.T) {
	a := adapter.Adapter{Name: "fake", Capabilities: adapter.Capabilities{Discovery: "list", ListJSON: true},
		Commands: map[string][]string{"start": {"fake"}, "list": {"fake", "agents", "--json"}}}
	run := &execx.FakeRunner{Responses: map[string]execx.Response{
		"fake agents --json": {Out: []byte(`[{"id":"old","cwd":"/home/u/demo"}]`)},
	}}
	before, err := agents.TakeSnapshot(context.Background(), fsx.Root{Dir: "/"}, a, run, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	run.Responses["fake agents --json"] = execx.Response{Out: []byte(`{"sessions":[
		{"id":"old","cwd":"/home/u/demo"},
		{"session_id":"new-1","cwd":"/home/u/demo","created_at":"` + started.Add(time.Second).UTC().Format(time.RFC3339) + `"},
		{"id":"elsewhere","cwd":"/home/u/api"},
		{"id":"stale","cwd":"/home/u/demo","created_at":"2001-01-01T00:00:00Z"},
		"garbage"
	]}`)}
	id, err := agents.Discover(context.Background(), fsx.Root{Dir: "/"}, a, run, before, started, "/home/u/demo")
	if err != nil {
		t.Fatal(err)
	}
	if id != "new-1" {
		t.Fatalf("id %q", id)
	}
}

// Review M4: ids from tool output must be safe in paths, globs and argv.
func TestDiscoverRejectsUnsafeIDs(t *testing.T) {
	a := adapter.Adapter{Name: "fake", Capabilities: adapter.Capabilities{Discovery: "list", ListJSON: true},
		Commands: map[string][]string{"start": {"fake"}, "list": {"fake", "ls"}}}
	run := &execx.FakeRunner{Responses: map[string]execx.Response{"fake ls": {Out: []byte(`[]`)}}}
	before, _ := agents.TakeSnapshot(context.Background(), fsx.Root{Dir: "/"}, a, run, "/home/u")
	for _, bad := range []string{"../x", "a/b", "a*b", "-rf", "a b", "x?", "[a]"} {
		run.Responses["fake ls"] = execx.Response{Out: []byte(`[{"id":"` + bad + `"}]`)}
		if id, err := agents.Discover(context.Background(), fsx.Root{Dir: "/"}, a, run, before, time.Now(), ""); err == nil {
			t.Errorf("unsafe id %q accepted as %q", bad, id)
		}
	}
	if agents.ValidNativeID("../x") || !agents.ValidNativeID(uuid) {
		t.Fatal("ValidNativeID")
	}
}
