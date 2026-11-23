package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// taskStatusEdit prepares rewriting the file of t (as it is now on disk)
// with t's fields.
func taskStatusEdit(d xdg.Dirs, t task.Task) (fileEdit, error) {
	path := task.Path(d, t.ID)
	before, err := wsRoot.ReadFile(path)
	if err != nil {
		return fileEdit{}, err
	}
	after, err := t.Encode()
	if err != nil {
		return fileEdit{}, err
	}
	return fileEdit{path: path, before: before, beforeExists: true, after: after, afterExists: true, perm: 0o644}, nil
}

// findTask loads the tasks and resolves ref.
func findTask(e *agentsEnv, w io.Writer, ref string) (task.Task, error) {
	ts, err := loadTasks(e.dirs, w)
	if err != nil {
		return task.Task{}, err
	}
	return task.Find(ts, ref)
}

// viewerPlaceholders may appear inside the viewer's arguments.
const viewerPlaceholders = "{base} {branch} {range} {repo} {worktree}"

func newTaskReviewCmd() *cobra.Command {
	var viewer string
	cmd := &cobra.Command{
		Use:   "review <id|slug>",
		Short: "Show the task's change in the diff viewer and mark the task as in review",
		Long: "Runs the viewer (--viewer, else tasks.diff_viewer; default \"git diff\") in the task's worktree\n" +
			"(or the main checkout). The command is split on spaces and run directly, never by a shell.\n" +
			"Arguments may contain " + viewerPlaceholders + "; each is replaced\n" +
			"inside its argument. Without placeholders, git gets the range <base>...<branch> appended, and\n" +
			"any other viewer (delta, less, ...) reads the diff on its input.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			st, err := loadTaskSettings(e.dirs)
			if err != nil {
				return err
			}
			t, err := findTask(e, cmd.ErrOrStderr(), args[0])
			if err != nil {
				return err
			}
			v := st.diffViewer
			if cmd.Flags().Changed("viewer") {
				v = viewer
			}
			argv := strings.Fields(v)
			if len(argv) == 0 || hasControlChars(v) {
				return fmt.Errorf("the diff viewer %q is not a command", v)
			}
			if err := showTaskDiff(cmd, t, argv); err != nil {
				return err
			}
			// The viewer may have been open for long: the task is read
			// again under the task lock, and only an open task becomes
			// review (a finish meanwhile is never undone).
			unlock, err := lockTasks(ctxOf(cmd), e.dirs, taskLockWait)
			if err != nil {
				return err
			}
			defer unlock()
			if t, err = findTask(e, io.Discard, t.ID); err != nil {
				return err
			}
			if t.Status != task.StatusOpen {
				return nil
			}
			t.Status = task.StatusReview
			ed, err := taskStatusEdit(e.dirs, t)
			if err != nil {
				return err
			}
			if err := commitEdits(e.dirs, "task.review", "review task "+t.Slug, []fileEdit{ed}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "task %s is in review; finish it with: cairn task finish %s --merge | --pr | --abandon\n", t.Slug, t.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&viewer, "viewer", "", "diff viewer command for this run (split on spaces, no shell)")
	return cmd
}

// showTaskDiff runs the viewer argv on the change of t since its base.
// Values from the task replace placeholders inside single arguments, so
// they are never split or interpreted; the program itself is never
// replaced.
func showTaskDiff(cmd *cobra.Command, t task.Task, argv []string) error {
	r := t.Repos[0]
	base := r.BaseCommit
	if base == "" {
		base = r.Base
	}
	if err := task.CheckRef(base); err != nil {
		return fmt.Errorf("task %s: base: %w", t.Slug, err)
	}
	dir := r.Repo
	if r.Worktree != "" {
		if _, err := os.Stat(r.Worktree); err == nil {
			dir = r.Worktree
		}
	}
	rng := base + "..." + r.Branch
	repl := strings.NewReplacer("{base}", base, "{branch}", r.Branch, "{range}", rng, "{repo}", r.Repo, "{worktree}", dir)
	used := false
	argv = append([]string(nil), argv...)
	for i := 1; i < len(argv); i++ {
		if a := repl.Replace(argv[i]); a != argv[i] {
			argv[i], used = a, true
		}
	}
	ctx := ctxOf(cmd)
	run := taskRunner()
	run.Dir, run.Stdin, run.Stdout, run.Stderr = dir, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	switch {
	case used:
	case argv[0] == "git":
		argv = append(argv, rng, "--")
	default:
		diff, err := newGit(ctx, r.Repo).Diff(base, r.Branch)
		if err != nil {
			return err
		}
		run.Stdin = bytes.NewReader(diff)
	}
	if _, err := run.Run(ctx, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("diff viewer %s: %w", quoteArgv(argv), err)
	}
	return nil
}

// finishRecord hands a task finished by merge or pull request over to the
// finish record (Task 7). It does nothing until that exists.
var finishRecord = func(cmd *cobra.Command, e *agentsEnv, t task.Task, p task.FinishPlan, res task.FinishResult) {}

func newTaskFinishCmd() *cobra.Command {
	var merge, pr, abandon, force, yes bool
	cmd := &cobra.Command{
		Use:   "finish <id|slug> --merge | --pr | --abandon",
		Short: "Finish a task: merge it, open a pull request, or abandon it",
		Long: "Shows every step first; nothing is ever implied, so exactly one mode is required.\n" +
			"  --merge    merge the branch into its base locally (not undoable), then remove the worktree\n" +
			"             and the merged branch.\n" +
			"  --pr       push the branch and open a pull request with tasks.pr_tool (gh or glab; not\n" +
			"             undoable), then remove the worktree; the branch stays.\n" +
			"  --abandon  remove the worktree, and the branch when it has no commits beyond its base.\n" +
			"The task's conversations are archived (cairn agents resume brings one back).\n\n" +
			"Nothing that holds work is removed: uncommitted, changed or ignored files in the worktree,\n" +
			"or commits that exist nowhere else, make finish refuse and list them. --force --yes lets\n" +
			"exactly the listed work go. A main checkout (--here task) is never cleaned: its uncommitted\n" +
			"changes block the finish even with --force. Steps marked \"not undoable\" need --yes or a\n" +
			"yes on a terminal.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, n := "", 0
			for _, m := range []struct {
				on   bool
				mode string
			}{{merge, task.ModeMerge}, {pr, task.ModePR}, {abandon, task.ModeAbandon}} {
				if m.on {
					mode = m.mode
					n++
				}
			}
			if n != 1 {
				return errors.New("choose one of --merge, --pr or --abandon: finishing a task is never automatic")
			}
			e, err := loadAgents()
			if err != nil {
				return err
			}
			st, err := loadTaskSettings(e.dirs)
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			unlock, err := lockTasks(ctx, e.dirs, taskLockWait)
			if err != nil {
				return err
			}
			defer unlock()
			t, err := findTask(e, cmd.ErrOrStderr(), args[0])
			if err != nil {
				return err
			}
			return finishTask(cmd, e, st, t, mode, force, yes)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&merge, "merge", false, "merge into the base branch locally (not undoable)")
	f.BoolVar(&pr, "pr", false, "push and open a pull request with tasks.pr_tool (not undoable)")
	f.BoolVar(&abandon, "abandon", false, "drop the task: remove its worktree, and its branch if it has no new commits")
	f.BoolVar(&force, "force", false, "let the listed work go (needs --yes)")
	f.BoolVar(&yes, "yes", false, "confirm the steps that are not undoable (and --force)")
	return cmd
}

func finishTask(cmd *cobra.Command, e *agentsEnv, st taskSettings, t task.Task, mode string, force, yes bool) error {
	ctx := ctxOf(cmd)
	out := cmd.OutOrStdout()
	finished := t.Status == task.StatusDone || t.Status == task.StatusAbandoned
	r := t.Repos[0]
	// No agent may run in the task while it is finished: every
	// conversation's lock is held until the end.
	cs, err := e.store.List(conv.Filter{Task: t.ID, Archived: true})
	if err != nil {
		return err
	}
	var rels []func()
	defer func() {
		for _, rel := range rels {
			rel()
		}
	}()
	for _, c := range cs {
		rel, err := e.notRunning(c)
		if err != nil {
			return fmt.Errorf("task %s: %w; nothing was changed", t.Slug, err)
		}
		rels = append(rels, rel)
	}

	g, cancel := finishGit(ctx, r.Repo)
	defer cancel()
	if finished {
		return cleanupFinished(cmd, e, g, t, force, yes)
	}
	intent, _ := readFinishIntent(e.dirs, t.ID)
	if mode == task.ModePR {
		if st.prTool == "" {
			return fmt.Errorf("no pull request tool is configured (set tasks.pr_tool to gh or glab); nothing was changed. To do it yourself:\n"+
				"  git -C %s push --set-upstream %s %s\n  then open a pull request of %s into %s,\n"+
				"  then: cairn task finish %s --abandon (a branch whose commits are pushed is kept)",
				r.Repo, st.prRemote, r.Branch, r.Branch, r.Base, t.Slug)
		}
		ok, err := g.HasRemote(st.prRemote)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s has no remote %q (tasks.pr_remote); nothing was changed", r.Repo, st.prRemote)
		}
	}
	p, err := task.PlanWith(g, t, mode, task.PlanOptions{FinishedTip: intent.Tip})
	if err != nil {
		return err
	}
	printFinishPlan(out, t, p, st)
	if len(p.Blocked) > 0 {
		return fmt.Errorf("cannot finish task %s: %s; nothing was changed", t.Slug, strings.Join(p.Blocked, "; "))
	}
	if len(p.Loss) > 0 {
		_, _ = fmt.Fprintln(out, "would be lost:")
		for _, l := range p.Loss {
			_, _ = fmt.Fprintf(out, "  %s\n", l)
		}
		if !force {
			_, _ = fmt.Fprintln(out, "commit (and push) what you want to keep, or pass --force --yes to lose exactly this")
			return fmt.Errorf("refused: finishing task %s would lose the work listed above; nothing was changed", t.Slug)
		}
		if !yes {
			return errors.New("--force needs --yes: it loses the work listed above; nothing was changed")
		}
	}
	if len(p.Irreversible) > 0 && !yes {
		in := cmd.InOrStdin()
		if !interactive(in) {
			return errors.New("the steps marked \"not undoable\" need confirmation: pass --yes; nothing was changed")
		}
		ok, err := ask(ctx, in, out, "Run these steps?")
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(out, "nothing was changed")
			return nil
		}
	}

	var opener func() error
	if mode == task.ModePR {
		opener = func() error { return openPullRequest(cmd, g, st, t) }
	}
	// Interrupted until now: nothing has changed. From here on the steps
	// run to their end (git gets SIGTERM only on the finish timeout).
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted; task %s was not finished, nothing was changed", t.Slug)
	}
	// The intent lets a rerun complete a finish cut off after its merge.
	if tip, err := g.ResolveCommit("refs/heads/" + r.Branch); err == nil {
		if err := writeFinishIntent(e.dirs, t.ID, finishIntent{Mode: mode, Tip: tip}); err != nil {
			return err
		}
	}
	res, ferr := task.Finish(g, t, p, task.FinishOptions{Force: force && yes, Remote: st.prRemote, OpenPR: opener})
	for _, d := range res.Done {
		_, _ = fmt.Fprintf(out, "done: %s\n", d)
	}
	for _, l := range res.Lost {
		_, _ = fmt.Fprintf(out, "lost (forced): %s\n", l)
	}
	for _, l := range res.Left {
		_, _ = fmt.Fprintf(out, "remains: %s\n", l)
	}
	if ferr != nil {
		if len(res.Done) == 0 {
			removeFinishIntent(e.dirs, t.ID)
		}
		return fmt.Errorf("task %s is not finished: %w", t.Slug, ferr)
	}

	e.archiveFinished(cmd, t, cs)
	t.Status = task.StatusDone
	if mode == task.ModeAbandon {
		t.Status = task.StatusAbandoned
	}
	ed, err := taskStatusEdit(e.dirs, t)
	if err != nil {
		return err
	}
	if err := commitEdits(e.dirs, "task.finish", fmt.Sprintf("finish task %s (%s)", t.Slug, mode), []fileEdit{ed}); err != nil {
		return err
	}
	removeFinishIntent(e.dirs, t.ID)
	_, _ = fmt.Fprintf(out, "task %s is %s\n", t.Slug, t.Status)
	if mode != task.ModeAbandon {
		finishRecord(cmd, e, t, p, res)
	}
	return nil
}

