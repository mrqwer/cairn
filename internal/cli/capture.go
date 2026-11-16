package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/capture"
	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

// newCaptureCmd is "cairn capture": it only reads tmux (a snapshot and the
// idle check); it never changes anything on the server.
func newCaptureCmd() *cobra.Command {
	var id, root string
	var write, list, force bool
	cmd := &cobra.Command{
		Use:   "capture <tmux-session>",
		Short: "Turn a live, unmanaged tmux session into a project config",
		Long: "Read a live tmux session that cairn does not manage and print the project config\n" +
			"that recreates it. With --write the config is saved (and recorded in history).\n" +
			"tmux is only read, never changed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				if len(args) > 0 || write || force || id != "" || root != "" {
					return errors.New("--list takes no session name and no --write, --id or --root")
				}
				return captureList(cmd)
			}
			if len(args) != 1 {
				return errors.New("capture needs a tmux session name (list them with: cairn capture --list)")
			}
			if id != "" {
				if err := checkID(id); err != nil {
					return err
				}
			}
			if force && !write {
				return errors.New("--force only applies with --write")
			}
			return captureSession(cmd, args[0], id, root, write, force)
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "project id (default: the session name as a slug)")
	cmd.Flags().StringVar(&root, "root", "", "project root (default: the directory most panes are in)")
	cmd.Flags().BoolVar(&write, "write", false, "save the project config instead of only printing it")
	cmd.Flags().BoolVar(&force, "force", false, "with --write: save even though tmux listed lines cairn could not parse")
	cmd.Flags().BoolVar(&list, "list", false, "list the live sessions cairn does not manage")
	cmd.Flags().BoolVar(&list, "all-unmanaged", false, "same as --list")
	_ = cmd.Flags().MarkHidden("all-unmanaged")
	return cmd
}

func captureSnapshot(cmd *cobra.Command) (wsDriver, tmux.Snapshot, error) {
	drv, err := newDriver(socketName(cmd))
	if err != nil {
		return nil, tmux.Snapshot{}, err
	}
	snap, err := drv.Snapshot(ctxOf(cmd))
	return drv, snap, err
}

func captureList(cmd *cobra.Command) error {
	_, snap, err := captureSnapshot(cmd)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	sessions := capture.Unmanaged(snap)
	if len(sessions) == 0 {
		_, _ = fmt.Fprintln(out, "no unmanaged tmux sessions")
		return nil
	}
	st, styled := styleFor(out)
	head := fmt.Sprintf("%s %s %s", ui.Pad("SESSION", 24), ui.Pad("WINDOWS", 8), "PANES")
	if styled {
		head = st.Bold(head)
	}
	_, _ = fmt.Fprintln(out, head)
	for _, s := range sessions {
		panes := 0
		for _, w := range s.Windows {
			panes += len(w.Panes)
		}
		_, _ = fmt.Fprintf(out, "%s %s %d\n", ui.Pad(s.Name, 24), ui.Pad(fmt.Sprint(len(s.Windows)), 8), panes)
	}
	hint := "capture one: cairn capture <session>"
	if styled {
		hint = st.Dim(hint)
	}
	_, _ = fmt.Fprintln(out, hint)
	return nil
}

// findSession looks a session up by its exact tmux name. tmux replaces '.'
// and ':' in session names with '_', so a miss suggests that spelling.
func findSession(snap tmux.Snapshot, name string) (tmux.Session, error) {
	s, err := capture.Find(snap, name)
	if err == nil {
		return s, nil
	}
	msg := fmt.Sprintf("no tmux session named %q", name)
	if alt := strings.NewReplacer(".", "_", ":", "_").Replace(name); alt != name {
		if _, err := capture.Find(snap, alt); err == nil {
			return s, fmt.Errorf("%s; tmux names it %q (it replaces '.' and ':' with '_'): cairn capture %s", msg, alt, alt)
		}
	}
	return s, fmt.Errorf("%s; list them with: cairn capture --list", msg)
}

// resolveRoot makes a --root value absolute ("~/" expands to the home).
func resolveRoot(root, home string) (string, error) {
	switch {
	case root == "":
		return "", nil
	case root == "~":
		return home, nil
	case strings.HasPrefix(root, "~/"):
		return filepath.Join(home, root[2:]), nil
	}
	return filepath.Abs(root)
}

