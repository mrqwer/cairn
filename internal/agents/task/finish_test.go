package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/execx"
)

// worktreeTask is a task in a new worktree of repo, branched from main.
func worktreeTask(t *testing.T, repo, slug string) Task {
	t.Helper()
	g := newGit(repo)
	base, err := g.ResolveCommit("main")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), slug)
	if err := g.CreateBranch("task/"+slug, base); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(wt, "task/"+slug); err != nil {
		t.Fatal(err)
	}
	return Task{ID: "01JDTESTTASK0000000000000" + strings.ToUpper(slug[:1]), Title: slug, Slug: slug, Project: "api",
		Isolation: IsolationWorktree, Status: StatusOpen, CreatedAt: time.Now().UTC(),
		Repos: []Repo{{Repo: repo, Base: "main", BaseCommit: base, Branch: "task/" + slug, Worktree: wt}}}
}

// hereTask is a task whose branch is checked out in repo itself.
func hereTask(t *testing.T, repo, slug string) Task {
	t.Helper()
	g := newGit(repo)
	base, err := g.ResolveCommit("main")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SwitchNew("task/"+slug, base); err != nil {
		t.Fatal(err)
	}
	return Task{ID: "01JDTESTTASK0000000000000H", Title: slug, Slug: slug, Project: "api",
		Isolation: IsolationHere, Status: StatusOpen, CreatedAt: time.Now().UTC(),
		Repos: []Repo{{Repo: repo, Base: "main", BaseCommit: base, Branch: "task/" + slug, Return: "main"}}}
}

func commitFile(t *testing.T, dir, name, text string) {
	t.Helper()
	write(t, filepath.Join(dir, name), text, 0o644)
	run(t, dir, "add", name)
	run(t, dir, "commit", "-q", "-m", "change "+name)
}

func has(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestUnpushedComparesAgainstBase(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	wt := tk.Repos[0].Worktree
	// Without a remote, only the task's own commits are unpushed, never
	// the base history ("init").
	commitFile(t, wt, "a.txt", "a\n")
	u, err := g.Unpushed(wt, "task/x", "refs/heads/main", tk.Repos[0].BaseCommit)
	if err != nil || len(u) != 1 || !strings.Contains(u[0], "change a.txt") {
		t.Fatalf("unpushed %q %v", u, err)
	}
	if _, err := g.Unpushed(wt, "task/x"); err == nil {
		t.Fatal("unpushed without a base accepted")
	}
	// Pushed to a remote: nothing is unpushed, but the commit is still
	// unmerged.
	bare := filepath.Join(t.TempDir(), "origin.git")
	run(t, repo, "init", "-q", "--bare", bare)
	run(t, repo, "remote", "add", "origin", bare)
	if err := g.Push("origin", "task/x"); err != nil {
		t.Fatal(err)
	}
	if u, err := g.Unpushed(wt, "task/x", "refs/heads/main"); err != nil || len(u) != 0 {
		t.Fatalf("after push %q %v", u, err)
	}
	if m, err := g.Unmerged("task/x", "refs/heads/main"); err != nil || len(m) != 1 {
		t.Fatalf("unmerged %q %v", m, err)
	}
}

func TestPlanListsIrreversible(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")

	p, err := Plan(g, tk, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Irreversible) == 0 || !has(p.Irreversible, "merge task/x into main") {
		t.Fatalf("merge irreversible %q", p.Irreversible)
	}
	if !has(stepTexts(p), "remove worktree") || !has(stepTexts(p), "delete branch task/x") {
		t.Fatalf("merge steps %q", stepTexts(p))
	}
	if len(p.Loss) != 0 || len(p.Blocked) != 0 {
		t.Fatalf("merge loss %q blocked %q", p.Loss, p.Blocked)
	}

	p, err = Plan(g, tk, ModePR)
	if err != nil {
		t.Fatal(err)
	}
	if !has(p.Irreversible, "push") || !has(p.Irreversible, "pull request") {
		t.Fatalf("pr irreversible %q", p.Irreversible)
	}

	// Abandon: the commit was never pushed, so it is a loss, and deleting
	// it is not undoable.
	p, err = Plan(g, tk, ModeAbandon)
	if err != nil {
		t.Fatal(err)
	}
	if !has(p.Loss, "change a.txt") || has(p.Loss, "init") {
		t.Fatalf("abandon loss %q", p.Loss)
	}
	if !has(p.Irreversible, "delete branch task/x") {
		t.Fatalf("abandon irreversible %q", p.Irreversible)
	}

	// A clean task without commits: abandon loses nothing and has no
	// irreversible step.
	clean := worktreeTask(t, repo, "y")
	p, err = Plan(g, clean, ModeAbandon)
	if err != nil || len(p.Loss) != 0 || len(p.Irreversible) != 0 {
		t.Fatalf("clean abandon %+v %v", p, err)
	}
	if _, err := Plan(g, clean, "squash"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func stepTexts(p FinishPlan) []string {
	var s []string
	for _, st := range p.Steps {
		s = append(s, st.Text)
	}
	return s
}

func TestPlanLossListsWorktreeWork(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	wt := tk.Repos[0].Worktree
	write(t, filepath.Join(wt, "README"), "edited\n", 0o644)
	write(t, filepath.Join(wt, "notes.md"), "n\n", 0o644)
	write(t, filepath.Join(wt, ".env"), "SECRET=1\n", 0o600)
	for _, mode := range []string{ModeMerge, ModePR, ModeAbandon} {
		p, err := Plan(g, tk, mode)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"uncommitted README", "uncommitted notes.md", "ignored .env"} {
			if !slices.Contains(p.Loss, want) {
				t.Errorf("%s: loss %q lacks %q", mode, p.Loss, want)
			}
		}
	}
}

func TestFinishRefusesLossWithoutForce(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	wt := tk.Repos[0].Worktree
	write(t, filepath.Join(wt, "notes.md"), "n\n", 0o644)
	p, err := Plan(g, tk, ModeAbandon)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(g, tk, p, FinishOptions{}); !errors.Is(err, ErrLoss) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "notes.md")); err != nil {
		t.Fatal("work lost without force")
	}
	if ok, _ := g.BranchExists("task/x"); !ok {
		t.Fatal("branch deleted")
	}
	// Forced: exactly the listed loss goes.
	res, err := Finish(g, tk, p, FinishOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Lost, []string{"uncommitted notes.md"}) {
		t.Fatalf("lost %q", res.Lost)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree kept")
	}
	if ok, _ := g.BranchExists("task/x"); ok {
		t.Fatal("branch without commits kept")
	}
}

