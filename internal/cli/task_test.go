package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/fsx"
)

// taskTestEnv is agentsTestEnv plus a git repository (one commit on main)
// registered as project "api". git sees only the test's HOME and config.
func taskTestEnv(t *testing.T) (dir, repo string, fd *fakeDriver) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, fd, _ = agentsTestEnv(t)
	gc := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gc, []byte("[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": gc, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
	} {
		t.Setenv(k, v)
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "") // restored after the test
		_ = os.Unsetenv(k)
	}
	repo = filepath.Join(dir, "src", "api")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "README"), "api\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), ".env\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	addProject(t, "api", repo)
	return dir, repo, fd
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	b, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func branchExists(t *testing.T, repo, b string) bool {
	t.Helper()
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b).Run() == nil
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadTaskFiles(t *testing.T) []task.Task {
	t.Helper()
	ts, lerrs, err := task.Load(fsx.Root{Dir: "/"}, testDirs(t))
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("load tasks: %v %v", lerrs, err)
	}
	return ts
}

func apiProjectText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config/cairn/projects/api.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewWorktreeCreatesBranchAndDir(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	writeFile(t, filepath.Join(repo, ".worktreeinclude"), ".env\n")
	git(t, repo, "add", ".worktreeinclude")
	git(t, repo, "commit", "-q", "-m", "include")
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(historyEvents(t))
	out := mustRun(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--worktree")
	wt := filepath.Join(dir, "state/cairn/worktrees/api/fix-login")
	if !branchExists(t, repo, "task/fix-login") {
		t.Fatal("branch not created")
	}
	if b, err := os.ReadFile(filepath.Join(wt, "README")); err != nil || string(b) != "api\n" {
		t.Fatalf("worktree: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(wt, ".env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env not copied 0600: %v %v", fi, err)
	}
	if got := git(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "task/fix-login" {
		t.Fatalf("worktree on %q", got)
	}
	ts := loadTaskFiles(t)
	if len(ts) != 1 {
		t.Fatalf("tasks %+v", ts)
	}
	tk := ts[0]
	r := tk.Repos[0]
	if tk.Slug != "fix-login" || tk.Title != "Fix login" || tk.Project != "api" || tk.Status != task.StatusOpen ||
		tk.Isolation != task.IsolationWorktree || r.Repo != repo || r.Branch != "task/fix-login" || r.Worktree != wt || r.Base != "main" || len(r.BaseCommit) != 40 {
		t.Fatalf("task %+v", tk)
	}
	var c conv.Conversation
	for _, x := range listConvs(t, dir) {
		if x.Task == tk.ID {
			c = x
		}
	}
	if c.ID == "" || c.Cwd != wt || c.Worktree != wt || c.Branch != "task/fix-login" || c.Project != "api" || c.Tool != "fake" {
		t.Fatalf("conversation %+v", c)
	}
	if txt := apiProjectText(t, dir); !strings.Contains(txt, `key = "task-fix-login"`) || !strings.Contains(txt, `agent = "`+c.ID+`"`) {
		t.Fatalf("project:\n%s", txt)
	}
	evs := historyEvents(t)
	if len(evs) != before+1 || evs[0].Kind != "task.new" {
		t.Fatalf("history %+v", evs)
	}
	if !strings.Contains(out, "fix-login") || !strings.Contains(out, wt) {
		t.Fatalf("out:\n%s", out)
	}
	// The same title again gets the next free slug.
	mustRun(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--worktree")
	if !branchExists(t, repo, "task/fix-login-2") {
		t.Fatal("collision suffix not used")
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees/api/fix-login-2")); err != nil {
		t.Fatal(err)
	}
}

func listConvs(t *testing.T, dir string) []conv.Conversation {
	t.Helper()
	l, err := testStore(dir).List(conv.Filter{Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestNewHereNoWorktree(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Quick fix", "--project", "api", "--tool", "fake", "--here")
	if got := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "task/quick-fix" {
		t.Fatalf("checkout on %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees")); !os.IsNotExist(err) {
		t.Fatalf("worktree root created: %v", err)
	}
	tk := loadTaskFiles(t)[0]
	if tk.Isolation != task.IsolationHere || tk.Repos[0].Worktree != "" {
		t.Fatalf("task %+v", tk)
	}
	for _, c := range listConvs(t, dir) {
		if c.Task == tk.ID && c.Cwd != repo {
			t.Fatalf("cwd %q", c.Cwd)
		}
	}
}

func TestNewHereRefusesDirtyCheckoutForOtherBase(t *testing.T) {
	_, repo, _ := taskTestEnv(t)
	git(t, repo, "switch", "-q", "-c", "other")
	writeFile(t, filepath.Join(repo, "other.txt"), "o\n")
	git(t, repo, "add", "other.txt")
	git(t, repo, "commit", "-q", "-m", "other")
	git(t, repo, "switch", "-q", "main")
	writeFile(t, filepath.Join(repo, "README"), "edited\n")
	_, err := runHost(t, "", "task", "new", "x", "--project", "api", "--tool", "fake", "--here", "--from", "other")
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err %v", err)
	}
	if branchExists(t, repo, "task/x") {
		t.Fatal("branch created")
	}
}

func TestDefaultIsolationAskFailsOnPipe(t *testing.T) {
	_, repo, _ := taskTestEnv(t)
	_, err := runHost(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake")
	if err == nil || !strings.Contains(err.Error(), "--worktree") || !strings.Contains(err.Error(), "--here") {
		t.Fatalf("err %v", err)
	}
	if branchExists(t, repo, "task/fix-login") {
		t.Fatal("branch created before the isolation was known")
	}
	// The config default decides without a prompt.
	t.Setenv("CAIRN_TASKS_DEFAULT_ISOLATION", "here")
	mustRun(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake")
	if loadTaskFiles(t)[0].Isolation != task.IsolationHere {
		t.Fatal("default isolation not used")
	}
	if _, err := runHost(t, "", "task", "new", "y", "--project", "api", "--tool", "fake", "--here", "--worktree"); err == nil {
		t.Fatal("both flags accepted")
	}
}

func TestNewRejectsBadFrom(t *testing.T) {
	_, repo, _ := taskTestEnv(t)
	for _, from := range []string{"--upload-pack=x", "-x", "nope", "a..b"} {
		if _, err := runHost(t, "", "task", "new", "x", "--project", "api", "--tool", "fake", "--worktree", "--from="+from); err == nil {
			t.Errorf("--from %q accepted", from)
		}
	}
	if branchExists(t, repo, "task/x") {
		t.Fatal("branch created")
	}
}

func setupRepo(t *testing.T, repo, setup string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, ".cairn.toml"), "[task]\nsetup = "+setup+"\n")
	git(t, repo, "add", ".cairn.toml")
	git(t, repo, "commit", "-q", "-m", "setup")
}

func TestNewCompensatesOnSetupFailure(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	setupRepo(t, repo, `["sh", "-c", "echo boom; exit 3"]`)
	before := len(historyEvents(t))
	out, err := runHost(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--worktree", "--approve-setup")
	if err == nil {
		t.Fatalf("setup failure not reported:\n%s", out)
	}
	if !strings.Contains(err.Error()+out, "boom") {
		t.Fatalf("setup output not shown: %v\n%s", err, out)
	}
	if branchExists(t, repo, "task/fix-login") {
		t.Fatal("branch left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees/api/fix-login")); !os.IsNotExist(err) {
		t.Fatalf("worktree left behind: %v", err)
	}
	if ts := loadTaskFiles(t); len(ts) != 0 {
		t.Fatalf("task file written: %+v", ts)
	}
	if strings.Contains(apiProjectText(t, dir), "task-") {
		t.Fatalf("window bound:\n%s", apiProjectText(t, dir))
	}
	if n := len(historyEvents(t)); n != before {
		t.Fatalf("history grew by %d", n-before)
	}
	for _, c := range listConvs(t, dir) {
		t.Fatalf("conversation left: %+v", c)
	}
	if got := git(t, repo, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Fatalf("worktree still registered:\n%s", got)
	}
}

func TestNewSetupApproval(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	setupRepo(t, repo, `["touch", "setup-ran"]`)
	// On a pipe without --approve-setup: shown, not run, the task is made.
	out := mustRun(t, "", "task", "new", "One", "--project", "api", "--tool", "fake", "--worktree")
	wt1 := filepath.Join(dir, "state/cairn/worktrees/api/one")
	if _, err := os.Stat(filepath.Join(wt1, "setup-ran")); !os.IsNotExist(err) {
		t.Fatal("setup ran without approval")
	}
	if !strings.Contains(out, "touch setup-ran") || !strings.Contains(out, "--approve-setup") {
		t.Fatalf("setup not shown:\n%s", out)
	}
	// Approved once for the project: it runs in the worktree, and the next
	// task runs it without asking.
	mustRun(t, "", "task", "new", "Two", "--project", "api", "--tool", "fake", "--worktree", "--approve-setup")
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees/api/two/setup-ran")); err != nil {
		t.Fatalf("approved setup did not run in the worktree: %v", err)
	}
	mustRun(t, "", "task", "new", "Three", "--project", "api", "--tool", "fake", "--worktree")
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees/api/three/setup-ran")); err != nil {
		t.Fatalf("trusted setup did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "setup-ran")); !os.IsNotExist(err) {
		t.Fatal("setup ran in the main checkout")
	}
}

func TestTaskUndoRemovesCleanWorktree(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--worktree")
	wt := filepath.Join(dir, "state/cairn/worktrees/api/fix-login")
	out := mustRun(t, "", "undo")
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree kept: %v\n%s", err, out)
	}
	if branchExists(t, repo, "task/fix-login") {
		t.Fatalf("branch kept:\n%s", out)
	}
	if ts := loadTaskFiles(t); len(ts) != 0 {
		t.Fatalf("task file kept: %+v", ts)
	}
	if strings.Contains(apiProjectText(t, dir), "task-fix-login") {
		t.Fatal("window binding kept")
	}
	// With work in the worktree, undo keeps worktree and branch and says so.
	mustRun(t, "", "task", "new", "Keep me", "--project", "api", "--tool", "fake", "--worktree")
	wt = filepath.Join(dir, "state/cairn/worktrees/api/keep-me")
	writeFile(t, filepath.Join(wt, "work.go"), "package x\n")
	git(t, wt, "add", "work.go")
	git(t, wt, "commit", "-q", "-m", "work")
	writeFile(t, filepath.Join(wt, "notes"), "wip\n")
	out = mustRun(t, "", "undo")
	if _, err := os.Stat(filepath.Join(wt, "notes")); err != nil {
		t.Fatal("work lost")
	}
	if !branchExists(t, repo, "task/keep-me") {
		t.Fatal("branch with a commit deleted")
	}
	if !strings.Contains(out, "remains") || !strings.Contains(out, "notes") {
		t.Fatalf("remains not reported:\n%s", out)
	}
}

func TestTaskUndoKeepsWorktreeOfRunningAgent(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Busy", "--project", "api", "--tool", "fake", "--worktree")
	var c conv.Conversation
	for _, x := range listConvs(t, dir) {
		c = x
	}
	rel, err := testStore(dir).AcquireLock(c.Tool, "cairn-"+c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	out := mustRun(t, "", "undo")
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees/api/busy")); err != nil {
		t.Fatalf("worktree of a running agent removed:\n%s", out)
	}
	if !branchExists(t, repo, "task/busy") || !strings.Contains(out, "runs") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestNewRecoversInterruptedTask(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	// A task new that died after creating branch and worktree: its journal
	// entry is left, unlocked, with no task file.
	wt := filepath.Join(dir, "state/cairn/worktrees/api/crashed")
	git(t, repo, "branch", "task/crashed")
	git(t, repo, "worktree", "add", "-q", wt, "task/crashed")
	base := git(t, repo, "rev-parse", "main")
	tk := task.Task{ID: conv.NewID(time.Now(), nil), Title: "crashed", Slug: "crashed", Project: "api", Isolation: task.IsolationWorktree,
		Status: task.StatusOpen, CreatedAt: time.Now().UTC(),
		Repos: []task.Repo{{Repo: repo, Base: "main", BaseCommit: base, Branch: "task/crashed", Worktree: wt}}}
	b, err := tk.Encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "state/cairn/agents/tasks-pending", tk.ID+".toml"), string(b))
	out := mustRun(t, "", "task", "ls")
	if !strings.Contains(out, "interrupted") {
		t.Fatalf("recovery not reported:\n%s", out)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("orphan worktree kept")
	}
	if branchExists(t, repo, "task/crashed") {
		t.Fatal("orphan branch kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "state/cairn/agents/tasks-pending", tk.ID+".toml")); !os.IsNotExist(err) {
		t.Fatal("journal entry kept")
	}
}

func TestTaskLsAndOpen(t *testing.T) {
	dir, _, fd := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--here")
	out := mustRun(t, "", "task", "ls")
	if !strings.Contains(out, "fix-login") || !strings.Contains(out, "open") {
		t.Fatalf("ls:\n%s", out)
	}
	var rows []struct {
		ID, Slug, Project, Status, Branch string
		Conversations                     int `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "", "task", "ls", "--json")), &rows); err != nil || len(rows) != 1 ||
		rows[0].Branch != "task/fix-login" || rows[0].Conversations != 1 {
		t.Fatalf("json %+v %v", rows, err)
	}
	if out := mustRun(t, "", "task", "ls", "--status", "done"); strings.Contains(out, "fix-login") {
		t.Fatalf("status filter:\n%s", out)
	}
	mustRun(t, "", "task", "open", "fix-login")
	if !fd.called("SelectPane ") {
		t.Fatalf("open did not jump: %v", fd.calls)
	}
	if fd.pane("api/task-fix-login/1") == nil {
		t.Fatalf("task window not created: %v", fd.calls)
	}
	// open with the binding gone re-creates the pane in the task window.
	c := listConvs(t, dir)[0]
	mustRun(t, "", "agents", "archive", c.ID)
	if strings.Contains(apiProjectText(t, dir), "task-fix-login") {
		t.Fatal("archive kept the window")
	}
	mustRun(t, "", "task", "open", "fix-login")
	if !strings.Contains(apiProjectText(t, dir), "task-fix-login") {
		t.Fatalf("open did not rebind:\n%s", apiProjectText(t, dir))
	}
	if _, err := runHost(t, "", "task", "open", "nope"); err == nil {
		t.Fatal("unknown task opened")
	}
}

func TestAgentsNewForTaskUsesItsWindowAndWorktree(t *testing.T) {
	dir, _, _ := taskTestEnv(t)
	mustRun(t, "", "task", "new", "Pair", "--project", "api", "--tool", "fake", "--worktree")
	mustRun(t, "", "agents", "new", "fake", "--task", "pair", "--name", "reviewer")
	wt := filepath.Join(dir, "state/cairn/worktrees/api/pair")
	var rv conv.Conversation
	for _, c := range listConvs(t, dir) {
		if c.Name == "reviewer" {
			rv = c
		}
	}
	tk := loadTaskFiles(t)[0]
	if rv.Task != tk.ID || rv.Cwd != wt || rv.Project != "api" || rv.Pane != "api/task-pair/2" {
		t.Fatalf("second conversation %+v", rv)
	}
}

func TestAgentsNewQueuedPrintsPosition(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	t.Setenv("CAIRN_AGENTS_MAX_RUNNING", "1")
	putConvs(t, dir, conv.Conversation{ID: "01RUNNING00000000000000000", Tool: "fake", State: conv.StateWorking, Project: "demo"})
	rel, err := testStore(dir).AcquireLock("fake", "cairn-01RUNNING00000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	out := mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "later")
	if !strings.Contains(out, "queued") || !strings.Contains(out, "position 1") {
		t.Fatalf("out:\n%s", out)
	}
	for _, c := range listConvs(t, dir) {
		if c.Name == "later" && c.State != conv.StateQueued {
			t.Fatalf("state %q", c.State)
		}
	}
}

func TestQueuePromotesOnDone(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	t.Setenv("CAIRN_AGENTS_MAX_RUNNING", "1")
	now := time.Now().UTC()
	putConvs(t, dir,
		conv.Conversation{ID: "01RUNNING00000000000000000", Tool: "fake", State: conv.StateWorking, LastActivityAt: now.Add(-time.Hour)},
		conv.Conversation{ID: "01QUEUEDB00000000000000000", Tool: "fake", State: conv.StateQueued, LastActivityAt: now.Add(-time.Minute)},
		conv.Conversation{ID: "01QUEUEDA00000000000000000", Tool: "fake", State: conv.StateQueued, LastActivityAt: now.Add(-2 * time.Minute)})
	rel, err := testStore(dir).AcquireLock("fake", "cairn-01RUNNING00000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	// While the running one works, an event does not promote anyone.
	mustRun(t, "", "event", "--conversation", "01RUNNING00000000000000000", "--kind", "waiting")
	if c, _, _ := testStore(dir).Get("01QUEUEDA00000000000000000"); c.State != conv.StateQueued {
		t.Fatalf("promoted while full: %q", c.State)
	}
	mustRun(t, "", "event", "--conversation", "01RUNNING00000000000000000", "--kind", "done")
	a, _, _ := testStore(dir).Get("01QUEUEDA00000000000000000")
	b, _, _ := testStore(dir).Get("01QUEUEDB00000000000000000")
	if a.State != conv.StateStarting || b.State != conv.StateQueued {
		t.Fatalf("after done: oldest %q, newer %q", a.State, b.State)
	}
}

func TestNewFailureRemovesOnlyWhatItMade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir, repo, _ := taskTestEnv(t)
	git(t, repo, "branch", "task/other")
	root := filepath.Join(dir, "state/cairn/worktrees/api")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	out, err := runHost(t, "", "task", "new", "Fix login", "--project", "api", "--tool", "fake", "--worktree")
	if err == nil {
		t.Fatalf("worktree add into a read-only dir succeeded:\n%s", out)
	}
	if branchExists(t, repo, "task/fix-login") {
		t.Fatalf("the branch it made was left:\n%s", out)
	}
	if !branchExists(t, repo, "task/other") {
		t.Fatal("an unrelated branch was deleted")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "state/cairn/agents/tasks-pending")); len(entries) > 1 {
		t.Fatalf("journal entries left: %v", entries)
	}
}

func TestConcurrentNewSameTitleGetsTwoBranches(t *testing.T) {
	dir, repo, _ := taskTestEnv(t)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			out, err := runHost(t, "", "task", "new", "Same", "--project", "api", "--tool", "fake", "--worktree")
			if err != nil {
				err = fmt.Errorf("%w\n%s", err, out)
			}
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []string{"task/same", "task/same-2"} {
		if !branchExists(t, repo, b) {
			t.Fatalf("%s missing", b)
		}
	}
	for _, s := range []string{"same", "same-2"} {
		if _, err := os.Stat(filepath.Join(dir, "state/cairn/worktrees/api", s, "README")); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(loadTaskFiles(t)); n != 2 {
		t.Fatalf("%d tasks", n)
	}
}

func TestArchiveTakesWaitingPaneOutOfQueue(t *testing.T) {
	dir, _, _ := agentsTestEnv(t)
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "waiter")
	c := onlyConv(t, dir)
	if _, err := testStore(dir).Update(c.ID, func(r *conv.Conversation) error { r.State = conv.StateQueued; return nil }); err != nil {
		t.Fatal(err)
	}
	// Its pane waits in the queue, holding the conversation lock.
	rel, err := testStore(dir).AcquireLock(c.Tool, "cairn-"+c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	mustRun(t, "", "agents", "archive", c.ID)
	if got := onlyConv(t, dir); got.ArchivedAt == nil || got.State != conv.StateStopped {
		t.Fatalf("after archive %+v", got)
	}
	// A conversation whose tool runs is still refused.
	mustRun(t, "", "agents", "new", "fake", "--project", "demo", "--name", "busy")
	var busy conv.Conversation
	for _, x := range listConvs(t, dir) {
		if x.Name == "busy" {
			busy = x
		}
	}
	if _, err := testStore(dir).Update(busy.ID, func(r *conv.Conversation) error { r.State = conv.StateWorking; return nil }); err != nil {
		t.Fatal(err)
	}
	rel2, err := testStore(dir).AcquireLock(busy.Tool, "cairn-"+busy.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	if _, err := runHost(t, "", "agents", "archive", busy.ID); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("running tool archived: %v", err)
	}
}
