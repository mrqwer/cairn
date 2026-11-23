package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

// defaultTaskTool is the tool of task new without --tool.
const defaultTaskTool = "claude"

// taskRunner runs git and setup commands: the environment without the
// variables that would point git at another repository than the one asked
// for.
func taskRunner() execx.OSRunner {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	return execx.OSRunner{Env: env}
}

// newGit runs git in dir; ctx is kept from cancellation so that cleanup
// after an interrupt still runs.
func newGit(ctx context.Context, dir string) task.Git {
	return task.Git{R: taskRunner(), Dir: dir, Ctx: context.WithoutCancel(ctx)}
}

// lockTasks serialises the start of task creations (recovery, choosing
// the slug, creating the branch and worktree) across processes, so two
// creations never pick the same branch and a crashed one is cleaned up
// before its slug can be picked again. It waits up to wait.
func lockTasks(ctx context.Context, d xdg.Dirs, wait time.Duration) (func(), error) {
	dir := taskJournal(d).Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) || ctx.Err() != nil {
			_ = f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, errors.New("another cairn task new is creating a task; try again")
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() })
	}, nil
}

// taskLockWait bounds the wait for another task creation's first steps.
var taskLockWait = 30 * time.Second

func taskJournal(d xdg.Dirs) task.Journal {
	return task.Journal{Dir: filepath.Join(d.State, "agents", "tasks-pending")}
}

func trustStore(d xdg.Dirs) task.TrustStore {
	return task.TrustStore{Root: wsRoot, Path: filepath.Join(d.State, "agents", "trust.json")}
}

// taskSettings is the validated tasks/agents configuration.
type taskSettings struct {
	isolation, prefix, worktreeRoot string
}

func loadTaskSettings(d xdg.Dirs) (taskSettings, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return taskSettings{}, err
	}
	var probs []string
	for _, fe := range cfg.Validate() {
		if strings.HasPrefix(fe.Key, "tasks.") {
			probs = append(probs, fe.Error())
		}
	}
	if len(probs) > 0 {
		return taskSettings{}, fmt.Errorf("invalid configuration: %s", strings.Join(probs, "; "))
	}
	root := cfg.Tasks.WorktreeRoot
	switch {
	case root == "":
		root = filepath.Join(d.State, "worktrees")
	case strings.HasPrefix(root, "~/"):
		root = filepath.Join(d.Home, root[2:])
	}
	return taskSettings{isolation: cfg.Tasks.DefaultIsolation, prefix: cfg.Tasks.BranchPrefix, worktreeRoot: root}, nil
}

// agentQueue is the concurrency queue with agents.max_running from the
// configuration of dirs; a configuration that does not load keeps the
// default (and says so on w).
func agentQueue(d xdg.Dirs, s conv.Store, w io.Writer) task.Queue {
	max := 4
	cfg, _, err := config.Load(fsx.Root{Dir: "/"}, d, os.Getenv)
	switch {
	case err != nil:
		if w != nil {
			_, _ = fmt.Fprintf(w, "note: %v; at most %d agents run at once\n", err, max)
		}
	case cfg.Agents.MaxRunning >= 0:
		max = cfg.Agents.MaxRunning
	}
	return agents.NewQueue(s, max)
}

// admitNew records c as queued when no slot is free and says so.
func (e *agentsEnv) admitNew(out, errOut io.Writer, c conv.Conversation) {
	q := agentQueue(e.dirs, e.store, errOut)
	queued, pos, err := agents.AdmitNew(e.store, q, c.ID, time.Now().UTC())
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(errOut, "note: the agent queue could not be checked: %v\n", err)
	case queued:
		_, _ = fmt.Fprintf(out, "queued: position %d (at most %d agents run at once); %s starts when one finishes\n", pos, q.Max, convTitle(c))
	}
}

// loadTasks reads the task files; a broken one is reported on w.
func loadTasks(d xdg.Dirs, w io.Writer) ([]task.Task, error) {
	ts, lerrs, err := task.Load(wsRoot, d)
	for _, le := range lerrs {
		_, _ = fmt.Fprintf(w, "skipped %s\n", le.Error())
	}
	return ts, err
}

