package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/task"
)

// newWorktreeTask runs task new "Fix login" --worktree in project api and
// returns its worktree.
func newWorktreeTask(t *testing.T, dir string) string {
	t.Helper()
	mustRun(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--worktree")
	return filepath.Join(dir, "state/cairn/worktrees/api/fix-login")
}

func taskStatus(t *testing.T) string {
	t.Helper()
	ts := loadTaskFiles(t)
	if len(ts) != 1 {
		t.Fatalf("tasks %+v", ts)
	}
	return ts[0].Status
}

func taskConvArchived(t *testing.T, dir string) bool {
	t.Helper()
	tk := loadTaskFiles(t)[0]
	for _, c := range listConvs(t, dir) {
		if c.Task == tk.ID {
			return c.ArchivedAt != nil
		}
	}
	t.Fatal("no conversation of the task")
	return false
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// unchanged fails unless the task is still open with its worktree, branch
// and live conversation.
func unchanged(t *testing.T, dir, repo, wt, out string) {
	t.Helper()
	if !exists(wt) || !branchExists(t, repo, "task/fix-login") || taskStatus(t) != task.StatusOpen || taskConvArchived(t, dir) {
		t.Fatalf("something changed (worktree %v, branch %v, status %s):\n%s",
			exists(wt), branchExists(t, repo, "task/fix-login"), taskStatus(t), out)
	}
}

func TestNoModeIsUsageError(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	for _, args := range [][]string{{}, {"--merge", "--abandon"}, {"--pr", "--merge"}, {"--force", "--yes"}} {
		out, err := runHost(t, "", append([]string{"task", "finish", "fix-login"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "--merge, --pr or --abandon") {
			t.Fatalf("%v: err %v\n%s", args, err, out)
		}
		unchanged(t, dir, repo, wt, out)
	}
}

func TestFinishRefusesDirtyWorktree(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "notes.md"), "mine\n")
	for _, mode := range []string{"--abandon", "--merge"} {
		out, err := runHost(t, "", "task", "finish", "fix-login", mode, "--yes")
		if err == nil || !strings.Contains(out, "uncommitted notes.md") || !strings.Contains(out, "--force --yes") {
			t.Fatalf("%s: err %v\n%s", mode, err, out)
		}
		unchanged(t, dir, repo, wt, out)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "notes.md")); string(b) != "mine\n" {
		t.Fatal("work lost")
	}
}

func TestFinishRefusesUnpushed(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "a.txt"), "a\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "add a")
	out, err := runHost(t, "", "task", "finish", "fix-login", "--abandon")
	if err == nil || !strings.Contains(out, "add a") {
		t.Fatalf("err %v\n%s", err, out)
	}
	// No remote: only the task's own commit, never the base history.
	if strings.Contains(out, " init") {
		t.Fatalf("base history listed as unpushed:\n%s", out)
	}
	unchanged(t, dir, repo, wt, out)
}

func TestForceNeedsYesAndPrintsLoss(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "notes.md"), "mine\n")
	out, err := runHost(t, "", "task", "finish", "fix-login", "--abandon", "--force")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(out, "uncommitted notes.md") {
		t.Fatalf("err %v\n%s", err, out)
	}
	unchanged(t, dir, repo, wt, out)
	out = mustRun(t, "", "task", "finish", "fix-login", "--abandon", "--force", "--yes")
	if !strings.Contains(out, "lost") || !strings.Contains(out, "notes.md") {
		t.Fatalf("loss not printed:\n%s", out)
	}
	if exists(wt) || branchExists(t, repo, "task/fix-login") {
		t.Fatal("forced abandon kept the worktree or branch")
	}
	if taskStatus(t) != task.StatusAbandoned || !taskConvArchived(t, dir) {
		t.Fatalf("status %s", taskStatus(t))
	}
}

