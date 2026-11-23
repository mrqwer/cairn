package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/hooks"
	"github.com/mrqwer/cairn/internal/agents/profile"
	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/reconcile"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Seams for tests: the runner of tool commands (list, native delete), the
// tmux driver of the server an agent pane runs in, and the tool launcher of
// the pane wrapper.
var (
	agentRunner execx.Runner = execx.OSRunner{}
	paneDriver               = func(socketFile string) (wsDriver, error) {
		if _, err := exec.LookPath("tmux"); err != nil {
			return nil, errNoTmux
		}
		c := tmux.NewClient("")
		c.SocketFile = socketFile
		return c, nil
	}
	runTool = func(_ context.Context, argv, env []string, dir string) (int, error) {
		c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv comes from the user's adapter
		c.Env = env
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			c.Dir = dir
		}
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, err
	}
)

// agentSelf is the cairn executable agent panes run.
func agentSelf() string {
	p, err := exePath()
	if err != nil {
		return "cairn"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func agentStore(d xdg.Dirs) conv.Store {
	return conv.Store{Dir: filepath.Join(d.State, "agents"), Root: wsRoot}
}

// agentsEnv is the loaded agent configuration and records.
type agentsEnv struct {
	dirs     xdg.Dirs
	store    conv.Store
	adapters map[string]adapter.Adapter
	lerrs    []adapter.LoadError
}

func loadAgents() (*agentsEnv, error) {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	as, lerrs, err := adapter.Load(wsRoot, dirs)
	if err != nil {
		return nil, err
	}
	e := &agentsEnv{dirs: dirs, store: agentStore(dirs), adapters: map[string]adapter.Adapter{}, lerrs: lerrs}
	for _, a := range as {
		e.adapters[a.Name] = a
	}
	return e, nil
}

func (e *agentsEnv) adapter(name string) (adapter.Adapter, bool) {
	a, ok := e.adapters[name]
	return a, ok
}

// resolve finds a conversation (archived included) by id, unique name or
// unique id prefix of at least 4 characters.
func (e *agentsEnv) resolve(ref string) (conv.Conversation, error) {
	all, err := e.store.List(conv.Filter{Archived: true})
	if err != nil {
		return conv.Conversation{}, err
	}
	for _, c := range all {
		if c.ID == ref {
			return c, nil
		}
	}
	pick := func(match func(conv.Conversation) bool) (conv.Conversation, bool, error) {
		var hits []conv.Conversation
		for _, c := range all {
			if match(c) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			return conv.Conversation{}, false, nil
		case 1:
			return hits[0], true, nil
		}
		ids := make([]string, len(hits))
		for i, c := range hits {
			ids[i] = c.ID
		}
		return conv.Conversation{}, false, fmt.Errorf("%q matches several conversations: %s", ref, strings.Join(ids, ", "))
	}
	if c, ok, err := pick(func(c conv.Conversation) bool { return c.Name == ref }); ok || err != nil {
		return c, err
	}
	if len(ref) >= 4 {
		up := strings.ToUpper(ref)
		if c, ok, err := pick(func(c conv.Conversation) bool { return strings.HasPrefix(c.ID, up) }); ok || err != nil {
			return c, err
		}
	}
	return conv.Conversation{}, fmt.Errorf("no conversation %q (see: cairn agents ls --archived)", ref)
}

func convTitle(c conv.Conversation) string {
	if c.Name != "" {
		return c.Name
	}
	return c.ID
}

// notRunning takes the conversation lock so the record is not changed under
// a running tool; release it when done.
func (e *agentsEnv) notRunning(c conv.Conversation) (func(), error) {
	rel, err := agents.AcquireConvLocks(e.store, c)
	var el conv.ErrLocked
	if errors.As(err, &el) {
		return nil, fmt.Errorf("%s is in use: its tool runs in a pane (%v); exit the tool first", convTitle(c), err)
	}
	return rel, err
}

// notRunningOrQueued is notRunning for archive and rm: a pane waiting in
// the agent queue holds the lock but runs no tool; it is taken out of the
// queue (atomically with its admission, so it cannot start meanwhile) and
// gives up, and the pane may then be removed.
func (e *agentsEnv) notRunningOrQueued(c conv.Conversation) (func(), error) {
	rel, err := e.notRunning(c)
	if err == nil {
		return rel, nil
	}
	if ok, derr := agents.Dequeue(e.store, c.ID); derr == nil && ok {
		return func() {}, nil
	}
	return nil, err
}

// fileEdit is one file change of a recorded operation.
type fileEdit struct {
	path                      string
	before, after             []byte
	beforeExists, afterExists bool
	perm                      fs.FileMode
}

func (f fileEdit) put(data []byte, exists bool) error {
	if !exists {
		return wsRoot.Remove(f.path)
	}
	return wsRoot.WriteAtomic(f.path, data, f.perm)
}

// commitEdits writes the edits and records them as one history event. A
// failed write rolls back the files already written.
func commitEdits(dirs xdg.Dirs, kind, summary string, edits []fileEdit) error {
	var done []fileEdit
	var changes []history.FileChange
	for _, f := range edits {
		if f.beforeExists == f.afterExists && bytes.Equal(f.before, f.after) {
			continue
		}
		if err := f.put(f.after, f.afterExists); err != nil {
			for _, d := range done {
				_ = d.put(d.before, d.beforeExists)
			}
			return fmt.Errorf("write %s: %w", f.path, err)
		}
		done = append(done, f)
		changes = append(changes, history.FileChange{Path: f.path, Before: f.before, After: f.after,
			BeforeExists: f.beforeExists, AfterExists: f.afterExists})
	}
	if len(changes) == 0 {
		return nil
	}
	if _, err := historyStore(dirs).Append(kind, summary, changes); err != nil {
		return fmt.Errorf("changes written but not recorded in history: %w", err)
	}
	return nil
}

// curatedEdit prepares a change of the curated overlay of id.
func (e *agentsEnv) curatedEdit(id string, fn func(*conv.Curated)) (fileEdit, error) {
	if err := e.store.SeedCurated(id); err != nil {
		return fileEdit{}, err
	}
	before, _, err := e.store.ReadCurated(id)
	if err != nil {
		return fileEdit{}, err
	}
	var cu conv.Curated
	if err := cu.Decode(before); err != nil {
		return fileEdit{}, fmt.Errorf("%s: %w", e.store.CuratedPath(id), err)
	}
	fn(&cu)
	return fileEdit{path: e.store.CuratedPath(id), before: before, beforeExists: true,
		after: cu.Encode(), afterExists: true, perm: 0o600}, nil
}

// projectEdit prepares a change of project id's file: decoded, changed by
// change, which returns the text edit (nil when it cannot do one). A text
// edit that decodes to the changed project keeps the user's comments and
// layout; otherwise the file is re-rendered and warn says so. The result is
// validated.
func projectEdit(dirs xdg.Dirs, id string, change func(raw *workspace.Project, before []byte) func(want workspace.Project) ([]byte, bool)) (f fileEdit, warn string, err error) {
	_, path, before, err := readProjectFile(id, true)
	if err != nil {
		return fileEdit{}, "", err
	}
	p, err := workspace.Decode(before)
	if err != nil {
		return fileEdit{}, "", fmt.Errorf("%s: %w", path, err)
	}
	orig, _ := workspace.Decode(before)
	text := change(&p, before)
	var after []byte
	switch {
	case sameProject(orig, p):
		after = before
	case text != nil:
		var ok bool
		if after, ok = text(p); !ok {
			after = nil
		}
	}
	if after == nil {
		if after, err = workspace.Marshal(p); err != nil {
			return fileEdit{}, "", err
		}
		warn = fmt.Sprintf("warning: %s was rewritten; its comments and formatting were not kept (the previous text is in cairn history)", path)
	}
	if probs := checkProjectData(id, after, dirs.Home); len(probs) > 0 {
		return fileEdit{}, "", fmt.Errorf("%s would become invalid: %s", path, strings.Join(probs, "; "))
	}
	return fileEdit{path: path, before: before, beforeExists: true, after: after, afterExists: true, perm: 0o644}, warn, nil
}

// addBindingEdit binds convID in window (named name when it is created) of
// project id; no change when bound already.
func addBindingEdit(dirs xdg.Dirs, id, convID, window, name string) (f fileEdit, key, warn string, err error) {
	f, warn, err = projectEdit(dirs, id, func(p *workspace.Project, before []byte) func(workspace.Project) ([]byte, bool) {
		var ok bool
		if key, ok = p.AgentPaneKey(convID); ok {
			return nil
		}
		key = p.AddAgentPaneIn(window, name, convID)
		wi := 0
		for i, w := range p.Windows {
			if w.Key == window {
				wi = i
			}
		}
		return func(want workspace.Project) ([]byte, bool) { return addPaneText(before, want, wi) }
	})
	return f, key, warn, err
}

// removeBindingEdit unbinds convID from project id.
func removeBindingEdit(dirs xdg.Dirs, id, convID string) (f fileEdit, window string, pos int, warn string, err error) {
	f, warn, err = projectEdit(dirs, id, func(p *workspace.Project, before []byte) func(workspace.Project) ([]byte, bool) {
		wi := -1
		for i, w := range p.Windows {
			for _, pn := range w.Panes {
				if pn.Role == workspace.RoleAgent && pn.Agent == convID {
					wi = i
				}
			}
		}
		n := len(p.Windows)
		var ok bool
		if window, pos, ok = p.RemoveAgentPane(convID); !ok {
			return nil
		}
		gone := len(p.Windows) < n
		return func(want workspace.Project) ([]byte, bool) { return removePaneText(before, want, wi, pos, gone) }
	})
	return f, window, pos, warn, err
}

// syncPaneKeys records in each conversation of project p the key of the
// pane it is bound to (keys are positional and move when a pane is removed).
func (e *agentsEnv) syncPaneKeys(p workspace.Project) {
	for _, w := range p.Windows {
		for _, pn := range w.Panes {
			if pn.Role != workspace.RoleAgent {
				continue
			}
			key, _ := p.AgentPaneKey(pn.Agent)
			_, _ = e.store.Update(pn.Agent, func(c *conv.Conversation) error { c.Pane = key; return nil })
		}
	}
}

// bindingOf finds the project and pane key conversation id is bound to.
func bindingOf(ws *wsState, id string) (workspace.Project, string, bool) {
	for _, p := range ws.projects {
		if key, ok := p.AgentPaneKey(id); ok {
			return p, key, true
		}
	}
	return workspace.Project{}, "", false
}

// unbind removes the pane binding of c (one history event together with
// the curated change, if any), then the live pane: it is first moved off
// its key (to an orphan key), the reconciler then moves the other agent
// panes to their keys by identity, and the pane is killed last (it may be
// the caller's own). Interrupted anywhere, no two panes share a key. drv is
// called only when there is a live pane to change.
func (e *agentsEnv) unbind(cmd *cobra.Command, c conv.Conversation, curated func(*conv.Curated), kind, summary string, drv func() (wsDriver, error)) error {
	unlock, err := lockApply(cmd)
	if err != nil {
		return err
	}
	defer unlock()
	ws, err := loadWorkspace()
	if err != nil {
		return err
	}
	var edits []fileEdit
	var window string
	p, _, bound := bindingOf(ws, c.ID)
	if bound {
		pe, w, _, warn, err := removeBindingEdit(e.dirs, p.ID, c.ID)
		if err != nil {
			return err
		}
		if warn != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warn)
		}
		window = w
		edits = append(edits, pe)
	}
	if curated != nil {
		ce, err := e.curatedEdit(c.ID, curated)
		if err != nil {
			return err
		}
		edits = append(edits, ce)
	}
	if err := commitEdits(e.dirs, kind, summary, edits); err != nil {
		return err
	}
	_, _ = e.store.Update(c.ID, func(r *conv.Conversation) error { r.Pane = ""; return nil })
	if !bound {
		return nil
	}
	ws2, err := loadWorkspace()
	if err != nil {
		return err
	}
	if np, ok := ws2.project(p.ID); ok {
		e.syncPaneKeys(np)
	}
	d, err := drv()
	if err != nil {
		return nil // no tmux: the config is updated, nothing live to change
	}
	return unbindLive(ctxOf(cmd), d, ws2, p.ID, window, c.ID)
}