// printFinishPlan shows the steps before anything runs.
func printFinishPlan(w io.Writer, t task.Task, p task.FinishPlan, st taskSettings) {
	sty, styled := styleFor(w)
	_, _ = fmt.Fprintf(w, "finish task %s (%s), %s:\n", t.Slug, t.Title, p.Mode)
	for i, s := range p.Steps {
		mark := ""
		if s.Irreversible {
			mark = "  [not undoable]"
			if styled {
				mark = "  " + sty.Yellow("[not undoable]")
			}
		}
		_, _ = fmt.Fprintf(w, "  %d. %s%s\n", i+1, s.Text, mark)
	}
	if p.Mode == task.ModePR {
		_, _ = fmt.Fprintf(w, "  (remote %s, pull request with %s)\n", st.prRemote, st.prTool)
	}
	_, _ = fmt.Fprintln(w, "  then archive the task's conversations")
}

// prTimeout bounds gh or glab; finishGitTimeout bounds each git step of a
// finish. Both stop the command with SIGTERM.
var (
	prTimeout        = 2 * time.Minute
	finishGitTimeout = 10 * time.Minute
)

// finishGit runs git for a finish: not cancelled by Ctrl-C or SIGTERM once
// started (a merge is never killed halfway), bounded by finishGitTimeout
// per command, and stopped with SIGTERM so git cleans up its locks.
func finishGit(ctx context.Context, dir string) (task.Git, func()) {
	run := taskRunner()
	run.Interrupt = syscall.SIGTERM
	g := task.Git{R: timedRunner{run, finishGitTimeout}, Dir: dir, Ctx: context.WithoutCancel(ctx)}
	return g, func() {}
}