func TestMergeFastForwardsBase(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "a.txt"), "a\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "add a")
	tip := git(t, repo, "rev-parse", "task/fix-login")
	// Not undoable: a pipe without --yes runs nothing.
	out, err := runHost(t, "", "task", "finish", "fix-login", "--merge")
	if err == nil || !strings.Contains(out, "not undoable") || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err %v\n%s", err, out)
	}
	unchanged(t, dir, repo, wt, out)
	if git(t, repo, "rev-parse", "main") == tip {
		t.Fatal("merged without confirmation")
	}
	before := len(historyEvents(t))
	out = mustRun(t, "", "task", "finish", "fix-login", "--merge", "--yes")
	if got := git(t, repo, "rev-parse", "main"); got != tip {
		t.Fatalf("main at %s, want %s:\n%s", got, tip, out)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "a\n" {
		t.Fatal("main checkout not updated")
	}
	if exists(wt) || branchExists(t, repo, "task/fix-login") {
		t.Fatalf("worktree or merged branch kept:\n%s", out)
	}
	if taskStatus(t) != task.StatusDone || !taskConvArchived(t, dir) {
		t.Fatalf("status %s\n%s", taskStatus(t), out)
	}
	found := false
	for _, e := range historyEvents(t)[:len(historyEvents(t))-before] {
		found = found || e.Kind == "task.finish"
	}
	if !found {
		t.Fatalf("no task.finish event: %+v", historyEvents(t))
	}
	if strings.Contains(apiProjectText(t, dir), "task-fix-login") {
		t.Fatal("task window still bound")
	}
	// Finished is final.
	if _, err := runHost(t, "", "task", "finish", "fix-login", "--abandon"); err == nil {
		t.Fatal("finished a done task again")
	}
}

func TestMergeConflictLeavesEverythingIntact(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "README"), "task\n")
	git(t, wt, "commit", "-q", "-am", "task readme")
	writeFile(t, filepath.Join(repo, "README"), "main\n")
	git(t, repo, "commit", "-q", "-am", "main readme")
	head := git(t, repo, "rev-parse", "HEAD")
	out, err := runHost(t, "", "task", "finish", "fix-login", "--merge", "--yes")
	if err == nil || !strings.Contains(err.Error(), "README") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if git(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("main moved")
	}
	if st := git(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("main checkout left dirty:\n%s", st)
	}
	if exists(filepath.Join(repo, ".git", "MERGE_HEAD")) {
		t.Fatal("merge left in progress")
	}
	unchanged(t, dir, repo, wt, out)
}

func TestAbandonRemovesCleanWorktreeAndBranchWithoutNewCommits(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	out := mustRun(t, "", "task", "finish", "fix-login", "--abandon")
	if exists(wt) || branchExists(t, repo, "task/fix-login") {
		t.Fatalf("kept:\n%s", out)
	}
	if taskStatus(t) != task.StatusAbandoned || !taskConvArchived(t, dir) {
		t.Fatalf("status %s", taskStatus(t))
	}
	if !exists(filepath.Join(repo, "README")) {
		t.Fatal("main checkout touched")
	}
}

func bareOrigin(t *testing.T, dir, repo string) string {
	t.Helper()
	bare := filepath.Join(dir, "origin.git")
	git(t, repo, "init", "-q", "--bare", bare)
	git(t, repo, "remote", "add", "origin", bare)
	return bare
}

func TestAbandonKeepsBranchWithNewCommits(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	bareOrigin(t, dir, repo)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "a.txt"), "a\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "add a")
	git(t, wt, "push", "-q", "origin", "task/fix-login")
	out := mustRun(t, "", "task", "finish", "fix-login", "--abandon")
	if exists(wt) {
		t.Fatalf("clean worktree kept:\n%s", out)
	}
	if !branchExists(t, repo, "task/fix-login") || !strings.Contains(out, "git branch -D task/fix-login") {
		t.Fatalf("branch with new commits not kept:\n%s", out)
	}
	if taskStatus(t) != task.StatusAbandoned {
		t.Fatalf("status %s", taskStatus(t))
	}
}

func TestPRWithoutConfiguredToolStops(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	bare := bareOrigin(t, dir, repo)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "a.txt"), "a\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "add a")
	out, err := runHost(t, "", "task", "finish", "fix-login", "--pr", "--yes")
	if err == nil || !strings.Contains(err.Error(), "tasks.pr_tool") || !strings.Contains(err.Error(), "git -C "+repo+" push") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if b := git(t, bare, "branch", "--list"); b != "" {
		t.Fatalf("pushed without a PR tool: %s", b)
	}
	unchanged(t, dir, repo, wt, out)
}