// windowFor is the window an agent pane of c goes to: its task's window,
// else the agents window.
func (e *agentsEnv) windowFor(c conv.Conversation) (key, name string) {
	if c.Task != "" {
		if ts, _, err := task.Load(wsRoot, e.dirs); err == nil {
			for _, t := range ts {
				if t.ID == c.Task {
					return t.Window(), t.Slug
				}
			}
		}
	}
	return workspace.AgentWindow, workspace.AgentWindow
}

// recoverIdle runs recoverTasks when no task creation is starting (it
// never waits for one).
func (e *agentsEnv) recoverIdle(ctx context.Context, w io.Writer) {
	unlock, err := lockTasks(ctx, e.dirs, 0)
	if err != nil {
		return
	}
	defer unlock()
	e.recoverTasks(ctx, w)
}

// recoverTasks undoes the git changes of task creations whose process died
// before writing the task file (see task.Journal) and reports them. The
// caller holds lockTasks.
func (e *agentsEnv) recoverTasks(ctx context.Context, w io.Writer) {
	rs, err := taskJournal(e.dirs).Recover(func(t task.Task) bool {
		return wsRoot.Exists(task.Path(e.dirs, t.ID))
	}, func(t task.Task) []string {
		e.dropTaskConvs(t.ID)
		// Never touch what a recorded task uses (the same branch picked
		// again after the crash).
		if ts, _, err := task.Load(wsRoot, e.dirs); err == nil {
			for _, o := range ts {
				for _, or := range o.Repos {
					for _, r := range t.Repos {
						if or.Repo == r.Repo && (or.Branch == r.Branch || (r.Worktree != "" && or.Worktree == r.Worktree)) {
							return []string{fmt.Sprintf("branch %s: in use by task %s; left alone", r.Branch, o.Slug)}
						}
					}
				}
			}
		}
		return task.Compensate(newGit(ctx, t.Repos[0].Repo), t)
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "note: interrupted task creations could not be checked: %v\n", err)
	}
	for _, r := range rs {
		if r.Committed {
			continue
		}
		_, _ = fmt.Fprintf(w, "cleaned up an interrupted task new %q (%s)\n", r.Task.Title, r.Task.Slug)
		for _, l := range r.Left {
			_, _ = fmt.Fprintf(w, "  remains: %s\n", l)
		}
	}
}

// dropTaskConvs hides the conversations recorded for a task whose creation
// failed (their records stay, as for every removal).
func (e *agentsEnv) dropTaskConvs(id string) {
	cs, err := e.store.List(conv.Filter{Task: id, Archived: true})
	if err != nil {
		return
	}
	for _, c := range cs {
		cu := conv.CuratedOf(c)
		cu.Removed = true
		_ = e.store.WriteCurated(c.ID, cu)
	}
}

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Tasks: a branch (and worktree) with its agent window",
	}
	cmd.AddCommand(newTaskNewCmd(), newTaskLsCmd(), newTaskOpenCmd())
	return cmd
}

