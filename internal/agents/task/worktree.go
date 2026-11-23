package task

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

// IncludeFile lists, one per line, the files a new worktree gets copied
// from the main checkout (untracked files such as .env); RepoFile holds a
// repository's task settings (R8).
const (
	IncludeFile = ".worktreeinclude"
	RepoFile    = ".cairn.toml"
)

// within reports whether p is root or below it (both clean and absolute).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func firstElem(rel string) string {
	first, _, _ := strings.Cut(rel, "/")
	return first
}

// includeMatches reads repo's IncludeFile and returns the files it names,
// relative to repo, sorted and without duplicates. Lines are paths or
// globs relative to the repo; blank lines and "#" comments are skipped. An
// absolute path, a ".." element, anything in .git and a symlink that leads
// outside the repo are refused: the whole list is then an error. A
// directory stands for the regular files below it. No IncludeFile is an
// empty list.
func includeMatches(repo string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(repo, IncludeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(rel, path string) error {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", IncludeFile, rel, err)
		}
		if !within(realRepo, real) {
			return fmt.Errorf("%s: %s leads outside the repository (to %s); refused", IncludeFile, rel, real)
		}
		fi, err := os.Stat(real)
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() && !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
		return nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bad := ""
		switch {
		case filepath.IsAbs(line):
			bad = "is absolute"
		case strings.ContainsFunc(line, func(r rune) bool { return r < ' ' || r == 0x7f }):
			bad = "has control characters"
		default:
			for _, el := range strings.Split(filepath.ToSlash(line), "/") {
				if el == ".." {
					bad = "contains \"..\""
				}
			}
			if firstElem(filepath.Clean(line)) == ".git" {
				bad = "is inside .git"
			}
		}
		if bad != "" {
			return nil, fmt.Errorf("%s line %d: %q %s; refused", IncludeFile, n, line, bad)
		}
		ms, err := filepath.Glob(filepath.Join(repo, line))
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %q: %w", IncludeFile, n, line, err)
		}
		for _, m := range ms {
			rel, err := filepath.Rel(repo, m)
			if err == nil && firstElem(rel) == ".git" {
				continue // a glob such as "*" also matches .git: never copied
			}
			if err != nil || !within(repo, m) {
				return nil, fmt.Errorf("%s line %d: %q matches %s outside the repository; refused", IncludeFile, n, line, m)
			}
			fi, err := os.Stat(m) // follows a symlink: add checks where it leads
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", IncludeFile, rel, err)
			}
			if !fi.IsDir() {
				if err := add(rel, m); err != nil {
					return nil, err
				}
				continue
			}
			real, err := filepath.EvalSymlinks(m)
			if err != nil {
				return nil, err
			}
			if !within(realRepo, real) {
				return nil, fmt.Errorf("%s: %s leads outside the repository (to %s); refused", IncludeFile, rel, real)
			}
			err = filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				r, _ := filepath.Rel(repo, p)
				return add(r, p)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// destOK checks that writing rel below worktree goes through no symlink
// and replaces nothing: false when the file already exists (a tracked file
// of the checkout is never overwritten); an error when a parent directory
// is a symlink (the copy could land outside the worktree).
func destOK(worktree, rel string) (bool, error) {
	cur := worktree
	parts := strings.Split(rel, "/")
	for i, el := range parts {
		cur = filepath.Join(cur, el)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		last := i == len(parts)-1
		if last {
			return false, nil // exists (whatever it is): left alone
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return false, fmt.Errorf("%s: %s in the worktree is not a plain directory; refused", IncludeFile, cur)
		}
	}
	return false, nil
}

// CopyInclude copies the files repo's IncludeFile names into worktree,
// keeping each file's permission bits (a 0600 secret stays 0600), and
// returns what it copied (relative paths, sorted). The whole list is
// checked before anything is copied: a refused line (absolute, "..", a
// symlink out of the repo, .git, a symlinked directory in the worktree)
// copies nothing. Files the worktree already has are left alone.
func CopyInclude(repo, worktree string) ([]string, error) {
	rels, err := includeMatches(repo)
	if err != nil || len(rels) == 0 {
		return nil, err
	}
	var todo []string
	for _, rel := range rels {
		ok, err := destOK(worktree, rel)
		if err != nil {
			return nil, err
		}
		if ok {
			todo = append(todo, rel)
		}
	}
	var copied []string
	for _, rel := range todo {
		if err := copyFile(filepath.Join(repo, rel), filepath.Join(worktree, rel)); err != nil {
			return copied, fmt.Errorf("copy %s: %w", rel, err)
		}
		copied = append(copied, rel)
	}
	return copied, nil
}

func copyFile(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := fi.Mode().Perm()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(dst, perm) // the umask may have narrowed it
	}
	if werr != nil {
		_ = os.Remove(dst)
	}
	return werr
}