// fakePRTool puts an executable name on PATH that records its arguments,
// one per line, in the returned file.
func fakePRTool(t *testing.T, name string, fail bool) string {
	t.Helper()
	bin := t.TempDir()
	rec := filepath.Join(bin, "args")
	code := "0"
	if fail {
		code = "4"
	}
	writeFile(t, filepath.Join(bin, name), "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+rec+"'\necho https://example.invalid/pr/1\nexit "+code+"\n")
	if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return rec
}

func TestPRPushesAndOpensWithConfiguredTool(t *testing.T) {
	for _, tool := range []string{"gh", "glab"} {
		t.Run(tool, func(t *testing.T) {
			dir, repo, _ := taskTestEnv(t)
			bare := bareOrigin(t, dir, repo)
			rec := fakePRTool(t, tool, false)
			t.Setenv("CAIRN_TASKS_PR_TOOL", tool)
			wt := newWorktreeTask(t, dir)
			writeFile(t, filepath.Join(wt, "a.txt"), "a\n")
			git(t, wt, "add", "a.txt")
			git(t, wt, "commit", "-q", "-m", "add a")
			out := mustRun(t, "", "task", "finish", "fix-login", "--pr", "--yes")
			if git(t, bare, "rev-parse", "task/fix-login") != git(t, repo, "rev-parse", "task/fix-login") {
				t.Fatal("not pushed")
			}
			b, err := os.ReadFile(rec)
			if err != nil {
				t.Fatalf("%s not run:\n%s", tool, out)
			}
			args := string(b)
			want := []string{"pr\ncreate\n", "--head=task/fix-login", "--base=main", "--title=Fix login"}
			if tool == "glab" {
				want = []string{"mr\ncreate\n", "--source-branch=task/fix-login", "--target-branch=main", "--title=Fix login"}
			}
			for _, w := range want {
				if !strings.Contains(args, w) {
					t.Fatalf("%s args lack %q:\n%s", tool, w, args)
				}
			}
			if !strings.Contains(out, "https://example.invalid/pr/1") || !strings.Contains(out, "not undoable") {
				t.Fatalf("out:\n%s", out)
			}
			if exists(wt) || !branchExists(t, repo, "task/fix-login") || taskStatus(t) != task.StatusDone {
				t.Fatalf("after PR: worktree %v, status %s", exists(wt), taskStatus(t))
			}
		})
	}
}

func TestPRToolFailureKeepsTaskOpen(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	bareOrigin(t, dir, repo)
	fakePRTool(t, "gh", true)
	t.Setenv("CAIRN_TASKS_PR_TOOL", "gh")
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "a.txt"), "a\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "add a")
	out, err := runHost(t, "", "task", "finish", "fix-login", "--pr", "--yes")
	if err == nil || !strings.Contains(err.Error(), "pull request was not opened") {
		t.Fatalf("err %v\n%s", err, out)
	}
	unchanged(t, dir, repo, wt, out)
}

func TestHereTaskFinishDoesNotTouchCheckout(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Quick fix", "--project", "api", "--tool", "fake", "--here")
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-q", "-m", "add a")
	writeFile(t, filepath.Join(repo, ".env"), "SECRET=1\n")
	writeFile(t, filepath.Join(repo, "notes.md"), "mine\n")
	out := mustRun(t, "", "task", "finish", "quick-fix", "--merge", "--yes")
	for _, f := range []string{".env", "notes.md", "a.txt", "README"} {
		if !exists(filepath.Join(repo, f)) {
			t.Fatalf("%s deleted from the main checkout:\n%s", f, out)
		}
	}
	if cur := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); cur != "main" {
		t.Fatalf("checkout on %s", cur)
	}
	if branchExists(t, repo, "task/quick-fix") || taskStatus(t) != task.StatusDone {
		t.Fatalf("branch kept or status %s:\n%s", taskStatus(t), out)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees")); !os.IsNotExist(err) {
		t.Fatal("worktree root appeared")
	}
}

