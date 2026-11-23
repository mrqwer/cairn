package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/fsx"
)

func recTask() Task {
	return Task{ID: "01JABCDEFGHJKMNPQRSTVWXYZ0", Title: "Fix login", Slug: "fix-login", Project: "demo",
		Status: StatusDone, Isolation: IsolationHere,
		Repos: []Repo{{Repo: "/home/u/src/demo", Branch: "task/fix-login", Base: "main"}}}
}

const goodText = "Goal: stop 502s.\nChanged: internal/db/pool.go.\nVerify: go test ./internal/db\nLinks: branch task/fix-login, commits 3f2a9c1..b81d0e4\n"

func TestDraftUsesRealFiles(t *testing.T) {
	d := Draft(recTask(), []string{"internal/db/pool.go", "cmd/x.go", "bad\nname"},
		[]string{"3f2a9c1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "b81d0e4bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, []string{"login agent"})
	for _, want := range []string{"Goal:", "Changed: internal/db/pool.go, cmd/x.go", "Decision: TODO", "Verify: TODO",
		"Links:", "task/fix-login", "3f2a9c1..b81d0e4", "login agent"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in:\n%s", want, d)
		}
	}
	if strings.Contains(d, "bad") {
		t.Errorf("file with a control character kept:\n%s", d)
	}
	if allOK(Gate(d)) {
		t.Error("a skeleton must not pass the gate")
	}
	e := Draft(recTask(), nil, nil, nil)
	if !strings.Contains(e, "Changed: TODO") || strings.Contains(e, "..") {
		t.Errorf("empty input is invented:\n%s", e)
	}
	one := Draft(recTask(), nil, []string{"3f2a9c1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil)
	if !strings.Contains(one, "commit 3f2a9c1") || strings.Contains(one, "3f2a9c1..") {
		t.Errorf("single commit:\n%s", one)
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = "dir/f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".go"
	}
	if m := Draft(recTask(), many, nil, nil); !strings.Contains(m, "more") || strings.Contains(m, many[29]) {
		t.Errorf("long file list is not cut:\n%s", m)
	}
}

func TestWriteRecordRefusesFailingGate(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	for _, text := range []string{"Goal: TODO", "robust fix internal/db/pool.go go test", words(130)} {
		p, err := WriteRecord(root, "/kb", recTask(), text)
		if err == nil || p != "" {
			t.Fatalf("%q: %q %v", text, p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root.Dir, "kb")); err == nil {
		t.Fatal("something was written")
	}
}

func TestWriteRecordFrontMatterAnchors(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	old := now
	now = func() time.Time { return time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC) }
	defer func() { now = old }()
	text := "Goal: x.\nChanged: ./internal/db/pool.go, docs/заметка.md, ../../etc/passwd, internal/db/pool.go.\nVerify: go test ./internal/db\nLinks: branch task/fix-login, commit 3f2a9c1\n"
	p, err := WriteRecord(root, "/kb", recTask(), text)
	if err != nil {
		t.Fatal(err)
	}
	if p != "/kb/demo/fix-login.md" {
		t.Fatalf("path %q", p)
	}
	b, err := root.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "---\ntask: 01JABCDEFGHJKMNPQRSTVWXYZ0\nproject: demo\nbranch: task/fix-login\nanchors:\n  - internal/db/pool.go\n  - docs/заметка.md\n  - internal/db\ncreated: 2026-11-23T10:00:00Z\n---\n" + text
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	if strings.Contains(string(b), "task/fix-login\n  -") {
		t.Fatal("branch is an anchor")
	}
}

func TestRecordPathNoTraversal(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	for _, mut := range []func(*Task){
		func(t *Task) { t.Slug = "../../evil" },
		func(t *Task) { t.Slug = ".." },
		func(t *Task) { t.Project = "../x" },
		func(t *Task) { t.Project = "a/b" },
		func(t *Task) { t.ID = "../x" },
	} {
		tk := recTask()
		mut(&tk)
		if p, err := WriteRecord(root, "/kb", tk, goodText); err == nil {
			t.Errorf("%+v written to %s", tk, p)
		}
	}
	for _, dir := range []string{"", "kb", "/kb/../x"} {
		if _, err := WriteRecord(root, dir, recTask(), goodText); err == nil {
			t.Errorf("dir %q accepted", dir)
		}
	}
	_ = filepath.Walk(root.Dir, func(p string, i os.FileInfo, _ error) error {
		if !i.IsDir() {
			t.Errorf("stray file %s", p)
		}
		return nil
	})
}

func TestWriteRecordKeepsOtherTasksRecord(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	if _, err := WriteRecord(root, "/kb", recTask(), goodText); err != nil {
		t.Fatal(err)
	}
	other := recTask()
	other.ID = "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	if _, err := WriteRecord(root, "/kb", other, goodText); err == nil {
		t.Fatal("a record of another task was overwritten")
	}
	if _, err := WriteRecord(root, "/kb", recTask(), goodText+"Decision: more.\n"); err != nil {
		t.Fatalf("redo of the same task: %v", err)
	}
}