// worktreeWork inspects the worktree at wt of repo: work is what removal
// would lose that is never an ignorable leftover ("uncommitted <f>",
// "changed <f>" for an include copy that differs from the repo's
// original), ignored the ignored files and directories other than
// unchanged include copies, copies those unchanged include copies.
func worktreeWork(g Git, repo, wt string) (work, ignored, copies []string, err error) {
	dirty, err := g.Dirty(wt)
	if err != nil {
		return nil, nil, nil, err
	}
	ign, err := g.Ignored(wt)
	if err != nil {
		return nil, nil, nil, err
	}
	tracked, err := g.Tracked(wt)
	if err != nil {
		return nil, nil, nil, err
	}
	rels, err := includeMatches(repo)
	if err != nil {
		return []string{"the include list cannot be checked: " + err.Error()}, nil, nil, nil
	}
	isCopy := map[string]bool{}
	for _, rel := range rels {
		if tracked[rel] {
			continue
		}
		p := filepath.Join(wt, rel)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		a, err1 := os.ReadFile(p)
		b, err2 := os.ReadFile(filepath.Join(repo, rel))
		if err1 != nil || err2 != nil || !bytes.Equal(a, b) {
			work = append(work, "changed "+rel)
			continue
		}
		isCopy[rel] = true
		copies = append(copies, rel)
	}
	for _, f := range dirty {
		if !isCopy[f] {
			work = append(work, "uncommitted "+f)
		}
	}
	for _, f := range ign {
		if !isCopy[f] {
			ignored = append(ignored, f)
		}
	}
	return work, ignored, copies, nil
}

// WorktreeLoss lists what removing the worktree at wt would lose: its work
// (see worktreeWork) and "ignored <f>" for each ignored file other than an
// unchanged include copy. A worktree that is gone loses nothing.
func WorktreeLoss(g Git, repo, wt string) ([]string, error) {
	if _, err := os.Lstat(wt); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	work, ignored, _, err := worktreeWork(g.in(repo), repo, wt)
	if err != nil {
		return nil, err
	}
	for _, f := range ignored {
		work = append(work, "ignored "+f)
	}
	return work, nil
}

// SafeRemove removes the worktree only when nothing in it is work: no
// modified, staged or untracked file, no include copy that differs from the
// repo's original, and no ignored file other than unchanged include copies
// (a local database, notes, build output: git would delete them silently).
// Unchanged include copies are deleted first. Otherwise it returns false
// and the reasons; nothing is changed. force lets ignored files go (never
// uncommitted or changed ones): they are returned as lost. A worktree
// directory that is already gone is pruned and counts as removed.
func SafeRemove(g Git, repo, worktree string, force bool) (removed bool, reasons, lost []string, err error) {
	g = g.in(repo)
	if _, err := os.Lstat(worktree); errors.Is(err, fs.ErrNotExist) {
		_, err := g.run("worktree", "prune")
		return err == nil, nil, nil, err
	}
	work, extra, copies, err := worktreeWork(g, repo, worktree)
	if err != nil {
		return false, nil, nil, err
	}
	if len(work) > 0 {
		return false, work, nil, nil
	}
	if len(extra) > 0 && !force {
		for _, f := range extra {
			reasons = append(reasons, "ignored "+f)
		}
		return false, reasons, nil, nil
	}
	for _, rel := range copies {
		if err := os.Remove(filepath.Join(worktree, rel)); err != nil {
			return false, nil, nil, err
		}
	}
	if err := g.RemoveWorktree(worktree); err != nil {
		return false, nil, nil, err
	}
	return true, nil, extra, nil
}