// chooseIsolation resolves the isolation from the flags and the config;
// "ask" asks on a terminal (Enter keeps the last choice) and fails on a pipe.
func chooseIsolation(cmd *cobra.Command, d xdg.Dirs, worktree, here bool, def string) (string, error) {
	switch {
	case worktree && here:
		return "", errors.New("--worktree and --here exclude each other")
	case worktree:
		return task.IsolationWorktree, nil
	case here:
		return task.IsolationHere, nil
	case def != task.IsolationAsk:
		return def, nil
	}
	in := cmd.InOrStdin()
	if !interactive(in) {
		return "", errors.New("tasks.default_isolation is \"ask\" and the input is not a terminal: pass --worktree or --here (or set tasks.default_isolation)")
	}
	last := filepath.Join(d.State, "agents", "task-isolation")
	prev := task.IsolationWorktree
	if b, err := wsRoot.ReadFile(last); err == nil && strings.TrimSpace(string(b)) == task.IsolationHere {
		prev = task.IsolationHere
	}
	out := cmd.OutOrStdout()
	for {
		_, _ = fmt.Fprintf(out, "Work in [w] a new worktree or [h] here, in the main checkout? (Enter: %s) ", prev)
		line, err := readLine(ctxOf(cmd), in)
		if err != nil {
			return "", err
		}
		choice := ""
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			choice = prev
		case "w", "worktree":
			choice = task.IsolationWorktree
		case "h", "here":
			choice = task.IsolationHere
		}
		if choice != "" {
			_ = wsRoot.MkdirAll(filepath.Dir(last), 0o700)
			_ = wsRoot.WriteAtomic(last, []byte(choice+"\n"), 0o600)
			return choice, nil
		}
	}
}

// readLine reads one line from in; Ctrl-C while waiting is ExitError 130.
func readLine(ctx context.Context, in io.Reader) (string, error) {
	prompting.Store(true)
	defer prompting.Store(false)
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		lines <- line
	}()
	select {
	case <-ctx.Done():
		return "", &ExitError{Code: exitInterrupted}
	case line := <-lines:
		return line, nil
	}
}

