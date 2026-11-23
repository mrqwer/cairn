package task

import (
	"errors"
	"fmt"
	"strings"
)

// Finish modes: there is no default, and none is ever implied.
const (
	ModeMerge   = "merge"
	ModePR      = "pr"
	ModeAbandon = "abandon"
)

// Step is one step of a finish, in the order it runs. Irreversible steps
// are shown as "not undoable" and need confirmation (R3).
type Step struct {
	Text         string
	Irreversible bool
}

// FinishPlan is what finishing a task in a mode would do, worked out
// without changing anything. Loss lists what it would lose
// ("uncommitted <f>", "changed <f>", "ignored <f>" in the worktree,
// "commit <id> <subject>" for a commit that exists nowhere else): it runs
// only when that is empty or forced. Blocked lists why it cannot run at
// all; --force never overrides that.
type FinishPlan struct {
	Mode         string
	Steps        []Step
	Loss         []string
	Irreversible []string
	Blocked      []string
	// Unmerged are the branch's commits beyond its base, Unpushed those of
	// them on no remote.
	Unmerged []string
	Unpushed []string

	base         string   // the base as a local branch ("" if it is not one)
	bases        []string // what new commits are counted against
	switchTo     string   // a --here checkout on the task branch goes here
	deleteBranch bool
}

// ErrLoss means finishing would lose work and was not forced.
var ErrLoss = errors.New("finishing would lose work")

// FinishOptions are the caller's decisions. Force lets exactly the plan's
// Loss go (the caller showed it and the user confirmed). Remote and OpenPR
// are for ModePR: the branch is pushed to Remote (default origin), then
// OpenPR opens the pull request.
type FinishOptions struct {
	Force  bool
	Remote string
	OpenPR func() error
}

// FinishResult reports what was done, what remains (one line each) and
// what was lost (forced, as listed in the plan's Loss).
type FinishResult struct {
	Done, Left, Lost []string
}