// unbindLive retires the live pane of conversation convID in window.
func unbindLive(ctx context.Context, d wsDriver, ws *wsState, project, window, convID string) error {
	snap, err := d.Snapshot(ctx)
	if err != nil {
		return err
	}
	prefix := project + "/" + window + "/"
	target := ""
	for _, s := range snap.Sessions {
		if s.Key != project {
			continue
		}
		for _, w := range s.Windows {
			if w.Key != project+"/"+window {
				continue
			}
			for _, tp := range w.Panes {
				if tp.Agent == convID && strings.HasPrefix(tp.Key, prefix) {
					target = tp.ID
				}
			}
		}
	}
	if target != "" {
		if err := d.SetOption(ctx, "pane", target, "@cairn_p", prefix+"orphan-"+strings.TrimPrefix(target, "%")); err != nil {
			return err
		}
	}
	if snap, err = d.Snapshot(ctx); err != nil {
		return err
	}
	ops := nonDestructive(reconcile.Plan(ws.projects, snap, reconcile.Options{Only: project}))
	for _, r := range reconcile.Apply(ctx, d, ws.projects, ops) {
		if r.Err != nil {
			return fmt.Errorf("%s: %w", opText(r.Op), r.Err)
		}
	}
	if target != "" {
		return d.KillPane(ctx, target)
	}
	return nil
}