// Compensate undoes what creating t did in git, as far as no work is lost
// (R3): each worktree is removed if SafeRemove allows it (force: see
// there), a task branch checked out in the main checkout is switched back
// to what was checked out before (Repo.Return), and the branch is deleted
// when it has no commits beyond its known base and nothing has it checked
// out. It returns what remains, one line each (empty when all is gone), and
// the ignored files force removed.
func Compensate(g Git, t Task, force bool) (left, lost []string) {
	for _, r := range t.Repos {
		gr := g.in(r.Repo)
		base := r.BaseCommit
		if base == "" {
			base = r.Base
		}
		exists, err := gr.BranchExists(r.Branch)
		if err != nil {
			left = append(left, fmt.Sprintf("branch %s in %s: %v", r.Branch, r.Repo, err))
			continue
		}
		var commits []string
		if exists && base != "" {
			if commits, err = gr.NewCommits(base, r.Branch); err != nil {
				left = append(left, fmt.Sprintf("branch %s in %s remains: %v", r.Branch, r.Repo, err))
				continue
			}
		}
		if r.Worktree != "" {
			removed, why, l, err := SafeRemove(gr, r.Repo, r.Worktree, force)
			for _, f := range l {
				lost = append(lost, filepath.Join(r.Worktree, f))
			}
			switch {
			case err != nil:
				left = append(left, fmt.Sprintf("worktree %s remains: %v", r.Worktree, err))
			case !removed:
				left = append(left, fmt.Sprintf("worktree %s remains: %s", r.Worktree, strings.Join(why, ", ")))
			}
			if err != nil || !removed {
				if exists {
					left = append(left, fmt.Sprintf("branch %s remains (checked out in %s)", r.Branch, r.Worktree))
				}
				continue
			}
		} else if exists {
			cur, err := gr.CurrentBranch()
			if err == nil && cur == r.Branch {
				back := r.Return
				if back == "" {
					// A task recorded before Return existed: its base, if
					// that is a local branch.
					if ok, _ := gr.BranchExists(r.Base); ok && r.Base != "" {
						back = r.Base
					}
				}
				switch {
				case base == "" || len(commits) > 0:
					left = append(left, fmt.Sprintf("branch %s remains checked out in %s (%d new commits)", r.Branch, r.Repo, len(commits)))
					continue
				case back == "":
					left = append(left, fmt.Sprintf("branch %s remains checked out in %s: what was checked out before is unknown", r.Branch, r.Repo))
					continue
				}
				if err := gr.Switch(back); err != nil {
					left = append(left, fmt.Sprintf("branch %s remains checked out in %s: %v", r.Branch, r.Repo, err))
					continue
				}
			}
		}
		switch {
		case !exists:
		case base == "":
			left = append(left, fmt.Sprintf("branch %s remains: its base is unknown, so its new commits cannot be counted", r.Branch))
		case len(commits) > 0:
			left = append(left, fmt.Sprintf("branch %s remains: %d new commits (%s)", r.Branch, len(commits), commits[0]))
		default:
			if err := gr.DeleteBranch(r.Branch); err != nil {
				left = append(left, fmt.Sprintf("branch %s remains: %v", r.Branch, err))
			}
		}
	}
	return left, lost
}

// ErrSetupNotApproved means the setup command was shown and not approved;
// it did not run.
var ErrSetupNotApproved = errors.New("the setup command was not approved; it did not run")