// Plan works out finishing t in mode: merge into its base, push and open
// a pull request, or abandon. Nothing is changed.
func Plan(g Git, t Task, mode string) (FinishPlan, error) {
	switch mode {
	case ModeMerge, ModePR, ModeAbandon:
	default:
		return FinishPlan{}, fmt.Errorf("unknown finish mode %q (merge, pr or abandon)", mode)
	}
	if err := t.Validate(); err != nil {
		return FinishPlan{}, err
	}
	if len(t.Repos) != 1 {
		return FinishPlan{}, fmt.Errorf("task %s has %d repositories; finishing handles one", t.Slug, len(t.Repos))
	}
	r := t.Repos[0]
	g = g.in(r.Repo)
	p := FinishPlan{Mode: mode}
	block := func(f string, a ...any) { p.Blocked = append(p.Blocked, fmt.Sprintf(f, a...)) }
	add := func(irr bool, f string, a ...any) {
		p.Steps = append(p.Steps, Step{Text: fmt.Sprintf(f, a...), Irreversible: irr})
	}

	exists, err := g.BranchExists(r.Branch)
	if err != nil {
		return p, err
	}
	if !exists {
		block("branch %s does not exist in %s", r.Branch, r.Repo)
		return p, nil
	}
	if r.Base != "" && !shaRe.MatchString(r.Base) && checkBranch(r.Base) == nil {
		if ok, err := g.BranchExists(r.Base); err != nil {
			return p, err
		} else if ok {
			p.base = r.Base
			p.bases = append(p.bases, "refs/heads/"+r.Base)
		}
	}
	switch {
	case r.BaseCommit != "":
		p.bases = append(p.bases, r.BaseCommit)
	case r.Base != "" && p.base == "":
		if c, err := g.ResolveCommit(r.Base); err == nil {
			p.bases = append(p.bases, c)
		}
	}
	if len(p.bases) == 0 {
		block("the base of %s is unknown, so its new commits cannot be counted", r.Branch)
		return p, nil
	}
	if p.Unmerged, err = g.Unmerged(r.Branch, p.bases...); err != nil {
		return p, err
	}
	if p.Unpushed, err = g.Unpushed(r.Repo, r.Branch, p.bases...); err != nil {
		return p, err
	}
	baseName := r.Base
	if baseName == "" {
		baseName = r.BaseCommit
	}

	// The worktree: what removing it loses.
	var wtLoss []string
	if r.Worktree != "" {
		if wtLoss, err = WorktreeLoss(g, r.Repo, r.Worktree); err != nil {
			return p, err
		}
	}
	// A --here task: the main checkout is switched, never cleaned, so its
	// changes block a switch (untracked and ignored files stay as they are).
	onBranch := false
	if r.Worktree == "" {
		cur, err := g.CurrentBranch()
		if err != nil {
			return p, err
		}
		onBranch = cur == r.Branch
		if onBranch && mode != ModePR {
			changed, err := g.Changed(r.Repo)
			if err != nil {
				return p, err
			}
			if len(changed) > 0 {
				block("the main checkout %s has uncommitted changes (%s): commit or stash them; --force never discards them",
					r.Repo, strings.Join(changed, ", "))
			}
		}
	}
	removeStep := func() {
		if r.Worktree == "" {
			return
		}
		if len(wtLoss) > 0 {
			add(true, "remove worktree %s, losing what is listed", r.Worktree)
			return
		}
		add(false, "remove worktree %s", r.Worktree)
	}

	switch mode {
	case ModeMerge:
		if p.base == "" {
			block("the base %s is not a local branch: merge it yourself, or use --pr", baseName)
			break
		}
		if len(p.Unmerged) == 0 {
			add(false, "nothing to merge: %s has no commits beyond %s", r.Branch, p.base)
		} else {
			at, err := g.CheckedOutAt(p.base)
			if err != nil {
				return p, err
			}
			where := "(not checked out anywhere: only the branch moves)"
			if at != "" {
				where = "in " + at
				changed, err := g.Changed(at)
				if err != nil {
					return p, err
				}
				if len(changed) > 0 {
					block("%s, where %s is checked out, has uncommitted changes (%s): commit or stash them first",
						at, p.base, strings.Join(changed, ", "))
				}
				if g.in(at).mergeInProgress() {
					block("%s has a merge in progress: finish or abort it first", at)
				}
			}
			add(true, "merge %s into %s %s (%s)", r.Branch, p.base, where, plural(len(p.Unmerged), "commit"))
		}
		if onBranch {
			p.switchTo = p.base
			add(false, "switch %s to %s", r.Repo, p.base)
		}
		removeStep()
		p.deleteBranch = true
		add(false, "delete branch %s (merged into %s)", r.Branch, p.base)
		p.Loss = wtLoss
	case ModePR:
		if p.base == "" {
			block("the base %s is not a local branch: a pull request needs a target branch", baseName)
		}
		if len(p.Unmerged) == 0 {
			block("nothing to propose: %s has no commits beyond %s", r.Branch, baseName)
		}
		add(true, "push %s and set it as its upstream", r.Branch)
		add(true, "open a pull request of %s into %s", r.Branch, baseName)
		removeStep()
		add(false, "keep branch %s (the pull request's head)", r.Branch)
		p.Loss = wtLoss
	case ModeAbandon:
		if onBranch {
			back := r.Return
			if back == "" {
				back = p.base
			}
			if back == "" {
				block("what %s had checked out before the task is unknown: switch it yourself, then run again", r.Repo)
			}
			p.switchTo = back
			add(false, "switch %s back to %s", r.Repo, back)
		}
		removeStep()
		p.Loss = wtLoss
		switch {
		case len(p.Unmerged) == 0:
			p.deleteBranch = true
			add(false, "delete branch %s (no commits beyond %s)", r.Branch, baseName)
		case len(p.Unpushed) > 0:
			p.deleteBranch = true
			add(true, "delete branch %s with %s that exist nowhere else", r.Branch, plural(len(p.Unpushed), "commit"))
			for _, c := range p.Unpushed {
				p.Loss = append(p.Loss, "commit "+c)
			}
		default:
			add(false, "keep branch %s: its %s beyond %s are on a remote (delete it with: git branch -D %s)",
				r.Branch, plural(len(p.Unmerged), "commit"), baseName, r.Branch)
		}
	}
	for _, s := range p.Steps {
		if s.Irreversible {
			p.Irreversible = append(p.Irreversible, s.Text)
		}
	}
	return p, nil
}

