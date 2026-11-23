package task

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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
	return porcelainFiles(out), nil
}

// porcelainFiles is the sorted file list of "status --porcelain=v1 -z".
func porcelainFiles(out []byte) []string {
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
	return files
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

// Unpushed lists the commits ("<short id> <subject>") of branch that are
// on none of bases (at least one: the task's base branch and/or its base
// commit) and on no remote-tracking branch, as seen from the checkout at
// path: the commits that exist nowhere else. A repository without remotes
// therefore lists only the commits beyond the bases, never their history.
func (g Git) Unpushed(path, branch string, bases ...string) ([]string, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	if err := checkBases(bases); err != nil {
		return nil, err
	}
	args := append([]string{"log", "--format=%h %s", "refs/heads/" + branch, "--not"}, bases...)
	return lines(g.in(path).run(append(args, "--remotes", "--")...))
}

// Unmerged lists the commits ("<short id> <subject>") of branch that are on
// none of bases.
func (g Git) Unmerged(branch string, bases ...string) ([]string, error) {
	if err := checkBranch(branch); err != nil {
		return nil, err
	}
	if err := checkBases(bases); err != nil {
		return nil, err
	}
	args := append([]string{"log", "--format=%h %s", "refs/heads/" + branch, "--not"}, bases...)
	return lines(g.run(append(args, "--")...))
}

func checkBases(bases []string) error {
	if len(bases) == 0 {
		return errors.New("no base to compare with")
	}
	for _, b := range bases {
		if err := CheckRef(b); err != nil {
			return err
		}
	}
	return nil
}

// remoteRe is a remote name cairn passes to git push.
var remoteRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidRemote reports whether name is a remote name cairn accepts.
func ValidRemote(name string) bool {
	return remoteRe.MatchString(name) && !strings.Contains(name, "..")
}

// Push pushes branch to the same name on remote and sets it as the
// branch's upstream. Not undoable: callers confirm first.
func (g Git) Push(remote, branch string) error {
	if !ValidRemote(remote) {
		return fmt.Errorf("remote %q is not a remote name", remote)
	}
	if err := checkBranch(branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	_, err := g.run("push", "--set-upstream", "--end-of-options", remote, ref+":"+ref)
	return err
}

// HasRemote reports whether the repository has a remote named name.
func (g Git) HasRemote(name string) (bool, error) {
	if !ValidRemote(name) {
		return false, fmt.Errorf("remote %q is not a remote name", name)
	}
	rs, err := lines(g.run("remote"))
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if r == name {
			return true, nil
		}
	}
	return false, nil
}

// IsAncestor reports whether commit a is an ancestor of (or equal to) b.
func (g Git) IsAncestor(a, b string) (bool, error) {
	if err := CheckRef(a); err != nil {
		return false, err
	}
	if err := CheckRef(b); err != nil {
		return false, err
	}
	out, err := g.R.Run(g.ctx(), "git", "-C", g.Dir, "merge-base", "--is-ancestor", "--end-of-options", a, b)
	if err == nil {
		return true, nil
	}
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base: %w: %s", err, bytes.TrimSpace(out))
}

// CheckedOutAt is the checkout (main or a worktree) that has the local
// branch checked out, or "".
func (g Git) CheckedOutAt(branch string) (string, error) {
	if err := checkBranch(branch); err != nil {
		return "", err
	}
	out, err := g.run("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	path := ""
	for _, f := range strings.Split(string(out), "\x00") {
		switch {
		case strings.HasPrefix(f, "worktree "):
			path = strings.TrimPrefix(f, "worktree ")
		case f == "branch refs/heads/"+branch:
			return path, nil
		}
	}
	return "", nil
}

// Changed lists the tracked files with changes (staged or not) of the
// checkout at path; untracked and ignored files are not listed.
func (g Git) Changed(path string) ([]string, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	out, err := g.in(path).run("status", "--porcelain=v1", "-z", "--untracked-files=no", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	return porcelainFiles(out), nil
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
