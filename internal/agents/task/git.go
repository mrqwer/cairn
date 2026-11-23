package task

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mrqwer/cairn/internal/execx"
)

// Git runs git in Dir as "git -C <Dir> ...". Every ref, branch and path is
// checked before it reaches argv (no leading "-") and sits behind "--" or
// --end-of-options where git takes one, so a value is never read as an
// option. Ctx nil is context.Background().
type Git struct {
	R   execx.Runner
	Dir string
	Ctx context.Context
}

func (g Git) ctx() context.Context {
	if g.Ctx != nil {
		return g.Ctx
	}
	return context.Background()
}

func (g Git) in(dir string) Git { g.Dir = dir; return g }

// run runs git and returns its output; a failure carries git's message.
func (g Git) run(args ...string) ([]byte, error) {
	out, err := g.R.Run(g.ctx(), "git", append([]string{"-C", g.Dir}, args...)...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return out, fmt.Errorf("git %s: %w", args[0], err)
		}
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return out, nil
}

func checkPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path %q is not absolute", p)
	}
	return nil
}

func checkBranch(b string) error {
	if err := CheckRef(b); err != nil {
		return err
	}
	if strings.HasPrefix(b, "refs/") || strings.HasSuffix(b, "/") {
		return fmt.Errorf("branch %q is not a short branch name", b)
	}
	return nil
}

// Root is the top-level directory of the checkout Dir is in.
func (g Git) Root() (string, error) {
	out, err := g.run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveCommit returns the full commit id ref names.
func (g Git) ResolveCommit(ref string) (string, error) {
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	out, err := g.run("rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%q is not a commit in %s", ref, g.Dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// BranchExists reports whether the local branch exists.
func (g Git) BranchExists(name string) (bool, error) {
	if err := checkBranch(name); err != nil {
		return false, err
	}
	out, err := g.R.Run(g.ctx(), "git", "-C", g.Dir, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+name)
	if err != nil {
		if len(bytes.TrimSpace(out)) == 0 {
			return false, nil // --quiet: a missing ref only sets the status
		}
		return false, fmt.Errorf("git rev-parse: %w: %s", err, bytes.TrimSpace(out))
	}
	return true, nil
}

// CreateBranch creates branch name at from, without upstream tracking.
func (g Git) CreateBranch(name, from string) error {
	if err := checkBranch(name); err != nil {
		return err
	}
	if err := CheckRef(from); err != nil {
		return err
	}
	_, err := g.run("branch", "--no-track", "--", name, from)
	return err
}

// DeleteBranch deletes the local branch, merged or not: callers check for
// new commits first (NewCommits).
func (g Git) DeleteBranch(name string) error {
	if err := checkBranch(name); err != nil {
		return err
	}
	_, err := g.run("branch", "-D", "--", name)
	return err
}

// AddWorktree checks out the existing branch in a new worktree at path.
func (g Git) AddWorktree(path, branch string) error {
	if err := checkPath(path); err != nil {
		return err
	}
	if err := checkBranch(branch); err != nil {
		return err
	}
	_, err := g.run("worktree", "add", "--", path, branch)
	return err
}

// RemoveWorktree removes the worktree at path. It is never forced: git
// refuses a worktree with modified or untracked files.
func (g Git) RemoveWorktree(path string) error {
	if err := checkPath(path); err != nil {
		return err
	}
	_, err := g.run("worktree", "remove", "--", path)
	return err
}

// Dirty lists the modified, staged, deleted and untracked (not ignored)
// files of the checkout at path, relative to it, sorted.
func (g Git) Dirty(path string) ([]string, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	out, err := g.in(path).run("status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	var files []string
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if len(r) < 4 {
			continue
		}
		files = append(files, r[3:])
		if r[0] == 'R' || r[0] == 'C' {
			i++ // the source path of a rename or copy follows
		}
	}
	sort.Strings(files)
	return files, nil
}

// Ignored lists the ignored files and directories (a directory as one
// entry ending in "/") of the checkout at path, relative to it, sorted.
func (g Git) Ignored(path string) ([]string, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	out, err := g.in(path).run("status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, r := range strings.Split(string(out), "\x00") {
		if strings.HasPrefix(r, "!! ") {
			files = append(files, r[3:])
		}
	}
	sort.Strings(files)
	return files, nil
}

// Tracked lists the files git tracks in the checkout at path.
func (g Git) Tracked(path string) (map[string]bool, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	out, err := g.in(path).run("ls-files", "-z")
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			m[f] = true
		}
	}
	return m, nil
}

// Unpushed lists the commits ("<short id> <subject>") of branch that no
// remote-tracking branch has, as seen from the checkout at path. Without
// remotes that is every commit of the branch.
func (g Git) Unpushed(path, branch string) ([]string, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	return lines(g.in(path).run("log", "--format=%h %s", branch, "--not", "--remotes", "--"))
}

// NewCommits lists the commits ("<short id> <subject>") on branch that are
// not on base.
func (g Git) NewCommits(base, branch string) ([]string, error) {
	if err := CheckRef(base); err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	return lines(g.run("log", "--format=%h %s", base+".."+branch, "--"))
}

// Merge merges branch into the branch checked out in Dir, always with a
// merge commit. Not undoable: callers confirm first.
func (g Git) Merge(branch string) error {
	if err := checkBranch(branch); err != nil {
		return err
	}
	_, err := g.run("merge", "--no-ff", "--no-edit", "--", branch)
	return err
}

// Diff is the change of branch since it left base (base...branch).
func (g Git) Diff(base, branch string) ([]byte, error) {
	if err := CheckRef(base); err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	return g.run("diff", base+"..."+branch, "--")
}

// CurrentBranch is the branch checked out in Dir ("" when detached).
func (g Git) CurrentBranch() (string, error) {
	out, err := g.R.Run(g.ctx(), "git", "-C", g.Dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if len(bytes.TrimSpace(out)) == 0 {
			return "", nil // detached
		}
		return "", fmt.Errorf("git symbolic-ref: %w: %s", err, bytes.TrimSpace(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// Switch checks out branch in Dir, or detaches at a commit id. git refuses
// when local changes would be overwritten.
func (g Git) Switch(ref string) error {
	if err := CheckRef(ref); err != nil {
		return err
	}
	if shaRe.MatchString(ref) {
		_, err := g.run("switch", "--detach", "--end-of-options", ref)
		return err
	}
	_, err := g.run("switch", "--no-guess", "--end-of-options", ref)
	return err
}

// SwitchNew creates branch name at from and checks it out in Dir.
func (g Git) SwitchNew(name, from string) error {
	if err := checkBranch(name); err != nil {
		return err
	}
	if err := CheckRef(from); err != nil {
		return err
	}
	_, err := g.run("switch", "--no-track", "-c", name, "--", from)
	return err
}

func lines(out []byte, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	var ls []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			ls = append(ls, l)
		}
	}
	return ls, nil
}