// bind makes sure c has an agent pane in its project and commits that with
// extra edits as one history event, under the apply lock so two concurrent
// resumes cannot bind the conversation twice. It records the pane key.
func (e *agentsEnv) bind(cmd *cobra.Command, c conv.Conversation, extra []fileEdit, kind, summary string) (string, error) {
	window, name := e.windowFor(c)
	return e.bindIn(cmd, c, window, name, extra, kind, summary)
}

// bindIn is bind into window (named name when it is created).
func (e *agentsEnv) bindIn(cmd *cobra.Command, c conv.Conversation, window, name string, extra []fileEdit, kind, summary string) (string, error) {
	unlock, err := lockApply(cmd)
	if err != nil {
		return "", err
	}
	defer unlock()
	pe, key, warn, err := addBindingEdit(e.dirs, c.Project, c.ID, window, name)
	if err != nil {
		return "", err
	}
	if warn != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warn)
	}
	if err := commitEdits(e.dirs, kind, summary, append([]fileEdit{pe}, extra...)); err != nil {
		return "", err
	}
	_, err = e.store.Update(c.ID, func(r *conv.Conversation) error { r.Pane = key; return nil })
	return key, err
}

// applyProject runs the reconciler (non-destructive) for project. Without
// create it does nothing when the project's session is not running.
func applyProject(cmd *cobra.Command, project string, create bool) (running bool, err error) {
	unlock, err := lockApply(cmd)
	if err != nil {
		return false, err
	}
	defer unlock()
	r, err := prepare(cmd)
	if err != nil {
		return false, err
	}
	for _, s := range r.snap.Sessions {
		running = running || s.Key == project
	}
	if !running && !create {
		return false, nil
	}
	ops := nonDestructive(r.plan(project, false))
	for _, op := range ops {
		if isSkip(op) {
			return running, fmt.Errorf("cannot apply %s: %s", project, op.Detail)
		}
	}
	if actionable(ops) {
		if err := renderResults(cmd.OutOrStdout(), reconcile.Apply(ctxOf(cmd), r.drv, r.ws.projects, ops)); err != nil {
			return running, err
		}
	}
	return true, nil
}