// Finish runs plan p of task t. The first step that is not undoable (merge,
// or push and pull request) runs first; if it fails, nothing else is done.
// Then a --here checkout is switched, the worktree removed and the branch
// deleted as planned, each only when nothing beyond the plan's Loss (and
// only with o.Force) would be lost: work that appeared after the plan was
// shown is kept and reported in Left.
func Finish(g Git, t Task, p FinishPlan, o FinishOptions) (res FinishResult, err error) {
	if len(p.Blocked) > 0 {
		return res, errors.New(strings.Join(p.Blocked, "; "))
	}
	if len(p.Loss) > 0 && !o.Force {
		return res, fmt.Errorf("%w: %s", ErrLoss, strings.Join(p.Loss, ", "))
	}
	if len(t.Repos) != 1 {
		return res, fmt.Errorf("task %s has %d repositories; finishing handles one", t.Slug, len(t.Repos))
	}
	r := t.Repos[0]
	g = g.in(r.Repo)
	switch p.Mode {
	case ModeMerge:
		if len(p.Unmerged) > 0 {
			msg, err := MergeInto(g, p.base, r.Branch)
			if err != nil {
				return res, err
			}
			res.Done = append(res.Done, msg)
		}
	case ModePR:
		if o.OpenPR == nil {
			return res, errors.New("no pull request command")
		}
		remote := o.Remote
		if remote == "" {
			remote = "origin"
		}
		if err := g.Push(remote, r.Branch); err != nil {
			return res, err
		}
		res.Done = append(res.Done, fmt.Sprintf("pushed %s to %s", r.Branch, remote))
		if err := o.OpenPR(); err != nil {
			return res, fmt.Errorf("%s was pushed to %s, but the pull request was not opened: %w", r.Branch, remote, err)
		}
		res.Done = append(res.Done, "opened a pull request of "+r.Branch)
	case ModeAbandon:
	default:
		return res, fmt.Errorf("unknown finish mode %q", p.Mode)
	}
	allowed := map[string]bool{}
	for _, l := range p.Loss {
		allowed[l] = true
	}

	branchFree := true
	if p.switchTo != "" {
		cur, err := g.CurrentBranch()
		switch {
		case err != nil:
			branchFree = false
			res.Left = append(res.Left, fmt.Sprintf("%s stays on %s: %v", r.Repo, r.Branch, err))
		case cur == r.Branch:
			if changed, err := g.Changed(r.Repo); err != nil || len(changed) > 0 {
				branchFree = false
				res.Left = append(res.Left, fmt.Sprintf("%s stays on %s: it has uncommitted changes (%s)", r.Repo, r.Branch, strings.Join(changed, ", ")))
				break
			}
			if err := g.Switch(p.switchTo); err != nil {
				branchFree = false
				res.Left = append(res.Left, fmt.Sprintf("%s stays on %s: %v", r.Repo, r.Branch, err))
				break
			}
			res.Done = append(res.Done, fmt.Sprintf("switched %s to %s", r.Repo, p.switchTo))
		}
	}

	if r.Worktree != "" {
		lost, left, err := removeWorktree(g, r.Repo, r.Worktree, allowed, o.Force)
		res.Lost = append(res.Lost, lost...)
		switch {
		case err != nil:
			branchFree = false
			res.Left = append(res.Left, fmt.Sprintf("worktree %s remains: %v", r.Worktree, err))
		case left != "":
			branchFree = false
			res.Left = append(res.Left, fmt.Sprintf("worktree %s remains: %s", r.Worktree, left))
		default:
			res.Done = append(res.Done, "removed worktree "+r.Worktree)
		}
	}

	if !p.deleteBranch {
		switch p.Mode {
		case ModePR:
			res.Left = append(res.Left, fmt.Sprintf("branch %s (the pull request's head)", r.Branch))
		case ModeAbandon:
			res.Left = append(res.Left, fmt.Sprintf("branch %s: its commits beyond the base are on a remote (delete it with: git branch -D %s)", r.Branch, r.Branch))
		}
		return res, nil
	}
	if !branchFree {
		res.Left = append(res.Left, fmt.Sprintf("branch %s remains (still checked out)", r.Branch))
		return res, nil
	}
	lost, left, err := deleteBranch(g, p, r.Branch, allowed, o.Force)
	switch {
	case err != nil:
		res.Left = append(res.Left, fmt.Sprintf("branch %s remains: %v", r.Branch, err))
	case left != "":
		res.Left = append(res.Left, fmt.Sprintf("branch %s remains: %s", r.Branch, left))
	default:
		res.Lost = append(res.Lost, lost...)
		res.Done = append(res.Done, "deleted branch "+r.Branch)
	}
	return res, nil
}

