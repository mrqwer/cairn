package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

func TestCopyIncludeGlob(t *testing.T) {
	repo, wt := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, ".worktreeinclude"), "# secrets\n.env\n\nconfig/*.local\nmissing.txt\n", 0o644)
	write(t, filepath.Join(repo, ".env"), "TOKEN=1\n", 0o600)
	write(t, filepath.Join(repo, "config/a.local"), "a", 0o640)
	write(t, filepath.Join(repo, "config/b.local"), "b", 0o644)
	write(t, filepath.Join(repo, "config/c.toml"), "c", 0o644)
	// A file the checkout already has is not overwritten.
	write(t, filepath.Join(wt, "config/b.local"), "tracked", 0o644)
	got, err := CopyInclude(repo, wt)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{".env", "config/a.local"}) {
		t.Fatalf("copied %q", got)
	}
	fi, err := os.Stat(filepath.Join(wt, ".env"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(wt, "config/a.local")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("a.local mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "config/b.local")); string(b) != "tracked" {
		t.Fatalf("existing file overwritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(wt, "config/c.toml")); !os.IsNotExist(err) {
		t.Fatal("unlisted file copied")
	}
	// No include file: nothing to do.
	if got, err := CopyInclude(t.TempDir(), t.TempDir()); err != nil || got != nil {
		t.Fatalf("no include file: %v %v", got, err)
	}
}

func TestCopyIncludeRefusesEscape(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "s", 0o600)
	cases := map[string]func(repo, wt string){
		"dotdot":    func(repo, _ string) { write(t, filepath.Join(repo, ".worktreeinclude"), ".env\n../x\n", 0o644) },
		"dotdotmid": func(repo, _ string) { write(t, filepath.Join(repo, ".worktreeinclude"), "a/../../x\n", 0o644) },
		"absolute": func(repo, _ string) {
			write(t, filepath.Join(repo, ".worktreeinclude"), filepath.Join(outside, "secret")+"\n", 0o644)
		},
		"symlink out": func(repo, _ string) {
			write(t, filepath.Join(repo, ".worktreeinclude"), ".env\nlink\n", 0o644)
			if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
		},
		"symlink dir out": func(repo, _ string) {
			write(t, filepath.Join(repo, ".worktreeinclude"), "d/secret\n", 0o644)
			if err := os.Symlink(outside, filepath.Join(repo, "d")); err != nil {
				t.Fatal(err)
			}
		},
		"dest symlink out": func(repo, wt string) {
			write(t, filepath.Join(repo, ".worktreeinclude"), "conf/x\n", 0o644)
			write(t, filepath.Join(repo, "conf/x"), "x", 0o600)
			if err := os.Symlink(outside, filepath.Join(wt, "conf")); err != nil {
				t.Fatal(err)
			}
		},
		"git dir": func(repo, _ string) { write(t, filepath.Join(repo, ".worktreeinclude"), ".git/config\n", 0o644) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			repo, wt := t.TempDir(), t.TempDir()
			write(t, filepath.Join(repo, ".env"), "E", 0o600)
			write(t, filepath.Join(repo, ".git/config"), "[core]\n", 0o644)
			setup(repo, wt)
			got, err := CopyInclude(repo, wt)
			if err == nil {
				t.Fatalf("accepted, copied %q", got)
			}
			if len(got) != 0 {
				t.Fatalf("copied %q before refusing", got)
			}
			if _, err := os.Stat(filepath.Join(wt, ".env")); !os.IsNotExist(err) {
				t.Fatal(".env copied although the list was refused")
			}
			if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
				t.Fatal("wrote outside the worktree")
			}
		})
	}
}

// fakeSetup records the commands it ran and the directory.
type fakeSetup struct {
	calls [][]string
	err   error
}

func (f *fakeSetup) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return []byte("setup output\n"), f.err
}

func trustStore(t *testing.T) TrustStore {
	return TrustStore{Root: fsx.Root{Dir: "/"}, Path: filepath.Join(t.TempDir(), "trust.json")}
}