// skippedNote describes the snapshot lines tmux listed that could not be
// parsed (the first three, shortened).
func skippedNote(skipped []string) string {
	shown := make([]string, 0, 3)
	for _, l := range skipped[:min(3, len(skipped))] {
		shown = append(shown, fmt.Sprintf("%q", ui.Truncate(oneLine(l), 40)))
	}
	return fmt.Sprintf("%s could not be parsed (%s) — the capture may be incomplete",
		ui.Plural(len(skipped), "tmux line", "tmux lines"), strings.Join(shown, ", "))
}

func captureSession(cmd *cobra.Command, name, id, root string, write, force bool) error {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return err
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
		if write && !force {
			return fmt.Errorf("refusing to write: tmux listed lines cairn could not parse, so windows or panes may be missing from the capture; "+
				"check the proposal (cairn capture %s) and re-run with --write --force to save it anyway", shellWord(name))
		}
		notes = append([]string{note}, notes...)
	}
	// capture.Session validated the id; it is safe in a path now.
	path := projectFile(dirs, p.ID)
	if wsRoot.Exists(path) {
		if write {
			return fmt.Errorf("project %s already exists (%s); pick another --id or edit it with: cairn project edit %s", p.ID, path, p.ID)
		}
		notes = append(notes, fmt.Sprintf("project %s already exists (%s); --write needs another --id", p.ID, path))
	}
	sessName := p.Session
	if sessName == "" {
		sessName = p.ID
	}
	for _, other := range snap.Sessions {
		if other.Name == sessName {
			notes = append(notes, fmt.Sprintf("a live tmux session named %q exists and is not managed by cairn; cairn will not touch it. "+
				"Rename it (tmux rename-session -t %s <new>) before running cairn apply, or adopt it (cairn adopt %s). "+
				"Closing it would kill its running programs.", sessName, shellWord(sessName), shellWord(sessName)))
			break
		}
	}
	if fi, err := wsRoot.Stat(p.Root); err != nil || !fi.IsDir() {
		notes = append(notes, fmt.Sprintf("root %s does not exist; apply skips the project until it does", p.Root))
	}
	data, err := workspace.Marshal(p)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	printProposal(out, "capture", s, p, data, notes)
	if !write {
		_, _ = fmt.Fprintf(out, "\n# save it with: cairn capture %s --id %s --write\n", shellWord(s.Name), p.ID)
		return nil
	}
	if err := wsRoot.MkdirAll(projectsDir(dirs), 0o755); err != nil {
		return err
	}
	if err := wsRoot.CreateExclusive(path, data, 0o644); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("project %s already exists (%s); pick another --id or edit it with: cairn project edit %s", p.ID, path, p.ID)
		}
		return err
	}
	if _, err := historyStore(dirs).Append("capture", fmt.Sprintf("capture session %s as project %s", s.Name, p.ID),
		[]history.FileChange{{Path: path, After: data, AfterExists: true}}); err != nil {
		return fmt.Errorf("project written to %s but not recorded in history: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "\nwrote %s\nRun: cairn plan %s\n", path, p.ID)
	return nil
}

// printProposal writes a header (naming the command verb), the TOML body (never styled) and the
// notes. Header and notes are TOML comments, so plain output can be saved
// as a project file as it is.
func printProposal(w io.Writer, verb string, s tmux.Session, p workspace.Project, data []byte, notes []string) {
	st, styled := styleFor(w)
	panes := 0
	for _, win := range s.Windows {
		panes += len(win.Panes)
	}
	head := fmt.Sprintf("# cairn %s: tmux session %q -> project %s (%s, %s)", verb, s.Name, p.ID,
		ui.Plural(len(s.Windows), "window", "windows"), ui.Plural(panes, "pane", "panes"))
	if styled {
		head = st.Bold(head)
	}
	_, _ = fmt.Fprintln(w, head)
	_, _ = fmt.Fprintln(w)
	_, _ = w.Write(data)
	if len(notes) > 0 {
		_, _ = fmt.Fprintln(w)
		for _, n := range notes {
			line := "# note: " + oneLine(n)
			if styled {
				line = st.Yellow(line)
			}
			_, _ = fmt.Fprintln(w, line)
		}
	}
}

// shellWord quotes s for a POSIX shell when it is not a plain word.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.,/+=@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// oneLine keeps a note on one comment line.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}