// quoteArgv renders argv for display, quoting what a shell would split.
func quoteArgv(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsFunc(a, func(r rune) bool { return r <= ' ' || strings.ContainsRune(`'"\$;&|<>()*?[]#~!{}`+"`", r) }) {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func newTaskNewCmd() *cobra.Command {
	var project, tool, from string
	var worktree, here, approveSetup bool
	cmd := &cobra.Command{
		Use:   "new <title>",
		Short: "Start a task: a branch, optionally a worktree, and a window with an agent",
		Long: "Creates branch <tasks.branch_prefix>/<slug> from --from (default: what the project's checkout\n" +
			"has checked out) and, with --worktree, a worktree under tasks.worktree_root with the files of\n" +
			".worktreeinclude copied in; with --here the main checkout switches to the branch. The setup\n" +
			"command of the repo's .cairn.toml runs once you approved it (asked on a terminal, or\n" +
			"--approve-setup; remembered per project until the file changes). Then the task is recorded\n" +
			"and gets a window with an agent pane.\n\n" +
			"If anything fails, what was created is removed again (a worktree only when clean, the branch\n" +
			"only without new commits) and what remains is reported. cairn undo does the same later.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			title := strings.TrimSpace(args[0])
			if title == "" || hasControlChars(title) {
				return errors.New("the title must not be empty or contain control characters")
			}
			e, err := loadAgents()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			a, ok := e.adapter(tool)
			if !ok {
				return fmt.Errorf("unknown tool %q (see: cairn agents doctor)", tool)
			}
			st, err := loadTaskSettings(e.dirs)
			if err != nil {
				return err
			}
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			p, err := projectFor(ws, project)
			if err != nil {
				return err
			}
			if from != "" {
				if err := task.CheckRef(from); err != nil {
					return fmt.Errorf("--from: %w", err)
				}
			}
			iso, err := chooseIsolation(cmd, e.dirs, worktree, here, st.isolation)
			if err != nil {
				return err
			}
			unlock, err := lockTasks(ctx, e.dirs, taskLockWait)
			if err != nil {
				return err
			}
			defer unlock()
			e.recoverTasks(ctx, errOut)
			t, err := planTask(ctx, e, st, p, title, iso, from)
			if err != nil {
				return err
			}
			return createTask(cmd, e, a.Name, a.Capabilities.PreassignID, t, approveSetup, out, unlock)
		},
	}
	f := cmd.Flags()
	f.StringVar(&project, "project", "", "project (default: the one containing the current directory)")
	f.StringVar(&tool, "tool", defaultTaskTool, "agent tool")
	f.StringVar(&from, "from", "", "start the branch here (default: the checkout's current branch)")
	f.BoolVar(&worktree, "worktree", false, "work in a new git worktree")
	f.BoolVar(&here, "here", false, "work in the project's main checkout")
	f.BoolVar(&approveSetup, "approve-setup", false, "approve the repo's setup command after showing it")
	return cmd
}

// projectFor is project id, else the project containing the working
// directory.
func projectFor(ws *wsState, id string) (workspace.Project, error) {
	if id != "" {
		return ws.requireProject(id)
	}
	wd, err := os.Getwd()
	if err != nil {
		return workspace.Project{}, err
	}
	p, ok := projectOfDir(ws, wd)
	if !ok {
		return workspace.Project{}, fmt.Errorf("no project contains %s; pass --project", wd)
	}
	return p, nil
}

// planTask decides everything about the new task without changing
// anything: repository, base, unique slug and branch, worktree path.
func planTask(ctx context.Context, e *agentsEnv, st taskSettings, p workspace.Project, title, iso, from string) (task.Task, error) {
	g := newGit(ctx, p.Root)
	repo, err := g.Root()
	if err != nil {
		return task.Task{}, fmt.Errorf("project %s: %s is not in a git repository: %w", p.ID, p.Root, err)
	}
	repo = filepath.Clean(repo)
	g.Dir = repo
	base := from
	if base == "" {
		if base, err = g.CurrentBranch(); err != nil {
			return task.Task{}, err
		}
		if base == "" {
			base = "HEAD"
		}
	}
	baseCommit, err := g.ResolveCommit(base)
	if err != nil {
		return task.Task{}, err
	}
	if base == "HEAD" {
		base = baseCommit // a detached checkout: name the commit
	}
	if iso == task.IsolationHere {
		head, err := g.ResolveCommit("HEAD")
		if err != nil {
			return task.Task{}, err
		}
		if head != baseCommit {
			dirty, err := g.Dirty(repo)
			if err != nil {
				return task.Task{}, err
			}
			if len(dirty) > 0 {
				return task.Task{}, fmt.Errorf("%s has uncommitted changes (%s); --here from another commit would carry them over: commit or stash them, or use --worktree",
					repo, strings.Join(dirty, ", "))
			}
		}
	}
	existing, err := loadTasks(e.dirs, io.Discard)
	if err != nil {
		return task.Task{}, err
	}
	wtDir := filepath.Join(st.worktreeRoot, p.ID)
	var probe error
	slug, branch := task.UniqueBranch(st.prefix, task.Slug(title), func(b, s string) bool {
		for _, t := range existing {
			if t.Project == p.ID && t.Slug == s {
				return true
			}
		}
		if iso == task.IsolationWorktree {
			if _, err := os.Lstat(filepath.Join(wtDir, s)); !errors.Is(err, fs.ErrNotExist) {
				return true
			}
		}
		ok, err := g.BranchExists(b)
		if err != nil {
			probe = err
			return false // stop: the error is returned below
		}
		return ok
	})
	if probe != nil {
		return task.Task{}, probe
	}
	r := task.Repo{Repo: repo, Base: base, BaseCommit: baseCommit, Branch: branch}
	if iso == task.IsolationWorktree {
		r.Worktree = filepath.Join(wtDir, slug)
	}
	now := time.Now().UTC()
	t := task.Task{ID: conv.NewID(now, nil), Title: title, Slug: slug, Project: p.ID, Repos: []task.Repo{r},
		Isolation: iso, Status: task.StatusOpen, CreatedAt: now.Truncate(time.Second)}
	if err := t.Validate(); err != nil {
		return task.Task{}, err
	}
	return t, nil
}

// createTask makes the planned task: branch, worktree, include files,
// setup, conversation, then the task file and the window binding as one
// history event. A failure before that event compensates every git change
// (journalled, so a crash is cleaned up by the next task command).
func createTask(cmd *cobra.Command, e *agentsEnv, tool string, preassign bool, t task.Task, approveSetup bool, out io.Writer, unlock func()) (err error) {
	ctx := ctxOf(cmd)
	r := t.Repos[0]
	g := newGit(ctx, r.Repo)
	end, err := taskJournal(e.dirs).Begin(t)
	if err != nil {
		return err
	}
	// made is what this run created, and all a failure undoes: never a
	// branch or worktree that was there before.
	var made *task.Repo
	var convID string
	defer func() {
		if err == nil {
			end()
			return
		}
		if convID != "" {
			e.dropTaskConvs(t.ID)
		}
		var left []string
		if made != nil {
			left = task.Compensate(g, task.Task{Isolation: t.Isolation, Repos: []task.Repo{*made}})
		}
		end()
		_, _ = fmt.Fprintf(out, "task new failed; cleaned up the branch and worktree it made\n")
		for _, l := range left {
			_, _ = fmt.Fprintf(out, "  remains: %s\n", l)
		}
	}()
	step := func() error {
		if ctx.Err() != nil {
			return errors.New("interrupted")
		}
		return nil
	}

	workdir := r.Repo
	if t.Isolation == task.IsolationWorktree {
		workdir = r.Worktree
		if err := os.MkdirAll(filepath.Dir(r.Worktree), 0o755); err != nil {
			return err
		}
		if err := g.CreateBranch(r.Branch, r.BaseCommit); err != nil {
			return err
		}
		made = &task.Repo{Repo: r.Repo, Base: r.Base, BaseCommit: r.BaseCommit, Branch: r.Branch}
		if err := g.AddWorktree(r.Worktree, r.Branch); err != nil {
			return err
		}
		made.Worktree = r.Worktree
		unlock()
		if err := step(); err != nil {
			return err
		}
		copied, err := task.CopyInclude(r.Repo, r.Worktree)
		if err != nil {
			return err
		}
		if len(copied) > 0 {
			_, _ = fmt.Fprintf(out, "copied into the worktree: %s\n", strings.Join(copied, ", "))
		}
	} else {
		if err := g.SwitchNew(r.Branch, r.BaseCommit); err != nil {
			return err
		}
		made = &r
		unlock()
	}
	if err := step(); err != nil {
		return err
	}

	repoFile := filepath.Join(workdir, task.RepoFile)
	in := cmd.InOrStdin()
	notApproved := false
	err = task.Setup(ctx, taskRunner(), trustStore(e.dirs), t.Project, repoFile, func(argv []string) bool {
		_, _ = fmt.Fprintf(out, "the setup command of %s (from %s, not yet approved):\n  %s\n", t.Project, repoFile, quoteArgv(argv))
		switch {
		case approveSetup:
			return true
		case interactive(in):
			ok, _ := ask(ctx, in, out, "Run it now and whenever this file is unchanged?")
			return ok
		}
		notApproved = true
		return false
	})
	switch {
	case errors.Is(err, task.ErrSetupNotApproved):
		if notApproved {
			_, _ = fmt.Fprintln(out, "not run: approve it with --approve-setup, or run task new on a terminal")
		} else {
			_, _ = fmt.Fprintln(out, "setup not run")
		}
		err = nil
	case err != nil:
		return err
	}
	if err := step(); err != nil {
		return err
	}

	now := time.Now().UTC()
	c := conv.Conversation{ID: conv.NewID(now, nil), Tool: tool, Name: t.Slug, Purpose: t.Title, Project: t.Project, Task: t.ID,
		Cwd: workdir, Worktree: r.Worktree, Branch: r.Branch, State: conv.StateStarting, CreatedAt: now, LastActivityAt: now}
	if preassign {
		c.NativeID = agents.NewNativeID()
	}
	if err := e.store.Put(c); err != nil {
		return err
	}
	convID = c.ID
	if err := e.store.SeedCurated(c.ID); err != nil {
		return err
	}
	data, err := t.Encode()
	if err != nil {
		return err
	}
	tf := fileEdit{path: task.Path(e.dirs, t.ID), after: data, afterExists: true, perm: 0o644}
	key, err := e.bindIn(cmd, c, t.Window(), t.Slug, []fileEdit{tf}, "task.new",
		fmt.Sprintf("new task %s in %s (%s)", t.Slug, t.Project, r.Branch))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "created task %s (%s) in %s: branch %s", t.Slug, t.ID, t.Project, r.Branch)
	if r.Worktree != "" {
		_, _ = fmt.Fprintf(out, ", worktree %s", r.Worktree)
	}
	_, _ = fmt.Fprintf(out, "\nagent %s (%s) in window %s, pane %s\n", c.Name, tool, t.Window(), key)
	e.admitNew(out, cmd.ErrOrStderr(), c)
	return openTaskWindow(cmd, e, t, c, key, false)
}