func TestHereTaskWithChangesIsRefusedEvenForced(t *testing.T) {
	_, repo, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Quick fix", "--project", "api", "--tool", "fake", "--here")
	writeFile(t, filepath.Join(repo, "README"), "edited\n")
	out, err := runHost(t, "", "task", "finish", "quick-fix", "--abandon", "--force", "--yes")
	if err == nil || !strings.Contains(out+err.Error(), "never discards") {
		t.Fatalf("err %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "README")); string(b) != "edited\n" {
		t.Fatal("change discarded")
	}
	if cur := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); cur != "task/quick-fix" {
		t.Fatalf("checkout on %s", cur)
	}
}

func TestFinishRefusesRunningAgent(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	var id, tool string
	for _, c := range listConvs(t, dir) {
		id, tool = c.ID, c.Tool
	}
	rel, err := testStore(dir).AcquireLock(tool, "cairn-"+id)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	out, err := runHost(t, "", "task", "finish", "fix-login", "--abandon")
	if err == nil || !strings.Contains(err.Error(), "runs") {
		t.Fatalf("err %v\n%s", err, out)
	}
	unchanged(t, dir, repo, wt, out)
}

// fakeViewer is a viewer that records its arguments and its input.
func fakeViewer(t *testing.T) (bin, args, input string) {
	t.Helper()
	d := t.TempDir()
	bin, args, input = filepath.Join(d, "viewer"), filepath.Join(d, "args"), filepath.Join(d, "input")
	writeFile(t, bin, "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+args+"'\ncat > '"+input+"'\necho viewed\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, args, input
}

func TestReviewShowsDiffAndSetsStatus(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	wt := newWorktreeTask(t, dir)
	writeFile(t, filepath.Join(wt, "a.txt"), "added line\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "add a")
	// The default viewer is git diff of the task's change.
	out := mustRun(t, "", "task", "review", "fix-login")
	if !strings.Contains(out, "+added line") {
		t.Fatalf("diff not shown:\n%s", out)
	}
	if taskStatus(t) != task.StatusReview {
		t.Fatalf("status %s", taskStatus(t))
	}
	if historyEvents(t)[0].Kind != "task.review" {
		t.Fatalf("history %+v", historyEvents(t))
	}
	base := loadTaskFiles(t)[0].Repos[0].BaseCommit
	// A placeholder gets the range as one argument.
	bin, args, input := fakeViewer(t)
	out = mustRun(t, "", "task", "review", "fix-login", "--viewer", bin+" --range={range}")
	if b, _ := os.ReadFile(args); string(b) != "--range="+base+"...task/fix-login\n" || !strings.Contains(out, "viewed") {
		t.Fatalf("args %q\n%s", b, out)
	}
	// Without a placeholder a viewer that is not git reads the diff.
	t.Setenv("CAIRN_TASKS_DIFF_VIEWER", bin+" --side-by-side")
	mustRun(t, "", "task", "review", "fix-login")
	if b, _ := os.ReadFile(input); !strings.Contains(string(b), "+added line") {
		t.Fatalf("input %q", b)
	}
	if b, _ := os.ReadFile(args); string(b) != "--side-by-side\n" {
		t.Fatalf("args %q", b)
	}
}

func TestReviewViewerIsNotAShell(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	newWorktreeTask(t, dir)
	bin, args, _ := fakeViewer(t)
	marker := filepath.Join(dir, "pwned")
	mustRun(t, "", "task", "review", "fix-login", "--viewer", bin+" a;touch "+marker+" $HOME `id` {branch}")
	b, _ := os.ReadFile(args)
	if string(b) != "a;touch\n"+marker+"\n$HOME\n`id`\ntask/fix-login\n" {
		t.Fatalf("args %q", b)
	}
	if exists(marker) {
		t.Fatal("viewer ran through a shell")
	}
	if _, err := runHost(t, "", "task", "review", "fix-login", "--viewer", "  "); err == nil {
		t.Fatal("empty viewer accepted")
	}
}