// jumpToPane selects the pane marked key and brings the user to its session.
func jumpToPane(cmd *cobra.Command, project, key string) error {
	socket := socketName(cmd)
	drv, err := newDriver(socket)
	if err != nil {
		return err
	}
	ctx := ctxOf(cmd)
	snap, err := drv.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, s := range snap.Sessions {
		if s.Key != project {
			continue
		}
		for _, w := range s.Windows {
			for _, p := range w.Panes {
				if p.Key == key {
					if err := drv.SelectPane(ctx, p.ID); err != nil {
						return err
					}
					return switchOrAttach(cmd, drv, socket, s.Name, s.ID)
				}
			}
		}
	}
	return fmt.Errorf("pane %s is not running; see: cairn plan %s", key, project)
}

func newAgentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "AI agent conversations: start, resume, list, find, import, name, tag, archive, remove, profiles, usage",
	}
	cmd.AddCommand(newAgentsNewCmd(), newAgentsResumeCmd(), newAgentsLsCmd(), newAgentsNameCmd(),
		newAgentsTagCmd(), newAgentsArchiveCmd(), newAgentsRmCmd(), newAgentsPaneCmd(), newAgentsDoctorCmd(),
		newAgentsImportCmd(), newAgentsFindCmd(), newAgentsIndexCmd(), newAgentsProfileCmd(), newAgentsUsageCmd())
	return cmd
}