// openTaskWindow applies the project and, when the user is at a terminal
// or inside tmux, jumps to the agent pane (starting its tool). Errors here
// leave the task as it is: cairn task open retries.
func openTaskWindow(cmd *cobra.Command, e *agentsEnv, t task.Task, c conv.Conversation, key string, always bool) error {
	out := cmd.OutOrStdout()
	running, err := applyProject(cmd, t.Project, always)
	if err != nil {
		return err
	}
	if !running {
		_, _ = fmt.Fprintf(out, "the session of %s is not running; open the task with: cairn task open %s\n", t.Project, t.Slug)
		return nil
	}
	if !always && tmuxClientSocket() == "" && !interactive(cmd.InOrStdin()) {
		_, _ = fmt.Fprintf(out, "open it with: cairn task open %s\n", t.Slug)
		return nil
	}
	if rel, err := e.notRunning(c); err == nil {
		rel()
		if err := agents.RequestStart(e.store, c.ID); err != nil {
			return err
		}
	}
	return jumpToPane(cmd, t.Project, key)
}

// taskRow is one task in task ls --json.
type taskRow struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Slug          string    `json:"slug"`
	Project       string    `json:"project"`
	Status        string    `json:"status"`
	Isolation     string    `json:"isolation"`
	Repo          string    `json:"repo"`
	Branch        string    `json:"branch"`
	Worktree      string    `json:"worktree"`
	Conversations int       `json:"conversations"`
	CreatedAt     time.Time `json:"created_at"`
}

