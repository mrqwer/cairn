package index_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/index"
	"github.com/mrqwer/cairn/internal/fsx"
)

func importEnv(t *testing.T) (env, conv.Store, index.ImportOptions) {
	t.Helper()
	e := newEnv(t)
	st := conv.Store{Dir: filepath.Join(e.home, "state/cairn/agents"), Root: fsx.Root{Dir: "/"}}
	opt := index.ImportOptions{Home: e.home, ProjectOf: func(cwd string) string {
		if cwd == "/home/u/projects/demo" || strings.HasPrefix(cwd, "/home/u/projects/demo/") {
			return "demo"
		}
		return ""
	}}
	return e, st, opt
}

func all(t *testing.T, st conv.Store) []conv.Conversation {
	t.Helper()
	cs, err := st.List(conv.Filter{Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].NativeID < cs[j].NativeID })
	return cs
}

func TestImportCreatesUnnamedRecords(t *testing.T) {
	e, st, opt := importEnv(t)
	p1 := e.writeClaude(t, uuid(1), "/home/u/projects/demo/sub", t0, "first prompt", "reply")
	e.writeClaude(t, uuid(2), "/elsewhere", t0.Add(time.Hour), "other prompt")
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 2 || rep.Existing != 0 || len(rep.Skipped) != 0 {
		t.Fatalf("report %+v", rep)
	}
	cs := all(t, st)
	if len(cs) != 2 {
		t.Fatalf("records %+v", cs)
	}
	c := cs[0]
	if c.Name != "" || c.State != conv.StateStopped || c.Tool != "claude" || c.NativeID != uuid(1) ||
		c.Project != "demo" || c.Cwd != "/home/u/projects/demo/sub" || c.TranscriptRef != p1 ||
		c.TranscriptParser != "claude-jsonl@1" || len(c.LaunchArgv) != 0 || !conv.ValidID(c.ID) {
		t.Fatalf("record %+v", c)
	}
	if !c.CreatedAt.Equal(t0) || !c.LastActivityAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("times %v %v", c.CreatedAt, c.LastActivityAt)
	}
	if cs[1].Project != "" || cs[1].Cwd != "/elsewhere" {
		t.Fatalf("second %+v", cs[1])
	}
}

func TestImportIdempotent(t *testing.T) {
	e, st, opt := importEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "a")
	ad := []adapter.Adapter{claudeAdapter()}
	if _, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, ad, st, opt); err != nil {
		t.Fatal(err)
	}
	e.writeClaude(t, uuid(2), "/w", t0, "b")
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, ad, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 1 || rep.Existing != 1 {
		t.Fatalf("report %+v", rep)
	}
	rep, err = index.Import(context.Background(), fsx.Root{Dir: "/"}, ad, st, opt)
	if err != nil || len(rep.Created) != 0 || rep.Existing != 2 {
		t.Fatalf("third %+v %v", rep, err)
	}
	if n := len(all(t, st)); n != 2 {
		t.Fatalf("records %d", n)
	}
}

func TestImportSkipsRecordedAndRemovedNativeIDs(t *testing.T) {
	e, st, opt := importEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "a")
	e.writeClaude(t, uuid(2), "/w", t0, "b")
	// uuid(1) was started by cairn; uuid(2) was imported then removed.
	if err := st.Put(conv.Conversation{ID: "C1", Tool: "claude", NativeID: uuid(1), Name: "mine", State: conv.StateIdle}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(conv.Conversation{ID: "C2", Tool: "claude", NativeID: uuid(2), State: conv.StateStopped}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(st.Dir, "curated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "curated", "C2.json"), []byte(`{"removed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 0 || rep.Existing != 2 {
		t.Fatalf("report %+v", rep)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	e, st, opt := importEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "a")
	opt.DryRun = true
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt)
	if err != nil || len(rep.Created) != 1 {
		t.Fatalf("report %+v %v", rep, err)
	}
	if n := len(all(t, st)); n != 0 {
		t.Fatalf("dry run wrote %d records", n)
	}
}

func TestImportToolFilterAndUnsupportedParser(t *testing.T) {
	e, st, opt := importEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "a")
	writeLines(t, filepath.Join(e.home, ".fake", uuid(2)+".jsonl"), uuid(2), "/w", t0, "b")
	ad := []adapter.Adapter{claudeAdapter(), fakeAdapter("fake-jsonl@1"), {Name: "agy", Binary: "agy"}}
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, ad, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 1 || len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Reason, "fake-jsonl@1") {
		t.Fatalf("report %+v", rep)
	}
	opt.Tool = "fake"
	rep, err = index.Import(context.Background(), fsx.Root{Dir: "/"}, ad, st, opt)
	if err != nil || len(rep.Created) != 0 || len(rep.Skipped) != 1 {
		t.Fatalf("tool filter %+v %v", rep, err)
	}
}