func TestSetupRequiresApproval(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, ".cairn.toml")
	write(t, file, "[task]\nsetup = [\"make\", \"deps; rm -rf /\"]\n", 0o644)
	trust := trustStore(t)
	run := &fakeSetup{}
	var shown [][]string
	deny := func(cmd []string, _ string) bool { shown = append(shown, cmd); return false }
	if err := Setup(ctx, run, trust, "demo", file, deny); !errors.Is(err, ErrSetupNotApproved) {
		t.Fatalf("denied: %v", err)
	}
	if err := Setup(ctx, run, trust, "demo", file, nil); !errors.Is(err, ErrSetupNotApproved) {
		t.Fatalf("no approver: %v", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("ran unapproved: %v", run.calls)
	}
	if len(shown) != 1 || !slices.Equal(shown[0], []string{"make", "deps; rm -rf /"}) {
		t.Fatalf("shown %q", shown)
	}
	approve := func(cmd []string, _ string) bool { shown = append(shown, cmd); return true }
	if err := Setup(ctx, run, trust, "demo", file, approve); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 1 || !slices.Equal(run.calls[0], []string{"make", "deps; rm -rf /"}) {
		t.Fatalf("calls %q", run.calls)
	}
	// Approved once per project: the next run asks nobody.
	if err := Setup(ctx, run, trust, "demo", file, deny); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 || len(shown) != 2 {
		t.Fatalf("calls %d shown %d", len(run.calls), len(shown))
	}
	// Another project is asked separately.
	if err := Setup(ctx, run, trust, "api", file, deny); !errors.Is(err, ErrSetupNotApproved) {
		t.Fatalf("other project: %v", err)
	}
	// A failing setup is an error with its output.
	run.err = errors.New("exit status 2")
	if err := Setup(ctx, run, trust, "demo", file, deny); err == nil || !strings.Contains(err.Error(), "setup output") {
		t.Fatalf("failure: %v", err)
	}
	// No file, or no setup key: nothing to run, nobody asked.
	n := len(run.calls)
	if err := Setup(ctx, run, trust, "demo", filepath.Join(dir, "none.toml"), deny); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "empty.toml"), "[task]\n", 0o644)
	if err := Setup(ctx, run, trust, "demo", filepath.Join(dir, "empty.toml"), deny); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != n {
		t.Fatal("ran without a setup command")
	}
	// A string is not an argv.
	write(t, filepath.Join(dir, "bad.toml"), "[task]\nsetup = \"make deps\"\n", 0o644)
	if err := Setup(ctx, run, trust, "demo", filepath.Join(dir, "bad.toml"), approve); err == nil {
		t.Fatal("string setup accepted")
	}
}

func TestSetupHashChangeAsksAgain(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), ".cairn.toml")
	write(t, file, "[task]\nsetup = [\"make\", \"deps\"]\n", 0o644)
	trust := trustStore(t)
	run := &fakeSetup{}
	asked := 0
	approve := func([]string, string) bool { asked++; return true }
	if err := Setup(ctx, run, trust, "demo", file, approve); err != nil {
		t.Fatal(err)
	}
	if err := Setup(ctx, run, trust, "demo", file, approve); err != nil || asked != 1 {
		t.Fatalf("asked %d %v", asked, err)
	}
	// Any change of the file, even outside the command, asks again.
	write(t, file, "# changed\n[task]\nsetup = [\"make\", \"deps\"]\n", 0o644)
	deny := func([]string, string) bool { asked++; return false }
	if err := Setup(ctx, run, trust, "demo", file, deny); !errors.Is(err, ErrSetupNotApproved) || asked != 2 {
		t.Fatalf("changed file: asked %d %v", asked, err)
	}
	if len(run.calls) != 2 {
		t.Fatalf("calls %d", len(run.calls))
	}
}

func TestSetupRunsInRepoDir(t *testing.T) {
	gitEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, ".cairn.toml")
	write(t, file, "[task]\nsetup = [\"touch\", \"made-by-setup\"]\n", 0o644)
	if err := Setup(context.Background(), execx.OSRunner{}, trustStore(t), "demo", file, func([]string, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "made-by-setup")); err != nil {
		t.Fatalf("setup did not run in the repo dir: %v", err)
	}
}