// removeWorktree removes the worktree wt when what it would lose now is
// within allowed (the plan's Loss, which only force lets go). It returns
// what was lost, or why it was kept.
func removeWorktree(g Git, repo, wt string, allowed map[string]bool, force bool) (lost []string, left string, err error) {
	loss, err := WorktreeLoss(g, repo, wt)
	if err != nil {
		return nil, "", err
	}
	var extra []string
	for _, l := range loss {
		if !allowed[l] || !force {
			extra = append(extra, l)
		}
	}
	if len(extra) > 0 {
		return nil, "not shown before, kept: " + strings.Join(extra, ", "), nil
	}
	if len(loss) == 0 {
		removed, why, _, err := SafeRemove(g, repo, wt, false)
		if err != nil {
			return nil, "", err
		}
		if !removed {
			return nil, strings.Join(why, ", "), nil
		}
		return nil, "", nil
	}
	if err := checkPath(wt); err != nil {
		return nil, "", err
	}
	if _, err := g.in(repo).run("worktree", "remove", "--force", "--", wt); err != nil {
		return nil, "", err
	}
	return loss, "", nil
}

// deleteBranch deletes the task branch when it is merged into the base
// (merge), or when its commits are still the ones the plan knew and those
// that exist nowhere else were listed as Loss and forced (abandon).
func deleteBranch(g Git, p FinishPlan, branch string, allowed map[string]bool, force bool) (lost []string, left string, err error) {
	unmerged, err := g.Unmerged(branch, p.bases...)
	if err != nil {
		return nil, "", err
	}
	if p.Mode == ModeMerge {
		merged, err := g.IsAncestor("refs/heads/"+branch, "refs/heads/"+p.base)
		if err != nil {
			return nil, "", err
		}
		if !merged {
			return nil, fmt.Sprintf("not fully merged into %s", p.base), nil
		}
		return nil, "", g.DeleteBranch(branch)
	}
	known := map[string]bool{}
	for _, c := range p.Unmerged {
		known[c] = true
	}
	for _, c := range unmerged {
		if !known[c] {
			return nil, "it has a commit that was not shown before (" + c + ")", nil
		}
	}
	unpushed, err := g.Unpushed(g.Dir, branch, p.bases...)
	if err != nil {
		return nil, "", err
	}
	for _, c := range unpushed {
		if !allowed["commit "+c] || !force {
			return nil, "commit " + c + " exists nowhere else", nil
		}
		lost = append(lost, "commit "+c)
	}
	if err := g.DeleteBranch(branch); err != nil {
		return nil, "", err
	}
	return lost, "", nil
}

func (g Git) mergeInProgress() bool {
	_, err := g.run("rev-parse", "--quiet", "--verify", "MERGE_HEAD")
	return err == nil
}