func newTaskLsCmd() *cobra.Command {
	var project, status string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			e.recoverIdle(ctxOf(cmd), cmd.ErrOrStderr())
			ts, err := loadTasks(e.dirs, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			cs, err := e.store.List(conv.Filter{})
			if err != nil {
				return err
			}
			n := map[string]int{}
			for _, c := range cs {
				n[c.Task]++
			}
			rows := []taskRow{}
			for _, t := range ts {
				if (project != "" && t.Project != project) || (status != "" && t.Status != status) {
					continue
				}
				r := t.Repos[0]
				rows = append(rows, taskRow{ID: t.ID, Title: t.Title, Slug: t.Slug, Project: t.Project, Status: t.Status,
					Isolation: t.Isolation, Repo: r.Repo, Branch: r.Branch, Worktree: r.Worktree, Conversations: n[t.ID], CreatedAt: t.CreatedAt})
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, rows)
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no tasks")
				return nil
			}
			sty, styled := styleFor(out)
			head := fmt.Sprintf("%s %s %s %s %s %s", ui.Pad("ID", 26), ui.Pad("SLUG", 24), ui.Pad("PROJECT", 12),
				ui.Pad("STATUS", 9), ui.Pad("BRANCH", 30), "TITLE")
			if styled {
				head = sty.Dim(head)
			}
			_, _ = fmt.Fprintln(out, head)
			for _, r := range rows {
				_, _ = fmt.Fprintf(out, "%s %s %s %s %s %s\n", ui.Pad(r.ID, 26), ui.Pad(r.Slug, 24), ui.Pad(r.Project, 12),
					ui.Pad(r.Status, 9), ui.Pad(r.Branch, 30), r.Title)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&project, "project", "", "only this project")
	f.StringVar(&status, "status", "", "only this status (open, review, done, abandoned)")
	f.BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func newTaskOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <id|slug>",
		Short: "Jump to a task's window, opening it (and its agent pane) if needed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			e.recoverIdle(ctxOf(cmd), cmd.ErrOrStderr())
			ts, err := loadTasks(e.dirs, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			t, err := task.Find(ts, args[0])
			if err != nil {
				return err
			}
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			if _, err := ws.requireProject(t.Project); err != nil {
				return err
			}
			cs, err := e.store.List(conv.Filter{Task: t.ID, Archived: true})
			if err != nil {
				return err
			}
			if len(cs) == 0 {
				return fmt.Errorf("task %s has no conversation; start one with: cairn agents new <tool> --task %s", t.Slug, t.Slug)
			}
			// A bound conversation of the task, else the latest one is
			// bound to the task's window again (unarchived).
			var c conv.Conversation
			key, bound := "", false
			for _, x := range cs {
				if _, k, ok := bindingOf(ws, x.ID); ok && (!bound || x.LastActivityAt.After(c.LastActivityAt)) {
					c, key, bound = x, k, true
				}
			}
			if !bound {
				c = cs[0]
				for _, x := range cs[1:] {
					if x.LastActivityAt.After(c.LastActivityAt) {
						c = x
					}
				}
				var extra []fileEdit
				if c.ArchivedAt != nil {
					ce, err := e.curatedEdit(c.ID, func(cu *conv.Curated) { cu.ArchivedAt = nil })
					if err != nil {
						return err
					}
					extra = append(extra, ce)
				}
				if key, err = e.bindIn(cmd, c, t.Window(), t.Slug, extra, "task.open", "open task "+t.Slug); err != nil {
					return err
				}
				e.admitNew(cmd.OutOrStdout(), cmd.ErrOrStderr(), c)
			}
			return openTaskWindow(cmd, e, t, c, key, true)
		},
	}
}