func TestSafeRemove(t *testing.T) {
	repo := tempRepo(t)
	write(t, filepath.Join(repo, ".worktreeinclude"), ".env\n", 0o644)
	run(t, repo, "add", ".worktreeinclude")
	run(t, repo, "commit", "-q", "-m", "include")
	write(t, filepath.Join(repo, ".env"), "TOKEN=1\n", 0o600)
	g := newGit(repo)
	mk := func(name string) string {
		wt := filepath.Join(t.TempDir(), name)
		if err := g.CreateBranch("task/"+name, "main"); err != nil {
			t.Fatal(err)
		}
		if err := g.AddWorktree(wt, "task/"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := CopyInclude(repo, wt); err != nil {
			t.Fatal(err)
		}
		return wt
	}
	// Clean apart from the unchanged include copy: removed.
	wt := mk("clean")
	if removed, why, _, err := SafeRemove(g, repo, wt, false); err != nil || !removed || len(why) != 0 {
		t.Fatalf("clean: %v %q %v", removed, why, err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("clean worktree left")
	}
	// A changed include copy is work: kept.
	wt = mk("env")
	write(t, filepath.Join(wt, ".env"), "TOKEN=mine\n", 0o600)
	if removed, why, _, err := SafeRemove(g, repo, wt, false); err != nil || removed || len(why) == 0 {
		t.Fatalf("changed include: %v %q %v", removed, why, err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, ".env")); string(b) != "TOKEN=mine\n" {
		t.Fatal("changed include lost")
	}
	// Untracked work: kept.
	wt = mk("untracked")
	write(t, filepath.Join(wt, "notes.md"), "n", 0o644)
	if removed, why, _, _ := SafeRemove(g, repo, wt, false); removed || len(why) == 0 || !strings.Contains(strings.Join(why, " "), "notes.md") {
		t.Fatalf("untracked: %v %q", removed, why)
	}
	if _, err := os.Stat(filepath.Join(wt, ".env")); err != nil {
		t.Fatal("include copy removed from a kept worktree")
	}
}

func TestCompensate(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	base, _ := g.ResolveCommit("main")
	mk := func(name string) Repo {
		wt := filepath.Join(t.TempDir(), name)
		if err := g.CreateBranch("task/"+name, base); err != nil {
			t.Fatal(err)
		}
		if err := g.AddWorktree(wt, "task/"+name); err != nil {
			t.Fatal(err)
		}
		return Repo{Repo: repo, Base: "main", BaseCommit: base, Branch: "task/" + name, Worktree: wt}
	}
	clean := mk("clean")
	if left, _ := Compensate(g, Task{Repos: []Repo{clean}}, false); len(left) != 0 {
		t.Fatalf("clean left %q", left)
	}
	if ok, _ := g.BranchExists("task/clean"); ok {
		t.Fatal("branch kept")
	}
	work := mk("work")
	write(t, filepath.Join(work.Worktree, "f"), "x", 0o644)
	run(t, work.Worktree, "add", "f")
	run(t, work.Worktree, "commit", "-q", "-m", "work")
	left, _ := Compensate(g, Task{Repos: []Repo{work}}, false)
	if ok, _ := g.BranchExists("task/work"); !ok {
		t.Fatal("branch with commits deleted")
	}
	if len(left) == 0 || !strings.Contains(strings.Join(left, "\n"), "task/work") {
		t.Fatalf("left %q", left)
	}
	// --here: the branch is switched back and deleted when it has no commits.
	if err := g.SwitchNew("task/here", base); err != nil {
		t.Fatal(err)
	}
	here := Repo{Repo: repo, Base: "main", BaseCommit: base, Branch: "task/here"}
	if left, _ := Compensate(g, Task{Isolation: IsolationHere, Repos: []Repo{here}}, false); len(left) != 0 {
		t.Fatalf("here left %q", left)
	}
	if cur, _ := g.CurrentBranch(); cur != "main" {
		t.Fatalf("checkout on %q", cur)
	}
}

func TestSafeRemoveRefusesIgnoredWork(t *testing.T) {
	repo := tempRepo(t)
	write(t, filepath.Join(repo, ".gitignore"), ".env\n*.db\n", 0o644)
	write(t, filepath.Join(repo, ".worktreeinclude"), ".env\n", 0o644)
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "ignore")
	write(t, filepath.Join(repo, ".env"), "TOKEN=1\n", 0o600)
	g := newGit(repo)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.CreateBranch("task/db", "main"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddWorktree(wt, "task/db"); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyInclude(repo, wt); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "local.db"), "rows", 0o644)
	removed, why, lost, err := SafeRemove(g, repo, wt, false)
	if err != nil || removed || !strings.Contains(strings.Join(why, " "), "local.db") || len(lost) != 0 {
		t.Fatalf("ignored work: %v %q %q %v", removed, why, lost, err)
	}
	if strings.Contains(strings.Join(why, " "), ".env") {
		t.Fatalf("an unchanged include copy counted as work: %q", why)
	}
	if _, err := os.Stat(filepath.Join(wt, "local.db")); err != nil {
		t.Fatal("ignored file lost")
	}
	// Forced: removed, and what was lost is listed.
	removed, _, lost, err = SafeRemove(g, repo, wt, true)
	if err != nil || !removed || !slices.Equal(lost, []string{"local.db"}) {
		t.Fatalf("forced: %v %q %v", removed, lost, err)
	}
	// Force never covers uncommitted (not ignored) work.
	if err := g.AddWorktree(wt, "task/db"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "notes.md"), "n", 0o644)
	if removed, _, _, _ := SafeRemove(g, repo, wt, true); removed {
		t.Fatal("forced removal lost untracked work")
	}
}