// MergeInto merges branch into the local branch base. Where base is checked
// out (the main checkout or a worktree) the merge runs there (a
// fast-forward when possible), refusing tracked changes; a failed merge is
// aborted, so the checkout is as it was. Where it is not, the branch ref
// alone moves: fast-forwarded, or to a merge commit built without a
// checkout; a conflict changes nothing. Not undoable: callers confirm
// first. It returns what it did.
func MergeInto(g Git, base, branch string) (string, error) {
	if err := checkBranch(base); err != nil {
		return "", err
	}
	if err := checkBranch(branch); err != nil {
		return "", err
	}
	baseRef, branchRef := "refs/heads/"+base, "refs/heads/"+branch
	old, err := g.ResolveCommit(baseRef)
	if err != nil {
		return "", err
	}
	tip, err := g.ResolveCommit(branchRef)
	if err != nil {
		return "", err
	}
	if ok, err := g.IsAncestor(tip, old); err != nil {
		return "", err
	} else if ok {
		return fmt.Sprintf("%s is already merged into %s", branch, base), nil
	}
	at, err := g.CheckedOutAt(base)
	if err != nil {
		return "", err
	}
	if at != "" {
		return mergeInCheckout(g.in(at), base, branch, old, tip)
	}
	ff, err := g.IsAncestor(old, tip)
	if err != nil {
		return "", err
	}
	if ff {
		if _, err := g.run("update-ref", "-m", "cairn task finish: fast-forward", baseRef, tip, old); err != nil {
			return "", err
		}
		return fmt.Sprintf("fast-forwarded %s to %s", base, tip[:7]), nil
	}
	out, err := g.run("merge-tree", "--write-tree", "--name-only", "--no-messages", old, tip)
	ls := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil {
		if len(ls) > 1 && shaRe.MatchString(ls[0]) {
			return "", fmt.Errorf("merging %s into %s conflicts in %s; nothing was changed", branch, base, strings.Join(uniq(ls[1:]), ", "))
		}
		return "", err
	}
	if len(ls) == 0 || !shaRe.MatchString(ls[0]) {
		return "", fmt.Errorf("git merge-tree: unexpected output %q", out)
	}
	c, err := g.run("commit-tree", ls[0], "-p", old, "-p", tip, "-m", fmt.Sprintf("Merge branch '%s' into %s", branch, base))
	if err != nil {
		return "", err
	}
	merge := strings.TrimSpace(string(c))
	if !shaRe.MatchString(merge) {
		return "", fmt.Errorf("git commit-tree: unexpected output %q", c)
	}
	if _, err := g.run("update-ref", "-m", "cairn task finish: merge", baseRef, merge, old); err != nil {
		return "", err
	}
	return fmt.Sprintf("merged %s into %s (merge commit %s)", branch, base, merge[:7]), nil
}

func mergeInCheckout(g Git, base, branch, old, tip string) (string, error) {
	if changed, err := g.Changed(g.Dir); err != nil {
		return "", err
	} else if len(changed) > 0 {
		return "", fmt.Errorf("%s has uncommitted changes (%s); nothing was merged", g.Dir, strings.Join(changed, ", "))
	}
	if g.mergeInProgress() {
		return "", fmt.Errorf("%s has a merge in progress; nothing was merged", g.Dir)
	}
	if _, merr := g.run("merge", "--no-edit", "--", branch); merr != nil {
		conflicts, _ := lines(g.run("diff", "--name-only", "--diff-filter=U"))
		if g.mergeInProgress() {
			if _, err := g.run("merge", "--abort"); err != nil {
				return "", fmt.Errorf("merging %s into %s failed (%v) and could not be aborted (%v): see git status in %s", branch, base, merr, err, g.Dir)
			}
		}
		head, err := g.ResolveCommit("HEAD")
		if err != nil || head != old {
			return "", fmt.Errorf("merging %s into %s failed (%v); %s is no longer at %s: see git status there", branch, base, merr, g.Dir, old[:7])
		}
		if len(conflicts) > 0 {
			return "", fmt.Errorf("merging %s into %s conflicts in %s; the merge was aborted, nothing was changed", branch, base, strings.Join(conflicts, ", "))
		}
		return "", fmt.Errorf("merging %s into %s failed, nothing was changed: %w", branch, base, merr)
	}
	head, err := g.ResolveCommit("HEAD")
	if err != nil {
		return "", err
	}
	if head == tip {
		return fmt.Sprintf("fast-forwarded %s to %s in %s", base, tip[:7], g.Dir), nil
	}
	return fmt.Sprintf("merged %s into %s in %s (merge commit %s)", branch, base, g.Dir, head[:7]), nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
