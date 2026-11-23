package cli

import (
	"context"
	"time"

	"github.com/mrqwer/cairn/internal/xdg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/task"
)

const goodRecord = "Goal: fix the login redirect.\nChanged: internal/auth/login.go.\nVerify: go test ./internal/auth\nLinks: branch task/fix-login\n"

// mergedTask makes the task fix-login with one commit of internal/auth/login.go
// and finishes it with --merge; it returns the output of the finish.
func mergedTask(t *testing.T, dir string) string {
	t.Helper()
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "internal/auth/login.go"), "package auth\n")
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "fix login")
	return mustRun(t, "", "task", "finish", "fix-login", "--merge", "--yes")
}

func draftPath(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, "state/cairn/agents/drafts", loadTaskFiles(t)[0].ID+".md")
}

func recordPath(dir string) string { return filepath.Join(dir, "state/cairn/records/api/fix-login.md") }

func TestFinishPrintsAndSavesDraftFromTheDiff(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	out := mergedTask(t, dir)
	for _, want := range []string{"Goal: TODO", "Changed: internal/auth/login.go", "Decision: TODO", "Links: branch task/fix-login, commit ", "cairn task record fix-login"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	st, err := os.Stat(draftPath(t, dir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("draft: %v %v", st, err)
	}
	if exists(recordPath(dir)) {
		t.Fatal("a record was written without the gate")
	}
}

func TestFinishSucceedsWhenDraftCannotBeSaved(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	if err := os.MkdirAll(filepath.Join(dir, "state/cairn/agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "state/cairn/agents/drafts"), "in the way")
	out := mergedTask(t, dir)
	if taskStatus(t) != task.StatusDone || !strings.Contains(out, "draft") {
		t.Fatalf("%s\n%s", taskStatus(t), out)
	}
}

func TestRecordRefusesUnfinishedAndAbandoned(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	newWorktreeTask(t, dir)
	if out, err := runHost(t, "", "task", "record", "fix-login", "--text-file", "-"); err == nil || !strings.Contains(err.Error(), "finish") {
		t.Fatalf("open: %v\n%s", err, out)
	}
	mustRun(t, "", "task", "finish", "fix-login", "--abandon", "--yes")
	if out, err := runHost(t, "", "task", "record", "fix-login", "--text-file", "-"); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("abandoned: %v\n%s", err, out)
	}
}

func TestRecordGateFailureWritesNothing(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mergedTask(t, dir)
	out, err := runHost(t, "", "task", "record", "fix-login")
	if err == nil || !strings.Contains(out, "fail") || !strings.Contains(out, "TODO") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if exists(recordPath(dir)) || exists(filepath.Dir(recordPath(dir))) {
		t.Fatal("written")
	}
	out, err = runHost(t, "robust "+goodRecord, "task", "record", "fix-login", "--text-file", "-")
	if err == nil || !strings.Contains(out, "robust") || exists(recordPath(dir)) {
		t.Fatalf("err %v\n%s", err, out)
	}
}

func TestRecordTextFileWritesAndUndoes(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mergedTask(t, dir)
	f := filepath.Join(dir, "rec.txt")
	writeFile(t, f, goodRecord)
	out := mustRun(t, "", "task", "record", "fix-login", "--text-file", f)
	if !strings.Contains(out, "pass") || !strings.Contains(out, recordPath(dir)) {
		t.Fatalf("%s", out)
	}
	b, err := os.ReadFile(recordPath(dir))
	if err != nil || !strings.HasPrefix(string(b), "---\ntask: "+loadTaskFiles(t)[0].ID+"\nproject: api\nbranch: task/fix-login\nanchors:\n  - internal/auth/login.go\n") ||
		!strings.HasSuffix(string(b), goodRecord) {
		t.Fatalf("%v\n%s", err, b)
	}
	// Redo overwrites; undo brings the first one back.
	writeFile(t, f, goodRecord+"Decision: keep the cookie.\n")
	mustRun(t, "", "task", "record", "fix-login", "--text-file", f)
	if b2, _ := os.ReadFile(recordPath(dir)); !strings.Contains(string(b2), "keep the cookie") {
		t.Fatal("not redone")
	}
	mustRun(t, "", "undo")
	if b3, _ := os.ReadFile(recordPath(dir)); string(b3) != string(b) {
		t.Fatalf("undo:\n%s", b3)
	}
	mustRun(t, "", "undo")
	if exists(recordPath(dir)) {
		t.Fatal("undo of the first record left the file")
	}
}

func TestRecordUsesKnowledgeRootAndBannedFromConfig(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	kb := filepath.Join(dir, "kb")
	writeFile(t, filepath.Join(dir, "config/cairn/cairn.toml"), "[knowledge]\nroot = \""+kb+"\"\n[tasks]\ngate_banned = [\"Holistic\"]\n")
	mergedTask(t, dir)
	if out, err := runHost(t, goodRecord+"A holistic view.\n", "task", "record", "fix-login", "--text-file", "-"); err == nil || !strings.Contains(out, "holistic") {
		t.Fatalf("err %v\n%s", err, out)
	}
	mustRun(t, goodRecord, "task", "record", "fix-login", "--text-file", "-")
	if !exists(filepath.Join(kb, "api/fix-login.md")) || exists(recordPath(dir)) {
		t.Fatal("wrong place")
	}
}

func TestRecordEditKeepsDraftWhenEditorFails(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mergedTask(t, dir)
	before, _ := os.ReadFile(draftPath(t, dir))
	runEditor = func(argv []string) error {
		if err := os.WriteFile(argv[len(argv)-1], []byte("half"), 0o600); err != nil {
			return err
		}
		return os.ErrInvalid
	}
	if out, err := runHost(t, "", "task", "record", "fix-login", "--edit"); err == nil {
		t.Fatalf("editor failure ignored:\n%s", out)
	}
	if after, _ := os.ReadFile(draftPath(t, dir)); string(after) != string(before) {
		t.Fatalf("draft changed: %q", after)
	}
	runEditor = func(argv []string) error {
		return os.WriteFile(argv[len(argv)-1], []byte(goodRecord), 0o600)
	}
	out := mustRun(t, "", "task", "record", "fix-login", "--edit")
	if !exists(recordPath(dir)) || !strings.Contains(out, "pass") {
		t.Fatalf("not written:\n%s", out)
	}
	if d, _ := os.ReadFile(draftPath(t, dir)); string(d) != goodRecord {
		t.Fatalf("draft not kept: %q", d)
	}
	st, _ := os.Stat(draftPath(t, dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestRecordWithoutDraftOrTextExplains(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mergedTask(t, dir)
	if err := os.Remove(draftPath(t, dir)); err != nil {
		t.Fatal(err)
	}
	if out, err := runHost(t, "", "task", "record", "fix-login"); err == nil || !strings.Contains(err.Error(), "--text-file") {
		t.Fatalf("err %v\n%s", err, out)
	}
}

func TestRecordCyrillicTitle(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Починить вход", "--project", "api", "--tool", "fake", "--worktree")
	tk := loadTaskFiles(t)[0]
	if !strings.HasPrefix(tk.Slug, "task-") {
		t.Fatalf("slug %q", tk.Slug)
	}
	wt := filepath.Join(dir, "state/cairn/worktrees/api", tk.Slug)
	writeFile(t, filepath.Join(wt, "docs/заметка.md"), "x\n")
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "docs")
	out := mustRun(t, "", "task", "finish", tk.Slug, "--merge", "--yes")
	if !strings.Contains(out, "Changed: docs/заметка.md") {
		t.Fatalf("%s", out)
	}
	text := "Цель: починить вход.\nИзменено: docs/заметка.md.\nПроверка: go test ./...\n"
	mustRun(t, text, "task", "record", tk.Slug, "--text-file", "-")
	b, err := os.ReadFile(filepath.Join(dir, "state/cairn/records/api", tk.Slug+".md"))
	if err != nil || !strings.Contains(string(b), "anchors:\n  - docs/заметка.md\n") {
		t.Fatalf("%v\n%s", err, b)
	}
	// Too long in Cyrillic words fails too.
	long := strings.Repeat("слово ", 125) + "docs/заметка.md go test"
	if _, err := runHost(t, long, "task", "record", tk.Slug, "--text-file", "-"); err == nil {
		t.Fatal("125 Cyrillic words passed")
	}
}

func TestRecordUndoWithConfiguredKnowledgeRoot(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	kb := filepath.Join(dir, "kb")
	writeFile(t, filepath.Join(dir, "config/cairn/cairn.toml"), "[knowledge]\nroot = \""+kb+"\"\n")
	mergedTask(t, dir)
	mustRun(t, goodRecord, "task", "record", "fix-login", "--text-file", "-")
	mustRun(t, "", "undo")
	if exists(filepath.Join(kb, "api/fix-login.md")) {
		t.Fatal("undo left the record")
	}
}

func TestRecordDoesNotOverwriteUsersNote(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mergedTask(t, dir)
	writeFile(t, recordPath(dir), "my own note\n")
	if out, err := runHost(t, goodRecord, "task", "record", "fix-login", "--text-file", "-"); err == nil || !strings.Contains(err.Error(), "not overwritten") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(recordPath(dir)); string(b) != "my own note\n" {
		t.Fatalf("changed: %q", b)
	}
}

func TestRecordEditDoesNotHoldTheTasksLock(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mergedTask(t, dir)
	var lockErr error
	runEditor = func(argv []string) error {
		dirs, err := xdg.Resolve(os.Getenv)
		if err != nil {
			return err
		}
		unlock, err := lockTasks(context.Background(), dirs, 50*time.Millisecond)
		lockErr = err
		if err == nil {
			unlock()
		}
		return os.WriteFile(argv[len(argv)-1], []byte(goodRecord), 0o600)
	}
	mustRun(t, "", "task", "record", "fix-login", "--edit")
	if lockErr != nil {
		t.Fatalf("lock held during the editor: %v", lockErr)
	}
	if !exists(recordPath(dir)) {
		t.Fatal("not written")
	}
}

func TestKnowledgeRootRefusedWhenBroad(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	home := os.Getenv("HOME")
	for _, root := range []string{home, filepath.Dir(home), filepath.Join(dir, "state"), filepath.Join(dir, "state/cairn"), filepath.Join(home, ".ssh"), filepath.Join(home, ".ssh/x"), filepath.Join(dir, "config/cairn")} {
		writeFile(t, filepath.Join(dir, "config/cairn/cairn.toml"), "[knowledge]\nroot = \""+root+"\"\n")
		if out, err := runHost(t, "", "task", "record", "nope", "--text-file", "-"); err == nil || !strings.Contains(err.Error(), "knowledge.root") {
			t.Errorf("%s accepted: %v\n%s", root, err, out)
		}
	}
}