func TestCompensateUnknownBaseKeepsBranch(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	if err := g.CreateBranch("task/nobase", "main"); err != nil {
		t.Fatal(err)
	}
	left, _ := Compensate(g, Task{Repos: []Repo{{Repo: repo, Branch: "task/nobase"}}}, false)
	if ok, _ := g.BranchExists("task/nobase"); !ok {
		t.Fatal("branch with unknown base deleted")
	}
	if len(left) == 0 || !strings.Contains(left[0], "base") {
		t.Fatalf("left %q", left)
	}
}

func TestCompensateHereReturnsToPreviousBranch(t *testing.T) {
	repo := tempRepo(t)
	g := newGit(repo)
	base, _ := g.ResolveCommit("main")
	if err := g.SwitchNew("feature", base); err != nil {
		t.Fatal(err)
	}
	if err := g.SwitchNew("task/here", "main"); err != nil {
		t.Fatal(err)
	}
	r := Repo{Repo: repo, Base: "origin/main", BaseCommit: base, Branch: "task/here", Return: "feature"}
	if left, _ := Compensate(g, Task{Isolation: IsolationHere, Repos: []Repo{r}}, false); len(left) != 0 {
		t.Fatalf("left %q", left)
	}
	if cur, _ := g.CurrentBranch(); cur != "feature" {
		t.Fatalf("back on %q, want feature", cur)
	}
}

func TestCopyIncludeStarSkipsGitDir(t *testing.T) {
	repo, wt := t.TempDir(), t.TempDir()
	write(t, filepath.Join(repo, ".worktreeinclude"), "*\n", 0o644)
	write(t, filepath.Join(repo, ".git/config"), "[core]\n", 0o644)
	write(t, filepath.Join(repo, ".env"), "E", 0o600)
	got, err := CopyInclude(repo, wt)
	if err != nil || !slices.Contains(got, ".env") {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git copied")
	}
}

func TestJournalPendingDoesNotTouch(t *testing.T) {
	j := Journal{Dir: t.TempDir()}
	tk := Task{ID: "01JPENDING0000000000000000", Title: "x", Slug: "x", Project: "demo", Isolation: IsolationHere,
		Status: StatusOpen, Repos: []Repo{{Repo: "/home/u/src/demo", Base: "main", BaseCommit: strings.Repeat("a", 40), Branch: "task/x"}}}
	end, err := j.Begin(tk)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := j.Pending(); len(p) != 0 {
		t.Fatalf("live entry reported: %v", p)
	}
	// Simulate a dead creator: drop the lock but keep the file.
	data, _ := os.ReadFile(filepath.Join(j.Dir, tk.ID+".toml"))
	end()
	write(t, filepath.Join(j.Dir, tk.ID+".toml"), string(data), 0o600)
	p, err := j.Pending()
	if err != nil || len(p) != 1 || p[0].ID != tk.ID {
		t.Fatalf("pending %v %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(j.Dir, tk.ID+".toml")); err != nil {
		t.Fatal("Pending removed the entry")
	}
}

func TestSetupApproverSeesHash(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".cairn.toml")
	body := "[task]\nsetup = [\"make\"]\n"
	write(t, file, body, 0o644)
	var got string
	_ = Setup(context.Background(), &fakeSetup{}, trustStore(t), "demo", file, func(_ []string, h string) bool { got = h; return false })
	if got != SetupHash([]byte(body)) || len(got) != 64 {
		t.Fatalf("hash %q", got)
	}
}