func TestFinishKeepsWorkThatAppearedAfterThePlan(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	wt := tk.Repos[0].Worktree
	write(t, filepath.Join(wt, "a.md"), "a\n", 0o644)
	p, err := Plan(g, tk, ModeAbandon)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "b.md"), "b\n", 0o644) // not in the shown loss
	res, err := Finish(g, tk, p, FinishOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "b.md")); err != nil {
		t.Fatal("unlisted work removed")
	}
	if len(res.Lost) != 0 || !has(res.Left, "b.md") {
		t.Fatalf("result %+v", res)
	}
	if ok, _ := g.BranchExists("task/x"); !ok {
		t.Fatal("branch of a kept worktree deleted")
	}
}

func TestAbandonForceDeletesUnpushedBranchAndListsCommits(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")
	p, err := Plan(g, tk, ModeAbandon)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(g, tk, p, FinishOptions{}); !errors.Is(err, ErrLoss) {
		t.Fatalf("err %v", err)
	}
	if ok, _ := g.BranchExists("task/x"); !ok {
		t.Fatal("branch deleted without force")
	}
	res, err := Finish(g, tk, p, FinishOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.BranchExists("task/x"); ok {
		t.Fatal("forced abandon kept the branch")
	}
	if !has(res.Lost, "change a.txt") {
		t.Fatalf("lost %q", res.Lost)
	}
}

func TestMergeIntoCheckedOutBaseFastForwards(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")
	tip := run(t, repo, "rev-parse", "task/x")
	p, err := Plan(g, tk, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Finish(g, tk, p, FinishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, repo, "rev-parse", "main"); got != tip {
		t.Fatalf("main %s, want fast-forward to %s", got, tip)
	}
	if b, err := os.ReadFile(filepath.Join(repo, "a.txt")); err != nil || string(b) != "a\n" {
		t.Fatalf("main checkout not updated: %q %v", b, err)
	}
	if ok, _ := g.BranchExists("task/x"); ok {
		t.Fatal("merged branch kept")
	}
	if len(res.Left) != 0 {
		t.Fatalf("left %q", res.Left)
	}
}

func TestMergeConflictInCheckoutIsAborted(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "README", "task\n")
	commitFile(t, repo, "README", "main\n")
	head := run(t, repo, "rev-parse", "HEAD")
	p, err := Plan(g, tk, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(g, tk, p, FinishOptions{}); err == nil || !strings.Contains(err.Error(), "README") {
		t.Fatalf("conflict err %v", err)
	}
	if got := run(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatal("main moved")
	}
	if st := run(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("checkout left dirty:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatal("merge left in progress")
	}
	if _, err := os.Stat(tk.Repos[0].Worktree); err != nil {
		t.Fatal("worktree removed after a failed merge")
	}
	if ok, _ := g.BranchExists("task/x"); !ok {
		t.Fatal("branch deleted after a failed merge")
	}
}

func TestMergeIntoBaseNotCheckedOut(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	run(t, repo, "switch", "-q", "-c", "elsewhere")
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")
	// main moved on without conflict: a merge commit is made without
	// touching any checkout.
	run(t, repo, "switch", "-q", "main")
	commitFile(t, repo, "b.txt", "b\n")
	run(t, repo, "switch", "-q", "elsewhere")
	p, err := Plan(g, tk, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(g, tk, p, FinishOptions{}); err != nil {
		t.Fatal(err)
	}
	parents := strings.Fields(run(t, repo, "log", "-1", "--format=%P", "main"))
	if len(parents) != 2 {
		t.Fatalf("main is not a merge: %v", parents)
	}
	if got := run(t, repo, "show", "main:a.txt"); got != "a" {
		t.Fatalf("a.txt %q", got)
	}
	if cur, _ := g.CurrentBranch(); cur != "elsewhere" {
		t.Fatalf("checkout switched to %q", cur)
	}

	// A conflict: nothing changes.
	tk2 := worktreeTask(t, repo, "y")
	commitFile(t, tk2.Repos[0].Worktree, "a.txt", "y\n")
	run(t, repo, "switch", "-q", "main")
	commitFile(t, repo, "a.txt", "main\n")
	run(t, repo, "switch", "-q", "elsewhere")
	before := run(t, repo, "rev-parse", "main")
	p, err = Plan(g, tk2, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(g, tk2, p, FinishOptions{}); err == nil || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("conflict err %v", err)
	}
	if got := run(t, repo, "rev-parse", "main"); got != before {
		t.Fatal("main moved on a conflict")
	}
	if _, err := os.Stat(tk2.Repos[0].Worktree); err != nil {
		t.Fatal("worktree removed")
	}
}

func TestMergeRefusesDirtyBaseCheckout(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")
	write(t, filepath.Join(repo, "README"), "local edit\n", 0o644)
	p, err := Plan(g, tk, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if !has(p.Blocked, "uncommitted") {
		t.Fatalf("blocked %q", p.Blocked)
	}
	if _, err := Finish(g, tk, p, FinishOptions{Force: true}); err == nil {
		t.Fatal("merged into a dirty checkout")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "README")); string(b) != "local edit\n" {
		t.Fatal("local edit lost")
	}
}

func TestMergeRefusesBaseThatIsNotABranch(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := worktreeTask(t, repo, "x")
	tk.Repos[0].Base = tk.Repos[0].BaseCommit
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")
	for _, mode := range []string{ModeMerge, ModePR} {
		p, err := Plan(g, tk, mode)
		if err != nil {
			t.Fatal(err)
		}
		if !has(p.Blocked, "not a local branch") {
			t.Fatalf("%s blocked %q", mode, p.Blocked)
		}
	}
}

func TestHereFinishKeepsCheckoutFiles(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := hereTask(t, repo, "h")
	commitFile(t, repo, "a.txt", "a\n")
	write(t, filepath.Join(repo, ".env"), "SECRET=1\n", 0o600)
	write(t, filepath.Join(repo, "notes.md"), "mine\n", 0o644)
	p, err := Plan(g, tk, ModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Loss) != 0 || len(p.Blocked) != 0 {
		t.Fatalf("plan %+v", p)
	}
	if _, err := Finish(g, tk, p, FinishOptions{}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := g.CurrentBranch(); cur != "main" {
		t.Fatalf("checkout on %q", cur)
	}
	for _, f := range []string{".env", "notes.md", "a.txt", "README"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Fatalf("%s gone from the main checkout", f)
		}
	}
	if ok, _ := g.BranchExists("task/h"); ok {
		t.Fatal("merged branch kept")
	}
}

func TestHereFinishRefusesTrackedChanges(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	tk := hereTask(t, repo, "h")
	write(t, filepath.Join(repo, "README"), "edit\n", 0o644)
	for _, mode := range []string{ModeMerge, ModeAbandon} {
		p, err := Plan(g, tk, mode)
		if err != nil {
			t.Fatal(err)
		}
		if !has(p.Blocked, "README") {
			t.Fatalf("%s blocked %q", mode, p.Blocked)
		}
		if _, err := Finish(g, tk, p, FinishOptions{Force: true}); err == nil {
			t.Fatalf("%s ran on a changed main checkout", mode)
		}
	}
	if cur, _ := g.CurrentBranch(); cur != "task/h" {
		t.Fatalf("checkout on %q", cur)
	}
}

func TestPushAndPROrder(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	bare := filepath.Join(t.TempDir(), "origin.git")
	run(t, repo, "init", "-q", "--bare", bare)
	run(t, repo, "remote", "add", "origin", bare)
	tk := worktreeTask(t, repo, "x")
	commitFile(t, tk.Repos[0].Worktree, "a.txt", "a\n")
	p, err := Plan(g, tk, ModePR)
	if err != nil {
		t.Fatal(err)
	}
	// A failing PR command leaves the worktree and branch.
	_, err = Finish(g, tk, p, FinishOptions{Remote: "origin", OpenPR: func() error { return errors.New("no auth") }})
	if err == nil || !strings.Contains(err.Error(), "no auth") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(tk.Repos[0].Worktree); err != nil {
		t.Fatal("worktree removed after a failed PR")
	}
	if got := run(t, bare, "rev-parse", "task/x"); got != run(t, repo, "rev-parse", "task/x") {
		t.Fatal("branch not pushed")
	}
	opened := false
	if _, err := Finish(g, tk, p, FinishOptions{Remote: "origin", OpenPR: func() error { opened = true; return nil }}); err != nil {
		t.Fatal(err)
	}
	if !opened {
		t.Fatal("PR not opened")
	}
	if _, err := os.Stat(tk.Repos[0].Worktree); !os.IsNotExist(err) {
		t.Fatal("worktree of a pushed task kept")
	}
	if ok, _ := g.BranchExists("task/x"); !ok {
		t.Fatal("PR branch deleted")
	}
}

func TestFinishGitArgvSafety(t *testing.T) {
	f := &execx.FakeRunner{}
	g := Git{R: f, Dir: "/r"}
	checks := []error{g.Push("-o", "task/x"), g.Push("origin", "--all"), g.Push("--receive-pack=x", "task/x")}
	_, e1 := g.Unmerged("--all", "main")
	_, e2 := g.Unmerged("task/x", "--all")
	_, e3 := g.Unpushed("/w", "task/x", "-x")
	_, e4 := g.CheckedOutAt("-x")
	_, e5 := MergeInto(g, "--x", "task/x")
	checks = append(checks, e1, e2, e3, e4, e5)
	for i, err := range checks {
		if err == nil {
			t.Errorf("check %d accepted an option-like argument", i)
		}
	}
	if len(f.Calls) != 0 {
		t.Fatalf("git was run: %v", f.Calls)
	}
	f = &execx.FakeRunner{Responses: map[string]execx.Response{
		"git -C /r push --set-upstream --end-of-options origin refs/heads/task/x:refs/heads/task/x": {},
	}}
	g = Git{R: f, Dir: "/r", Ctx: context.Background()}
	if err := g.Push("origin", "task/x"); err != nil {
		t.Fatal(err)
	}
}

func TestHereAbandonSwitchesBack(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	run(t, repo, "switch", "-q", "-c", "feature")
	tk := hereTask(t, repo, "h")
	tk.Repos[0].Return = "feature"
	write(t, filepath.Join(repo, "notes.md"), "mine\n", 0o644)
	p, err := Plan(g, tk, ModeAbandon)
	if err != nil || len(p.Loss) != 0 || len(p.Blocked) != 0 {
		t.Fatalf("plan %+v %v", p, err)
	}
	if _, err := Finish(g, tk, p, FinishOptions{}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := g.CurrentBranch(); cur != "feature" {
		t.Fatalf("checkout on %q", cur)
	}
	if ok, _ := g.BranchExists("task/h"); ok {
		t.Fatal("branch without commits kept")
	}
	if _, err := os.Stat(filepath.Join(repo, "notes.md")); err != nil {
		t.Fatal("untracked file in the main checkout removed")
	}
}