// timedRunner gives every command its own timeout.
type timedRunner struct {
	r execx.OSRunner
	d time.Duration
}

func (t timedRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	return t.r.Run(ctx, name, args...)
}

// prRepo is the repository of the remote's URL as gh and glab name it
// ("host/owner/repo"), or "" when the URL is not a host URL (a local path).
func prRepo(remoteURL string) string {
	host, path := "", ""
	switch {
	case strings.Contains(remoteURL, "://"):
		u, err := url.Parse(remoteURL)
		if err != nil {
			return ""
		}
		host, path = u.Hostname(), u.Path
	default:
		// scp-like: [user@]host:path
		hp, p, ok := strings.Cut(remoteURL, ":")
		if !ok || strings.Contains(hp, "/") {
			return ""
		}
		if _, h, ok := strings.Cut(hp, "@"); ok {
			hp = h
		}
		host, path = hp, p
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || !strings.Contains(path, "/") || !prRepoRe.MatchString(host+"/"+path) {
		return ""
	}
	return host + "/" + path
}

var prRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(/[A-Za-z0-9_.][A-Za-z0-9_.-]*)+$`)

// openPullRequest runs gh or glab in the main checkout with the task's
// title, pinned to the repository of tasks.pr_remote when its URL names
// one; every value is one argument (--flag=value), never shell text. A
// pull request that already exists for the branch counts as opened.
func openPullRequest(cmd *cobra.Command, g task.Git, st taskSettings, t task.Task) error {
	r := t.Repos[0]
	repo := ""
	if u, err := g.RemoteURL(st.prRemote); err == nil {
		repo = prRepo(u)
	}
	body := fmt.Sprintf("Task: %s\nBranch: %s", t.Title, r.Branch)
	var create, view []string
	switch st.prTool {
	case "gh":
		create = []string{"pr", "create", "--base=" + r.Base, "--head=" + r.Branch, "--title=" + t.Title, "--body=" + body}
		view = []string{"pr", "view", "--json=url", "--jq=.url"}
	case "glab":
		create = []string{"mr", "create", "--source-branch=" + r.Branch, "--target-branch=" + r.Base,
			"--title=" + t.Title, "--description=" + body, "--yes"}
		view = []string{"mr", "view", "--output=json"}
		if repo != "" {
			repo = "https://" + repo
		}
	default:
		return fmt.Errorf("unknown pull request tool %q", st.prTool)
	}
	if repo != "" {
		create = append(create, "--repo="+repo)
		view = append(view, "--repo="+repo)
	}
	view = append(view, "--", r.Branch)
	out := cmd.OutOrStdout()
	var got bytes.Buffer
	run := taskRunner()
	run.Interrupt = syscall.SIGTERM
	run.Dir, run.Stdout, run.Stderr = r.Repo, io.MultiWriter(out, &got), io.MultiWriter(cmd.ErrOrStderr(), &got)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctxOf(cmd)), prTimeout)
	defer cancel()
	_, err := run.Run(ctx, st.prTool, create...)
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s %s create did not finish within %s; check whether the pull request exists, then run finish --pr again", st.prTool, create[0], prTimeout)
	case err == nil:
		return nil
	case !strings.Contains(strings.ToLower(got.String()), "already exists"):
		return fmt.Errorf("%s %s create: %w", st.prTool, create[0], err)
	}
	// It exists (an earlier run, or by hand): show it.
	vrun := taskRunner()
	vrun.Dir = r.Repo
	vctx, vcancel := context.WithTimeout(context.WithoutCancel(ctxOf(cmd)), prTimeout)
	defer vcancel()
	b, verr := vrun.Run(vctx, st.prTool, view...)
	link := strings.TrimSpace(string(b))
	if st.prTool == "glab" {
		var mr struct {
			WebURL string `json:"web_url"`
		}
		if json.Unmarshal(b, &mr) == nil {
			link = mr.WebURL
		}
	}
	if verr != nil || link == "" {
		_, _ = fmt.Fprintf(out, "a pull request of %s already exists (its address could not be read)\n", r.Branch)
		return nil
	}
	_, _ = fmt.Fprintf(out, "a pull request of %s already exists: %s\n", r.Branch, link)
	return nil
}

// archiveFinished archives the conversations of a finished task and removes
// their panes (the task window goes with the last one).
func (e *agentsEnv) archiveFinished(cmd *cobra.Command, t task.Task, cs []conv.Conversation) {
	out := cmd.OutOrStdout()
	now := time.Now().UTC()
	n := 0
	for _, c := range cs {
		if c.ArchivedAt != nil {
			continue
		}
		err := e.unbind(cmd, c, func(cu *conv.Curated) { cu.ArchivedAt = &now }, "agents.archive",
			fmt.Sprintf("archive agent %s (task %s finished)", convTitle(c), t.Slug),
			func() (wsDriver, error) { return newDriver(socketName(cmd)) })
		if err != nil {
			_, _ = fmt.Fprintf(out, "note: %s was not archived: %v\n", convTitle(c), err)
			continue
		}
		n++
	}
	if n > 0 {
		_, _ = fmt.Fprintf(out, "archived %d conversation(s) of the task (cairn agents resume brings one back)\n", n)
	}
}

// finishIntent is written before a finish's first irreversible step and
// removed with its status write: a finish cut off in between is reported,
// and its rerun knows the branch tip that was merged.
type finishIntent struct {
	Mode string `json:"mode"`
	Tip  string `json:"tip"`
}

func finishIntentDir(d xdg.Dirs) string { return filepath.Join(d.State, "agents", "tasks-finishing") }

func writeFinishIntent(d xdg.Dirs, id string, fi finishIntent) error {
	if !conv.ValidID(id) {
		return fmt.Errorf("task id %q", id)
	}
	b, err := json.Marshal(fi)
	if err != nil {
		return err
	}
	if err := wsRoot.MkdirAll(finishIntentDir(d), 0o700); err != nil {
		return err
	}
	return wsRoot.WriteAtomic(filepath.Join(finishIntentDir(d), id+".json"), b, 0o600)
}

func readFinishIntent(d xdg.Dirs, id string) (finishIntent, bool) {
	var fi finishIntent
	if !conv.ValidID(id) {
		return fi, false
	}
	b, err := wsRoot.ReadFile(filepath.Join(finishIntentDir(d), id+".json"))
	if err != nil || json.Unmarshal(b, &fi) != nil {
		return finishIntent{}, false
	}
	switch fi.Mode {
	case task.ModeMerge, task.ModePR, task.ModeAbandon:
	default:
		return finishIntent{}, false
	}
	if task.CheckRef(fi.Tip) != nil {
		fi.Tip = ""
	}
	return fi, true
}

func removeFinishIntent(d xdg.Dirs, id string) {
	if conv.ValidID(id) {
		_ = wsRoot.Remove(filepath.Join(finishIntentDir(d), id+".json"))
	}
}

// reportFinishing tells about finishes that were cut off and about
// finished tasks whose worktree is left; recover also prints the fix.
func (e *agentsEnv) reportFinishing(w io.Writer) int {
	ts, _, err := task.Load(wsRoot, e.dirs)
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range ts {
		if fi, ok := readFinishIntent(e.dirs, t.ID); ok && t.Status != task.StatusDone && t.Status != task.StatusAbandoned {
			_, _ = fmt.Fprintf(w, "the finish of task %s was interrupted; complete it with: cairn task finish %s --%s\n", t.Slug, t.Slug, fi.Mode)
			n++
			continue
		}
		if (t.Status == task.StatusDone || t.Status == task.StatusAbandoned) && t.Repos[0].Worktree != "" {
			if _, err := os.Lstat(t.Repos[0].Worktree); err == nil {
				_, _ = fmt.Fprintf(w, "task %s is %s but its worktree %s remains; remove it with: cairn task finish %s --abandon\n",
					t.Slug, t.Status, t.Repos[0].Worktree, t.Slug)
				n++
			}
		}
	}
	return n
}

// cleanupFinished removes what a finished task left (its worktree), under
// the same loss rules; the status and the branch stay.
func cleanupFinished(cmd *cobra.Command, e *agentsEnv, g task.Git, t task.Task, force, yes bool) error {
	out := cmd.OutOrStdout()
	removeFinishIntent(e.dirs, t.ID)
	p, err := task.Plan(g, t, task.ModeCleanup)
	if err != nil {
		return err
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("task %s is already %s and nothing of it remains", t.Slug, t.Status)
	}
	_, _ = fmt.Fprintf(out, "task %s is already %s; what remains:\n", t.Slug, t.Status)
	for i, s := range p.Steps {
		_, _ = fmt.Fprintf(out, "  %d. %s\n", i+1, s.Text)
	}
	if len(p.Loss) > 0 {
		_, _ = fmt.Fprintln(out, "would be lost:")
		for _, l := range p.Loss {
			_, _ = fmt.Fprintf(out, "  %s\n", l)
		}
		if !force {
			return fmt.Errorf("refused: removing the worktree of task %s would lose the work listed above; nothing was changed (--force --yes loses exactly this)", t.Slug)
		}
		if !yes {
			return errors.New("--force needs --yes: it loses the work listed above; nothing was changed")
		}
	}
	res, err := task.Finish(g, t, p, task.FinishOptions{Force: force && yes})
	for _, d := range res.Done {
		_, _ = fmt.Fprintf(out, "done: %s\n", d)
	}
	for _, l := range res.Lost {
		_, _ = fmt.Fprintf(out, "lost (forced): %s\n", l)
	}
	for _, l := range res.Left {
		_, _ = fmt.Fprintf(out, "remains: %s\n", l)
	}
	return err
}