// TrustStore remembers, per project, the content hash of the repo file
// whose setup command the user approved (one per project: approving a
// changed file replaces the old approval). Path is a JSON file (0600).
type TrustStore struct {
	Root fsx.Root
	Path string
}

type trustFile struct {
	Projects map[string]string `json:"projects"`
}

func (t TrustStore) read() (trustFile, error) {
	var tf trustFile
	data, err := t.Root.ReadFile(t.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return trustFile{Projects: map[string]string{}}, nil
	}
	if err != nil {
		return tf, err
	}
	if err := json.Unmarshal(data, &tf); err != nil {
		return tf, fmt.Errorf("%s: %w", t.Path, err)
	}
	if tf.Projects == nil {
		tf.Projects = map[string]string{}
	}
	return tf, nil
}

// Trusted reports whether project's approved hash is hash.
func (t TrustStore) Trusted(project, hash string) (bool, error) {
	tf, err := t.read()
	return err == nil && tf.Projects[project] == hash, err
}

// Trust records hash as project's approved repo file.
func (t TrustStore) Trust(project, hash string) error {
	if err := t.Root.MkdirAll(filepath.Dir(t.Path), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(t.Root.Path(t.Path+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lf.Fd()), unix.LOCK_UN) }()
	tf, err := t.read()
	if err != nil {
		return err
	}
	tf.Projects[project] = hash
	b, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return t.Root.WriteAtomic(t.Path, append(b, '\n'), 0o600)
}

// SetupCommand reads the [task] setup argv of a repo file: nil when the
// file or the key is missing. It must be an array of strings, not a shell
// line.
func SetupCommand(data []byte) ([]string, error) {
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	tk, ok := m["task"].(map[string]any)
	if !ok {
		return nil, nil
	}
	raw, ok := tk["setup"]
	if !ok {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil, errors.New("task.setup must be a non-empty array of strings (an argv, not a shell line)")
	}
	argv := make([]string, len(arr))
	for i, v := range arr {
		s, ok := v.(string)
		if !ok || strings.ContainsRune(s, 0) {
			return nil, fmt.Errorf("task.setup[%d] must be a string", i)
		}
		argv[i] = s
	}
	if argv[0] == "" || strings.HasPrefix(argv[0], "-") {
		return nil, fmt.Errorf("task.setup[0] %q is not a command", argv[0])
	}
	return argv, nil
}

// Setup runs the setup command of repoFile (a repo's RepoFile) in the
// file's directory. Like direnv (R8), the file is ignored until trusted:
// the command is passed to approve, and runs only when approve says yes;
// that approval is remembered for project under the file's content hash,
// so any change of the file asks again. Without approval (approve nil or
// false) it returns ErrSetupNotApproved and runs nothing. No file or no
// setup key is nothing to do. A failed command is an error with the end
// of its output.
// SetupHash is the approval key of a repo file: the sha256 of its bytes.
func SetupHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func Setup(ctx context.Context, run execx.Runner, trust TrustStore, project, repoFile string, approve func(cmd []string, hash string) bool) error {
	data, err := os.ReadFile(repoFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	argv, err := SetupCommand(data)
	if err != nil {
		return fmt.Errorf("%s: %w", repoFile, err)
	}
	if argv == nil {
		return nil
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	ok, err := trust.Trusted(project, hash)
	if err != nil {
		return err
	}
	if !ok {
		if approve == nil || !approve(argv, hash) {
			return ErrSetupNotApproved
		}
		if err := trust.Trust(project, hash); err != nil {
			return err
		}
	}
	if r, ok := run.(execx.OSRunner); ok && r.Dir == "" {
		r.Dir = filepath.Dir(repoFile)
		run = r
	}
	out, err := run.Run(ctx, argv[0], argv[1:]...)
	if err != nil {
		return fmt.Errorf("setup %s failed: %w\n%s", strings.Join(argv, " "), err, tail(out, 20))
	}
	return nil
}

// tail is the last n lines of out.
func tail(out []byte, n int) string {
	ls := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