// projectOfDir is the active project whose root contains dir (the deepest).
func projectOfDir(ws *wsState, dir string) (workspace.Project, bool) {
	var best workspace.Project
	found := false
	for _, p := range ws.projects {
		rel, err := filepath.Rel(p.Root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if !found || len(p.Root) > len(best.Root) {
			best, found = p, true
		}
	}
	return best, found
}

func newAgentsNewCmd() *cobra.Command {
	var name, project, taskRef, profileName, cwd string
	var now bool
	cmd := &cobra.Command{
		Use:   "new <tool>",
		Short: "Start a new conversation in an agent pane of a project",
		Long: "Start a new conversation in an agent pane of a project.\n\n" +
			"cairn undo of this removes the pane binding only; the conversation record stays\n" +
			"listed (unbound) until you archive or remove it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			a, ok := e.adapter(args[0])
			if !ok {
				names := make([]string, 0, len(e.adapters))
				for n := range e.adapters {
					names = append(names, n)
				}
				sort.Strings(names)
				return fmt.Errorf("unknown tool %q (one of: %s)", args[0], strings.Join(names, ", "))
			}
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			var tk task.Task
			if taskRef != "" {
				ts, err := loadTasks(e.dirs, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				if tk, err = task.Find(ts, taskRef); err != nil {
					return err
				}
				if project == "" {
					project = tk.Project
				} else if project != tk.Project {
					return fmt.Errorf("task %s belongs to project %s, not %s", tk.Slug, tk.Project, project)
				}
			}
			p, err := projectFor(ws, project)
			if err != nil {
				return err
			}
			switch {
			case cwd == "" && tk.ID != "":
				cwd = tk.Repos[0].Repo
				if tk.Repos[0].Worktree != "" {
					cwd = tk.Repos[0].Worktree
				}
			case cwd == "":
				cwd = p.Root
			default:
				cwd, err = filepath.Abs(cwd)
			}
			if err != nil {
				return err
			}
			if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
				return fmt.Errorf("working directory %s does not exist", cwd)
			}
			if err := checkConvName(name, true); err != nil {
				return err
			}
			if err := checkProfileFlag(profileName); err != nil {
				return err
			}
			setup, err := e.launchProfile(a, p, profileName, tk.Profile)
			if err != nil {
				return err
			}
			t := time.Now().UTC()
			c := conv.Conversation{ID: conv.NewID(t, nil), Tool: a.Name, Name: name, Project: p.ID, Task: tk.ID,
				Cwd: cwd, Profile: profileName, Risk: len(setup.risky) > 0, State: conv.StateStarting, CreatedAt: t, LastActivityAt: t}
			if tk.ID != "" {
				c.Worktree, c.Branch = tk.Repos[0].Worktree, tk.Repos[0].Branch
			}
			if a.Capabilities.PreassignID {
				c.NativeID = agents.NewNativeID()
			}
			if c.Name == "" {
				c.Name = a.Name + "-" + strings.ToLower(c.ID[len(c.ID)-6:])
			}
			if err := e.store.Put(c); err != nil {
				return err
			}
			if err := e.store.SeedCurated(c.ID); err != nil {
				return err
			}
			key, err := e.bind(cmd, c, nil, "agents.new", fmt.Sprintf("new agent %s (%s) in %s", c.Name, a.Name, p.ID))
			if err != nil {
				return err
			}
			if now {
				if err := agents.RequestStart(e.store, c.ID); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "created agent %s (%s, %s) in %s, pane %s\n", ui.SafeLine(c.Name), c.ID, a.Name, p.ID, key)
			setup.report(out, a.Name, p.ID)
			e.admitNew(out, cmd.ErrOrStderr(), c)
			running, err := applyProject(cmd, p.ID, false)
			if err != nil {
				return err
			}
			if !running {
				_, _ = fmt.Fprintf(out, "the session of %s is not running; start it with: cairn open %s\n", p.ID, p.ID)
			} else if !now {
				_, _ = fmt.Fprintln(out, "the tool starts when you attach to the session (or use --now)")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "conversation name (default: <tool>-<id suffix>)")
	f.StringVar(&project, "project", "", "project (default: the one containing the current directory)")
	f.StringVar(&taskRef, "task", "", "task (id or slug): its project, worktree and window")
	f.StringVar(&profileName, "profile", "", "profile (default: the task's, else the project's default_profile; see: cairn agents profile ls)")
	f.StringVar(&cwd, "cwd", "", "working directory of the tool (default: the project root)")
	f.BoolVar(&now, "now", false, "start the tool at once instead of on first attach")
	return cmd
}

// checkConvName rejects names a tool could misread: control characters,
// and a leading "-" (names are passed in tool argv, e.g. --name {name}).
func checkConvName(name string, emptyOK bool) error {
	switch {
	case name == "" && !emptyOK:
		return errors.New("the name must not be empty")
	case hasControlChars(name):
		return errors.New("the name must not contain control characters")
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("the name %q must not start with \"-\"", name)
	}
	return nil
}

func hasControlChars(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

func newAgentsResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <id|name>",
		Short: "Jump to a conversation's pane, opening one (and unarchiving) if needed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			c, err := e.resolve(args[0])
			if err != nil {
				return err
			}
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			var extra []fileEdit
			if c.ArchivedAt != nil {
				ce, err := e.curatedEdit(c.ID, func(cu *conv.Curated) { cu.ArchivedAt = nil })
				if err != nil {
					return err
				}
				extra = append(extra, ce)
			}
			p, key, bound := bindingOf(ws, c.ID)
			summary := "resume agent " + convTitle(c)
			if bound {
				if err := commitEdits(e.dirs, "agents.resume", summary, extra); err != nil {
					return err
				}
			} else {
				if c.Project == "" {
					return fmt.Errorf("%s has no project to open it in", convTitle(c))
				}
				if p, err = ws.requireProject(c.Project); err != nil {
					return err
				}
				if key, err = e.bind(cmd, c, extra, "agents.resume", summary); err != nil {
					return err
				}
			}
			// Start at once unless the tool already runs in its pane.
			if rel, err := e.notRunning(c); err == nil {
				rel()
				e.admitNew(cmd.OutOrStdout(), cmd.ErrOrStderr(), c)
				if err := agents.RequestStart(e.store, c.ID); err != nil {
					return err
				}
			}
			if _, err := applyProject(cmd, p.ID, true); err != nil {
				return err
			}
			return jumpToPane(cmd, p.ID, key)
		},
	}
}

