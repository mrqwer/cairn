package task

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
)

// gitEnv isolates git from the user's configuration and identity: HOME and
// GIT_CONFIG_GLOBAL point into the test's temp dir, system config is off.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	gc := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(gc, []byte("[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"HOME": home, "GIT_CONFIG_GLOBAL": gc, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
	} {
		t.Setenv(k, v)
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "") // restored after the test
		_ = os.Unsetenv(k)
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

// tempRepo is a fresh repository with one commit on main.
func tempRepo(t *testing.T) string {
	t.Helper()
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "README"), "hello\n", 0o644)
	write(t, filepath.Join(dir, ".gitignore"), ".env\n", 0o644)
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

func newGit(dir string) Git { return Git{R: execx.OSRunner{}, Dir: dir} }

func TestGitBranchWorktreeLifecycle(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	root, err := g.Root()
	if err != nil || root != repo {
		t.Fatalf("root %q %v", root, err)
	}
	base, err := g.ResolveCommit("main")
	if err != nil || len(base) != 40 {
		t.Fatalf("resolve %q %v", base, err)
	}
	if _, err := g.ResolveCommit("nope"); err == nil {
		t.Fatal("unknown ref resolved")
	}
	if err := g.CreateBranch("task/x", "main"); err != nil {
		t.Fatal(err)
	}
	if ok, err := g.BranchExists("task/x"); err != nil || !ok {
		t.Fatalf("exists %v %v", ok, err)
	}
	if ok, err := g.BranchExists("task/y"); err != nil || ok {
		t.Fatalf("exists y %v %v", ok, err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.AddWorktree(wt, "task/x"); err != nil {
		t.Fatal(err)
	}
	if d, err := g.Dirty(wt); err != nil || len(d) != 0 {
		t.Fatalf("dirty %v %v", d, err)
	}
	write(t, filepath.Join(wt, "new file"), "x", 0o644)
	write(t, filepath.Join(wt, "README"), "changed\n", 0o644)
	write(t, filepath.Join(wt, ".env"), "ignored\n", 0o600)
	d, err := g.Dirty(wt)
	if err != nil || !slices.Equal(d, []string{"README", "new file"}) {
		t.Fatalf("dirty %q %v", d, err)
	}
	if err := g.RemoveWorktree(wt); err == nil {
		t.Fatal("dirty worktree removed")
	}
	run(t, wt, "add", ".")
	run(t, wt, "commit", "-q", "-m", "work")
	if c, err := g.NewCommits(base, "task/x"); err != nil || len(c) != 1 || !strings.Contains(c[0], "work") {
		t.Fatalf("new commits %q %v", c, err)
	}
	if u, err := g.Unpushed(wt, "task/x", base); err != nil || len(u) != 1 {
		t.Fatalf("unpushed %q %v", u, err)
	}
	diff, err := g.Diff(base, "task/x")
	if err != nil || !strings.Contains(string(diff), "+changed") {
		t.Fatalf("diff %s %v", diff, err)
	}
	if err := g.RemoveWorktree(wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if err := g.Merge("task/x"); err != nil {
		t.Fatal(err)
	}
	if c, _ := g.NewCommits(base, "main"); len(c) == 0 {
		t.Fatal("merge did not land")
	}
	if err := g.DeleteBranch("task/x"); err != nil {
		t.Fatal(err)
	}
	if cur, err := g.CurrentBranch(); err != nil || cur != "main" {
		t.Fatalf("current %q %v", cur, err)
	}
}

func TestGitRefusesOptionLikeArguments(t *testing.T) {
	f := &execx.FakeRunner{}
	g := Git{R: f, Dir: "/home/u/src/demo"}
	checks := []error{
		g.CreateBranch("-D", "main"),
		g.CreateBranch("task/x", "--upload-pack=x"),
		g.AddWorktree("rel/path", "task/x"),
		g.AddWorktree("/home/u/wt", "-b"),
		g.RemoveWorktree("-f"),
		g.DeleteBranch("--all"),
		g.Merge("--abort"),
		g.SwitchNew("x", "-"),
		g.Switch("-c"),
	}
	_, e1 := g.Diff("-p", "main")
	_, e2 := g.NewCommits("main", "--all")
	_, e3 := g.Unpushed("/home/u/wt", "--all", "main")
	_, e4 := g.ResolveCommit("--all")
	checks = append(checks, e1, e2, e3, e4)
	for i, err := range checks {
		if err == nil {
			t.Errorf("check %d accepted an option-like argument", i)
		}
	}
	if len(f.Calls) != 0 {
		t.Fatalf("git was run: %v", f.Calls)
	}
}

func TestGitUsesDashDashSeparators(t *testing.T) {
	f := &execx.FakeRunner{Responses: map[string]execx.Response{
		"git -C /r branch --no-track -- task/x main":  {},
		"git -C /r worktree add -- /w task/x":         {},
		"git -C /r worktree remove -- /w":             {},
		"git -C /r branch -D -- task/x":               {},
		"git -C /r merge --no-ff --no-edit -- task/x": {},
	}}
	g := Git{R: f, Dir: "/r", Ctx: context.Background()}
	for _, err := range []error{g.CreateBranch("task/x", "main"), g.AddWorktree("/w", "task/x"),
		g.RemoveWorktree("/w"), g.DeleteBranch("task/x"), g.Merge("task/x")} {
		if err != nil {
			t.Fatal(err)
		}
	}
}