func TestImportSkipsBadFilesAndUnsafeNames(t *testing.T) {
	e, st, opt := importEnv(t)
	dir := filepath.Join(e.home, ".claude/projects/-home-u-demo")
	e.writeClaude(t, uuid(1), "/w", t0, "a")
	if err := os.WriteFile(filepath.Join(dir, uuid(2)+".jsonl"), []byte("\x00garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A name that is not a usable native id is never recorded.
	writeLines(t, filepath.Join(dir, "-rf ..x.jsonl"), "x", "/w", t0, "c")
	// A symlinked transcript is not followed.
	target := filepath.Join(e.home, "secret.jsonl")
	writeLines(t, target, uuid(3), "/w", t0, "s")
	if err := os.Symlink(target, filepath.Join(dir, uuid(3)+".jsonl")); err != nil {
		t.Fatal(err)
	}
	// An untrusted cwd (relative, control characters) is not kept.
	writeLines(t, filepath.Join(dir, uuid(4)+".jsonl"), uuid(4), "rel/dir\n", t0, "d")
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 2 || len(rep.Skipped) != 3 {
		t.Fatalf("report %+v", rep)
	}
	for _, c := range all(t, st) {
		if c.NativeID == uuid(4) && c.Cwd != "" {
			t.Fatalf("untrusted cwd kept: %q", c.Cwd)
		}
	}
}

func TestImportDoesNotWriteConfig(t *testing.T) {
	e, st, opt := importEnv(t)
	cfg := filepath.Join(e.home, "config/cairn")
	if err := os.MkdirAll(filepath.Join(cfg, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "projects/demo.toml"), []byte("id = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := func() map[string]string {
		m := map[string]string{}
		_ = filepath.WalkDir(cfg, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, _ := d.Info()
			b, _ := os.ReadFile(p)
			m[p] = fi.ModTime().String() + string(b)
			return nil
		})
		return m
	}
	before := snap()
	e.writeClaude(t, uuid(1), "/home/u/projects/demo", t0, "a")
	if _, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt); err != nil {
		t.Fatal(err)
	}
	after := snap()
	if len(before) != len(after) {
		t.Fatalf("config changed: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("config file %s changed", k)
		}
	}
}

// An imported conversation is opened with the tool's resume, never start
// (it exists already), and is not mistaken for a fresh one.
func TestImportedRecordResumes(t *testing.T) {
	e, st, opt := importEnv(t)
	e.writeClaude(t, uuid(1), "/w", t0, "a")
	if _, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt); err != nil {
		t.Fatal(err)
	}
	c := all(t, st)[0]
	a := claudeAdapter()
	a.Capabilities.PreassignID = true
	if c.State == conv.StateStarting || !agents.ShouldResume(fsx.Root{Dir: "/"}, e.home, a, c) {
		t.Fatalf("imported record would start a new conversation: %+v", c)
	}
}

// Review I4: a conversation that opens with a pasted screenshot over the
// import head is still imported.
func TestImportLongFirstLine(t *testing.T) {
	e, st, opt := importEnv(t)
	p := filepath.Join(e.home, ".claude/projects/-w", uuid(1)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","sessionId":"` + uuid(1) + `","cwd":"/home/u/projects/demo","timestamp":"2026-10-01T09:00:00Z","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.Repeat("QUFB", 512<<10) + `"}},{"type":"text","text":"see screenshot"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := index.Import(context.Background(), fsx.Root{Dir: "/"}, []adapter.Adapter{claudeAdapter()}, st, opt)
	if err != nil || len(rep.Created) != 1 || len(rep.Skipped) != 0 {
		t.Fatalf("report %+v %v", rep, err)
	}
	if c := rep.Created[0]; c.Project != "demo" || !c.CreatedAt.Equal(t0) {
		t.Fatalf("record %+v", c)
	}
}