// afterTaskUndo compensates the git side of an undone task new: each
// worktree is removed if clean and no agent runs in it, each branch
// deleted if it has no new commits; what remains is reported.
func afterTaskUndo(cmd *cobra.Command, ev history.Event) {
	out := cmd.OutOrStdout()
	e, err := loadAgents()
	if err != nil {
		_, _ = fmt.Fprintf(out, "the task's branch and worktree were not checked: %v\n", err)
		return
	}
	for _, f := range ev.Files {
		if filepath.Dir(f.Path) != task.Dir(e.dirs) || !f.AfterExists || f.BeforeExists {
			continue
		}
		t, err := task.Decode(f.After)
		if err != nil {
			_, _ = fmt.Fprintf(out, "%s: %v; its branch and worktree were left as they are\n", f.Path, err)
			continue
		}
		cs, _ := e.store.List(conv.Filter{Task: t.ID, Archived: true})
		var live []string
		for _, c := range cs {
			if e.store.LockLive(c.Tool, "cairn-"+c.ID) {
				live = append(live, convTitle(c))
			}
		}
		if len(live) > 0 {
			_, _ = fmt.Fprintf(out, "task %s: worktree and branch remain: agent %s runs in it (exit it, then remove them with git)\n",
				t.Slug, strings.Join(live, ", "))
			continue
		}
		left := task.Compensate(newGit(ctxOf(cmd), t.Repos[0].Repo), t)
		if len(left) == 0 {
			_, _ = fmt.Fprintf(out, "task %s: removed its branch %s", t.Slug, t.Repos[0].Branch)
			if t.Repos[0].Worktree != "" {
				_, _ = fmt.Fprintf(out, " and worktree %s", t.Repos[0].Worktree)
			}
			_, _ = fmt.Fprintln(out)
			continue
		}
		_, _ = fmt.Fprintf(out, "task %s: kept what holds work:\n", t.Slug)
		for _, l := range left {
			_, _ = fmt.Fprintf(out, "  remains: %s\n", l)
		}
	}
}