// lsJSON is one conversation in agents ls --json.
type lsJSON struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Purpose        string    `json:"purpose"`
	Tool           string    `json:"tool"`
	NativeID       string    `json:"native_id"`
	Project        string    `json:"project"`
	Task           string    `json:"task"`
	State          string    `json:"state"`
	Tags           []string  `json:"tags"`
	Pane           string    `json:"pane"`
	Profile        string    `json:"profile"`
	Risk           bool      `json:"risk"`
	Archived       bool      `json:"archived"`
	CreatedAt      time.Time `json:"created_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
}

func newAgentsLsCmd() *cobra.Command {
	var f conv.Filter
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List conversations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			cs, err := e.store.List(f)
			if err != nil {
				return err
			}
			// The pane comes from the bindings in the config, which undo
			// restores; the record's copy may be stale.
			if ws, err := loadWorkspace(); err == nil {
				for i := range cs {
					cs[i].Pane = ""
					if _, key, ok := bindingOf(ws, cs[i].ID); ok {
						cs[i].Pane = key
					}
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				rows := make([]lsJSON, 0, len(cs))
				for _, c := range cs {
					tags := c.Tags
					if tags == nil {
						tags = []string{}
					}
					rows = append(rows, lsJSON{ID: c.ID, Name: c.Name, Purpose: c.Purpose, Tool: c.Tool, NativeID: c.NativeID,
						Project: c.Project, Task: c.Task, State: c.State, Tags: tags, Pane: c.Pane, Profile: c.Profile, Risk: c.Risk,
						Archived: c.ArchivedAt != nil || c.State == conv.StateArchived, CreatedAt: c.CreatedAt, LastActivityAt: c.LastActivityAt})
				}
				return writeJSON(out, rows)
			}
			if len(cs) == 0 {
				_, _ = fmt.Fprintln(out, "no conversations")
				return nil
			}
			st, styled := styleFor(out)
			head := fmt.Sprintf("  %s %s %s %s %s %s", ui.Pad("ID", 26), ui.Pad("NAME", 28), ui.Pad("TOOL", 8),
				ui.Pad("PROJECT", 14), ui.Pad("STATE", 9), "TAGS")
			if styled {
				head = st.Dim(head)
			}
			_, _ = fmt.Fprintln(out, head)
			for _, c := range cs {
				state := c.State
				if c.ArchivedAt != nil {
					state = conv.StateArchived
				}
				badge := ui.Pad(profile.Badge(c.Risk), 1)
				if styled && c.Risk {
					badge = st.Yellow(badge)
				}
				_, _ = fmt.Fprintf(out, "%s %s %s %s %s %s %s\n", badge, ui.Pad(c.ID, 26), ui.Pad(ui.SafeLine(c.Name), 28), ui.Pad(ui.SafeLine(c.Tool), 8),
					ui.Pad(ui.SafeLine(c.Project), 14), ui.Pad(state, 9), ui.SafeLine(strings.Join(c.Tags, ",")))
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.Project, "project", "", "only this project")
	fl.StringVar(&f.Task, "task", "", "only this task")
	fl.StringVar(&f.Tool, "tool", "", "only this tool")
	fl.StringVar(&f.State, "state", "", "only this state")
	fl.StringVar(&f.Tag, "tag", "", "only conversations with this tag")
	fl.BoolVar(&f.Archived, "archived", false, "include archived conversations")
	fl.BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// locateTranscript is the transcript file of c: its transcript_ref, else
// the adapter's history.locator with the native id (exactly one match).
func (e *agentsEnv) locateTranscript(c conv.Conversation) (string, error) {
	if c.TranscriptRef != "" {
		return c.TranscriptRef, nil
	}
	a, ok := e.adapter(c.Tool)
	if !ok || a.History.Locator == "" || c.NativeID == "" {
		return "", fmt.Errorf("no transcript known for %s", convTitle(c))
	}
	loc := a.History.Locator
	if strings.HasPrefix(loc, "~/") {
		loc = filepath.Join(e.dirs.Home, loc[2:])
	}
	ms, err := filepath.Glob(wsRoot.Path(strings.ReplaceAll(loc, "{native_id}", c.NativeID)))
	if err != nil {
		return "", err
	}
	if len(ms) != 1 {
		return "", fmt.Errorf("no single transcript for %s (%d matches of %s)", convTitle(c), len(ms), loc)
	}
	return ms[0], nil
}

func newAgentsNameCmd() *cobra.Command {
	var purpose string
	var apply bool
	cmd := &cobra.Command{
		Use:   "name <id> [<name>]",
		Short: "Name a conversation; without a name, suggest one from its first prompt",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			c, err := e.resolve(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			name, setPurpose := "", cmd.Flags().Changed("purpose")
			if len(args) == 2 {
				name = strings.TrimSpace(args[1])
			} else {
				path, err := e.locateTranscript(c)
				if err != nil {
					return err
				}
				prompt, err := agents.FirstPrompt(path)
				if err != nil {
					return fmt.Errorf("%s: %w", ui.SafeLine(path), err)
				}
				// The prompt is untrusted transcript text: no control
				// sequences reach the terminal or the record.
				sname, spurpose := agents.SuggestName(ui.SafeLine(prompt))
				if sname == "" {
					return errors.New("no name could be suggested from the first prompt")
				}
				name = sname
				if !setPurpose {
					purpose, setPurpose = spurpose, true
				}
				_, _ = fmt.Fprintf(out, "suggested name: %s\npurpose: %s\n", name, purpose)
				if !apply {
					_, _ = fmt.Fprintf(out, "apply with: cairn agents name %s --apply\n", c.ID)
					return nil
				}
			}
			if err := checkConvName(name, false); err != nil {
				return err
			}
			if hasControlChars(purpose) {
				return errors.New("the purpose must not contain control characters")
			}
			ce, err := e.curatedEdit(c.ID, func(cu *conv.Curated) {
				cu.Name = name
				if setPurpose {
					cu.Purpose = purpose
				}
			})
			if err != nil {
				return err
			}
			if err := commitEdits(e.dirs, "agents.name", fmt.Sprintf("name agent %s: %s", c.ID, name), []fileEdit{ce}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "named %s: %s\n", c.ID, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&purpose, "purpose", "", "one-line purpose")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the suggested name")
	return cmd
}

func newAgentsTagCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag <id> +tag -tag ...",
		Short: "Add (+tag or tag) and remove (-tag) tags",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			c, err := e.resolve(args[0])
			if err != nil {
				return err
			}
			type op struct {
				add bool
				tag string
			}
			var ops []op
			for _, a := range args[1:] {
				o := op{add: true, tag: a}
				switch {
				case strings.HasPrefix(a, "+"):
					o.tag = a[1:]
				case strings.HasPrefix(a, "-"):
					o = op{add: false, tag: a[1:]}
				}
				if o.tag == "" || strings.ContainsAny(o.tag, ",") || strings.IndexFunc(o.tag, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
					return fmt.Errorf("invalid tag %q: no spaces, commas or control characters", a)
				}
				ops = append(ops, o)
			}
			var tags []string
			ce, err := e.curatedEdit(c.ID, func(cu *conv.Curated) {
				for _, o := range ops {
					i := -1
					for j, t := range cu.Tags {
						if t == o.tag {
							i = j
						}
					}
					switch {
					case o.add && i < 0:
						cu.Tags = append(cu.Tags, o.tag)
					case !o.add && i >= 0:
						cu.Tags = append(cu.Tags[:i], cu.Tags[i+1:]...)
					}
				}
				tags = cu.Tags
			})
			if err != nil {
				return err
			}
			if err := commitEdits(e.dirs, "agents.tag", fmt.Sprintf("tag agent %s: %s", c.ID, strings.Join(args[1:], " ")), []fileEdit{ce}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", convTitle(c), strings.Join(tags, ", "))
			return nil
		},
	}
	// "-tag" after the id is an argument, not a flag.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func newAgentsArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <id>",
		Short: "Hide a conversation and remove its pane (resumable, undoable)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			c, err := e.resolve(args[0])
			if err != nil {
				return err
			}
			rel, err := e.notRunningOrQueued(c)
			if err != nil {
				return err
			}
			defer rel()
			t := time.Now().UTC()
			if err := e.unbind(cmd, c, func(cu *conv.Curated) { cu.ArchivedAt = &t }, "agents.archive",
				"archive agent "+convTitle(c), func() (wsDriver, error) { return newDriver(socketName(cmd)) }); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "archived %s (cairn agents resume %s brings it back; cairn undo reverts)\n", convTitle(c), c.ID)
			return nil
		},
	}
}

func newAgentsRmCmd() *cobra.Command {
	var native, transcript, yes bool
	cmd := &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove a conversation (undoable; --native and --transcript are not)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			c, err := e.resolve(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var deleteArgv []string
			if native {
				a, ok := e.adapter(c.Tool)
				if !ok {
					return fmt.Errorf("no adapter for tool %q: native delete unavailable", c.Tool)
				}
				if deleteArgv, err = agents.BuildDelete(a, c); err != nil {
					return fmt.Errorf("native delete: %w", err)
				}
			}
			var tpath string
			if transcript {
				if !yes {
					_, _ = fmt.Fprintln(out, "not undoable: --transcript deletes the transcript file")
					return errors.New("deleting the transcript needs --yes")
				}
				if tpath, err = e.locateTranscript(c); err != nil {
					return err
				}
			}
			if native || transcript {
				if native {
					_, _ = fmt.Fprintf(out, "not undoable: delete the %s conversation %s (%s)\n", c.Tool, c.NativeID, strings.Join(deleteArgv, " "))
				}
				if tpath != "" {
					_, _ = fmt.Fprintf(out, "not undoable: delete the transcript %s\n", tpath)
				}
				if !yes {
					if !interactive(cmd.InOrStdin()) {
						return errors.New("irreversible removal needs confirmation: re-run with --yes")
					}
					ok, err := ask(ctxOf(cmd), cmd.InOrStdin(), out, "Continue?")
					if err != nil {
						return err
					}
					if !ok {
						_, _ = fmt.Fprintln(out, "nothing removed")
						return nil
					}
				}
			}
			rel, err := e.notRunningOrQueued(c)
			if err != nil {
				return err
			}
			defer rel()
			// The reversible part first: the record can be brought back even
			// if an irreversible step below fails.
			if err := e.unbind(cmd, c, func(cu *conv.Curated) { cu.Removed = true }, "agents.rm",
				"remove agent "+convTitle(c), func() (wsDriver, error) { return newDriver(socketName(cmd)) }); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "removed %s (cairn undo brings the record back)\n", convTitle(c))
			if deleteArgv != nil {
				if b, err := agentRunner.Run(ctxOf(cmd), deleteArgv[0], deleteArgv[1:]...); err != nil {
					return fmt.Errorf("native delete failed: %w: %s", err, strings.TrimSpace(string(b)))
				}
				_, _ = fmt.Fprintf(out, "deleted the %s conversation %s (not undoable)\n", c.Tool, c.NativeID)
			}
			if tpath != "" {
				if err := wsRoot.Remove(tpath); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "deleted the transcript %s (not undoable)\n", tpath)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&native, "native", false, "also delete the conversation in the tool (not undoable)")
	cmd.Flags().BoolVar(&transcript, "transcript", false, "also delete the transcript file (not undoable; needs --yes)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask before irreversible steps")
	return cmd
}

// rawKeys returns a switch to single-key input when in is a terminal.
func rawKeys(in io.Reader) func() (restore func()) {
	f, ok := in.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	return func() func() {
		fd := int(f.Fd())
		old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			return func() {}
		}
		raw := *old
		raw.Lflag &^= unix.ICANON | unix.ECHO
		raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
		if unix.IoctlSetTermios(fd, unix.TCSETS, &raw) != nil {
			return func() {}
		}
		return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }
	}
}

func newAgentsPaneCmd() *cobra.Command {
	var now bool
	cmd := &cobra.Command{
		Use:    "pane <id>",
		Short:  "Run inside an agent pane: show the conversation and start its tool lazily",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := loadAgents()
			if err != nil {
				return err
			}
			// Ctrl-C belongs to the tool: keep SIGINT caught for the whole
			// life of the wrapper so it never ends the pane (the tool gets
			// the default disposition back at exec).
			sigint := make(chan os.Signal, 1)
			signal.Notify(sigint, os.Interrupt)
			defer signal.Stop(sigint)
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			socketFile, _, _ := strings.Cut(os.Getenv("TMUX"), ",")
			paneID := os.Getenv("TMUX_PANE")
			drv := func() (wsDriver, error) {
				if socketFile != "" {
					return paneDriver(socketFile)
				}
				return newDriver(socketName(cmd))
			}
			env := agents.PaneEnv{
				Store:   e.store,
				Adapter: e.adapter,
				Attached: func(ctx context.Context) (bool, error) {
					if socketFile == "" || paneID == "" {
						return true, nil // not in tmux: the user is here
					}
					c := tmux.NewClient("")
					c.SocketFile = socketFile
					return c.SessionAttached(ctx, paneID)
				},
				Run:     runTool,
				Keys:    cmd.InOrStdin(),
				RawKeys: rawKeys(cmd.InOrStdin()),
				Out:     cmd.OutOrStdout(),
				Now:     now,
				Root:    wsRoot,
				Home:    e.dirs.Home,
				Runner:  agentRunner,
				Hooks: func(a adapter.Adapter, c conv.Conversation) ([]string, error) {
					return hooks.Inject(a, c.ID, agentSelf(), e.dirs, wsRoot)
				},
				Activity:    terminalActivity(cmd.InOrStdin()),
				ProfileArgs: e.paneProfileArgs(cmd.OutOrStdout()),
				Close: func(c conv.Conversation) error {
					return e.unbind(cmd, c, nil, "agents.close", "close agent pane of "+convTitle(c), drv)
				},
				Archive: func(c conv.Conversation) error {
					t := time.Now().UTC()
					var cur func(*conv.Curated)
					if _, ok, _ := e.store.Get(c.ID); ok {
						cur = func(cu *conv.Curated) { cu.ArchivedAt = &t }
					}
					return e.unbind(cmd, c, cur, "agents.archive", "archive agent "+convTitle(c), drv)
				},
			}
			q := agentQueue(e.dirs, e.store, cmd.OutOrStdout())
			env.Queue = &q
			err = agents.RunPane(ctx, env, args[0])
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&now, "now", false, "start the tool without waiting for a client")
	return cmd
}

// terminalActivity reports when the pane's terminal last showed output:
// the kernel moves a tty's modification time when it is written to (in
// steps of a few seconds), so no tmux call is needed. nil when in is not a
// terminal.
func terminalActivity(in io.Reader) func() (time.Time, error) {
	f, ok := in.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	return func() (time.Time, error) {
		fi, err := f.Stat()
		if err != nil {
			return time.Time{}, err
		}
		return fi.ModTime(), nil
	}
}
