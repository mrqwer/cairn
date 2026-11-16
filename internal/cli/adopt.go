package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/adopt"
	"github.com/mrqwer/cairn/internal/capture"
	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/reconcile"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

// newAdoptCmd is "cairn adopt": it brings a live, unmanaged session under
// management by setting only cairn's user options (@cairn_s, @cairn_w,
// @cairn_p) on it; nothing is renamed, moved, resized or restarted.
func newAdoptCmd() *cobra.Command {
	var id, root, release string
	var dryRun, force bool
	cmd := &cobra.Command{
		Use:   "adopt <tmux-session> | --release <project>",
		Short: "Manage a live tmux session as a project without restarting it",
		Long: "Capture a live tmux session that cairn does not manage (like cairn capture), mark its\n" +
			"session, windows and panes as managed (cairn's @cairn_* user options only) and save the\n" +
			"project config. The programs keep running; nothing is renamed, moved or resized.\n\n" +
			"cairn undo removes the saved config only; cairn adopt --release <project> removes the\n" +
			"markers again (the config stays).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if release != "" {
				if len(args) > 0 || id != "" || root != "" || dryRun || force {
					return errors.New("--release takes only the project id: cairn adopt --release <project>")
				}
				if err := checkID(release); err != nil {
					return err
				}
				return adoptRelease(cmd, release)
			}
			if len(args) != 1 {
				return errors.New("adopt needs a tmux session name (list them with: cairn capture --list)")
			}
			if id != "" {
				if err := checkID(id); err != nil {
					return err
				}
			}
			return adoptSession(cmd, args[0], id, root, dryRun, force)
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "project id (default: the session name as a slug)")
	cmd.Flags().StringVar(&root, "root", "", "project root (default: the directory most panes are in)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the project and the markers; change nothing")
	cmd.Flags().BoolVar(&force, "force", false, "adopt even though tmux listed lines cairn could not parse")
	cmd.Flags().StringVar(&release, "release", "", "remove the markers of this project from its live session (the config stays)")
	return cmd
}

// adoptSession runs the adoption. Order: everything is checked first
// (project file absent, session unmarked, every mark free), then the
// markers are set (reversible: each verified on a fresh snapshot, rolled
// back on failure), then the config is written and recorded in history.
// When the write or the history record fails, the file is removed and the
// markers are removed again, so a failed adopt leaves nothing behind.
func adoptSession(cmd *cobra.Command, name, id, root string, dryRun, force bool) error {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return err
	}
	exists := func(id string) error {
		path := projectFile(dirs, id)
		if !wsRoot.Exists(path) {
			return nil
		}
		return fmt.Errorf("project %s already exists (%s); pick another --id (adopting never changes an existing project)", id, path)
	}
	// With --id the refusal comes before tmux is even read.
	if id != "" && !dryRun {
		if err := exists(id); err != nil {
			return err
		}
	}
	if root, err = resolveRoot(root, dirs.Home); err != nil {
		return err
	}
	drv, snap, err := captureSnapshot(cmd)
	if err != nil {
		return err
	}
	s, err := findSession(snap, name)
	if err != nil {
		return err
	}
	if s.Key != "" {
		return fmt.Errorf("session %q is already managed by cairn (project %s); to adopt it again, release it first: cairn adopt --release %s", s.Name, s.Key, s.Key)
	}
	isIdle := func(pid int) bool {
		ok, err := drv.PaneIdle(pid)
		return err == nil && ok
	}
	res, err := capture.Session(s, capture.Options{ID: id, Root: root, IsIdle: isIdle})
	if err != nil {
		return err
	}
	p, notes := res.Project, res.Notes
	if len(snap.Skipped) > 0 {
		note := skippedNote(snap.Skipped)
		warn := "warning: " + note
		if st, styled := styleFor(cmd.ErrOrStderr()); styled {
			warn = st.Red(warn)
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warn)
		if !dryRun && !force {
			return fmt.Errorf("refusing to adopt: tmux listed lines cairn could not parse, so windows or panes may be missing from the capture; "+
				"check the proposal (cairn adopt %s --dry-run) and re-run with --force to adopt it anyway", shellWord(name))
		}
		notes = append([]string{note}, notes...)
	}
	path := projectFile(dirs, p.ID) // capture.Session validated the id
	if err := exists(p.ID); err != nil {
		if !dryRun {
			return err
		}
		notes = append(notes, err.Error())
	}
	if fi, err := wsRoot.Stat(p.Root); err != nil || !fi.IsDir() {
		notes = append(notes, fmt.Sprintf("root %s does not exist; apply skips the project until it does", p.Root))
	}
	marks := adopt.Markers(res)
	if err := adopt.Check(snap, marks); err != nil {
		return fmt.Errorf("cannot adopt session %q: %w", s.Name, err)
	}
	data, err := workspace.Marshal(p)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printProposal(out, "adopt", s, p, data, notes)
	printMarks(out, marks)
	if dryRun {
		_, _ = fmt.Fprintf(out, "\n# dry run: nothing changed; adopt it with: cairn adopt %s --id %s\n", shellWord(s.Name), p.ID)
		return nil
	}

	ctx := ctxOf(cmd)
	if err := adopt.Apply(ctx, drv, snap, marks); err != nil {
		return err
	}
	undoMarks := func(cause error) error {
		if errors.Is(cause, context.Canceled) {
			cause = fmt.Errorf("adopt interrupted: %w", cause)
		}
		if err := adopt.Unmark(ctx, drv, marks); err != nil {
			return fmt.Errorf("%w; removing the markers again failed (remove them with: cairn adopt --release %s): %v", cause, p.ID, err)
		}
		return fmt.Errorf("%w; the markers were removed again, nothing was adopted", cause)
	}
	// Interrupted after the markers: take them back, write nothing.
	if err := ctx.Err(); err != nil {
		return undoMarks(err)
	}
	if err := wsRoot.MkdirAll(projectsDir(dirs), 0o755); err != nil {
		return undoMarks(err)
	}
	if err := wsRoot.CreateExclusive(path, data, 0o644); err != nil {
		if errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("project %s already exists (%s)", p.ID, path)
		}
		return undoMarks(err)
	}
	if _, err := historyStore(dirs).Append("adopt", fmt.Sprintf("adopt session %s as project %s", s.Name, p.ID),
		[]history.FileChange{{Path: path, After: data, AfterExists: true}}); err != nil {
		cause := fmt.Errorf("recording the adoption in history failed: %w", err)
		if rerr := removeProjectFile(path); rerr != nil {
			cause = fmt.Errorf("%w; removing %s failed: %v", cause, path, rerr)
			sess := p.Session
			if sess == "" {
				sess = p.ID
			}
			warnNewSession(cmd.ErrOrStderr(), p.ID, sess, s.Name)
		}
		return undoMarks(cause)
	}
	windows, panes := 0, 0
	for _, m := range marks {
		switch m.Scope {
		case "window":
			windows++
		case "pane":
			panes++
		}
	}
	_, _ = fmt.Fprintf(out, "\nmarked session %s: 1 session, %s, %s\nwrote %s\n\n", s.Name,
		ui.Plural(windows, "window", "windows"), ui.Plural(panes, "pane", "panes"), path)
	if err := adoptPlan(ctx, out, drv, p.ID); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nRun: cairn plan %s\n", p.ID)
	return nil
}

// printMarks lists the markers as TOML comments (the output stays a valid
// project file).
func printMarks(w io.Writer, marks []adopt.Mark) {
	st, styled := styleFor(w)
	_, _ = fmt.Fprintln(w)
	for _, m := range marks {
		line := fmt.Sprintf("# mark: %s %s %s = %s", m.Scope, m.Target, m.Name, m.Value)
		if styled {
			line = st.Dim(line)
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// adoptPlan prints what apply would now do for the adopted project.
func adoptPlan(ctx context.Context, w io.Writer, drv wsDriver, id string) error {
	ws, err := loadWorkspace()
	if err != nil {
		return err
	}
	if _, err := ws.requireProject(id); err != nil {
		return err
	}
	snap, err := drv.Snapshot(ctx)
	if err != nil {
		return err
	}
	renderPlan(w, reconcile.Plan(ws.projects, snap, reconcile.Options{Only: id, PaneIdle: drv.PaneIdle}))
	return nil
}

// adoptRelease removes project id's markers from its live session (and
// orphan markers in unmarked sessions). The project file is not needed (an
// interrupted adopt can be released too) and is never changed; when it
// exists, its session name decides what apply will do next.
func adoptRelease(cmd *cobra.Command, id string) error {
	drv, err := newDriver(socketName(cmd))
	if err != nil {
		return err
	}
	ctx := ctxOf(cmd)
	snap, err := drv.Snapshot(ctx)
	if err != nil {
		return err
	}
	live := adopt.Sessions(snap, id)
	n, err := adopt.Release(ctx, drv, id)
	out := cmd.OutOrStdout()
	if n > 0 {
		_, _ = fmt.Fprintf(out, "removed %s of project %s; the tmux session, windows and panes are unchanged\n",
			ui.Plural(n, "marker", "markers"), id)
	}
	if err != nil {
		return err
	}
	ws, err := loadWorkspace()
	if err != nil {
		return err
	}
	p, ok := ws.project(id)
	if !ok {
		return nil
	}
	path := projectFile(ws.dirs, id)
	for _, s := range live {
		if s.Name == p.Session {
			_, _ = fmt.Fprintf(out, "the project config stays (%s); while the live session keeps the name %q, cairn plan reports a name clash\n",
				path, s.Name)
			continue
		}
		warnNewSession(cmd.ErrOrStderr(), id, p.Session, s.Name)
	}
	return nil
}

// warnNewSession warns that a project config names another session than
// the live one it was adopted from, so apply/up would start a second one.
func warnNewSession(w io.Writer, id, session, live string) {
	if session == live {
		return
	}
	msg := fmt.Sprintf("warning: project %s will create a NEW tmux session %q on cairn apply/up (the live session is named %q) — run cairn project archive %s or cairn undo first",
		id, session, live, id)
	if st, styled := styleFor(w); styled {
		msg = st.Yellow(msg)
	}
	_, _ = fmt.Fprintln(w, msg)
}

// removeProjectFile removes a project file (a seam for tests).
var removeProjectFile = func(path string) error { return wsRoot.Remove(path) }
