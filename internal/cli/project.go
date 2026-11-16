package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Add, list, edit, archive and remove workspace projects"}
	cmd.AddCommand(newProjectAddCmd(), newProjectLsCmd(), newProjectEditCmd(),
		newProjectStatusCmd("archive", workspace.StatusArchived, "Archive a project (cairn stops managing it; tmux is not touched)"),
		newProjectStatusCmd("resume", workspace.StatusActive, "Make an archived or paused project active again"),
		newProjectRmCmd())
	return cmd
}

func newProjectAddCmd() *cobra.Command {
	var root, tmpl, name string
	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Create a project config from a template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if err := checkID(id); err != nil {
				return err
			}
			dirs, err := xdg.Resolve(os.Getenv)
			if err != nil {
				return err
			}
			if root == "" {
				if root, err = os.Getwd(); err != nil {
					return err
				}
			} else if !strings.HasPrefix(root, "~") {
				if root, err = filepath.Abs(root); err != nil {
					return err
				}
			}
			if name == "" {
				name = id
			}
			// build returns a fresh, undefaulted project: one copy is
			// written, another is defaulted for validation (E3).
			build := func() (workspace.Project, error) {
				p, ok := workspace.Template(tmpl)
				if !ok {
					return p, fmt.Errorf("unknown template %q (one of: %s)", tmpl, strings.Join(workspace.TemplateNames(), ", "))
				}
				p.ID, p.Name, p.Root = id, name, root
				return p, nil
			}
			p, err := build()
			if err != nil {
				return err
			}
			v, _ := build()
			v.ApplyDefaults(dirs.Home)
			if errs := v.Validate(); len(errs) > 0 {
				msgs := make([]string, len(errs))
				for i, e := range errs {
					msgs[i] = e.Error()
				}
				return fmt.Errorf("invalid project: %s", strings.Join(msgs, "; "))
			}
			path := projectFile(dirs, id)
			data, err := workspace.Marshal(p)
			if err != nil {
				return err
			}
			if err := wsRoot.MkdirAll(projectsDir(dirs), 0o755); err != nil {
				return err
			}
			// Exclusive: a concurrent add of the same id cannot overwrite.
			if err := wsRoot.CreateExclusive(path, data, 0o644); err != nil {
				if errors.Is(err, fs.ErrExist) {
					return fmt.Errorf("project %s already exists (%s); edit it with: cairn project edit %s", id, path, id)
				}
				return err
			}
			if _, err := historyStore(dirs).Append("project.add", "add project "+id,
				[]history.FileChange{{Path: path, After: data, AfterExists: true}}); err != nil {
				return fmt.Errorf("project written to %s but not recorded in history: %w", path, err)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "added project %s (template %s): %s\n", id, tmpl, path)
			if fi, err := wsRoot.Stat(v.Root); err != nil || !fi.IsDir() {
				_, _ = fmt.Fprintf(out, "note: root %s does not exist; apply skips the project until it does\n", v.Root)
			}
			_, _ = fmt.Fprintf(out, "Next: cairn open %s\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "project root directory (default: the current directory)")
	cmd.Flags().StringVar(&tmpl, "template", "shell", "template: "+strings.Join(workspace.TemplateNames(), ", "))
	cmd.Flags().StringVar(&name, "name", "", "display name (default: the id)")
	return cmd
}

func newProjectLsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List projects with their live tmux state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws, err := loadWorkspace()
			if err != nil {
				return err
			}
			printLoadErrors(cmd.ErrOrStderr(), ws.loadErrs)
			// Live state is best effort: without tmux (or with a broken
			// server) it shows "?".
			live, known := map[string]bool{}, false
			if drv, err := newDriver(socketName(cmd)); err == nil {
				if snap, err := drv.Snapshot(ctxOf(cmd)); err == nil {
					known = true
					for _, s := range snap.Sessions {
						if s.Key != "" {
							live[s.Key] = true
						}
					}
				}
			}
			out := cmd.OutOrStdout()
			st, styled := styleFor(out)
			n := 0
			for _, p := range ws.projects {
				if p.Status == workspace.StatusArchived && !all {
					continue
				}
				if n == 0 {
					_, _ = fmt.Fprintf(out, "%s %s %s %s %s\n", ui.Pad("ID", 16), ui.Pad("STATUS", 9), ui.Pad("LIVE", 8), ui.Pad("WINDOWS", 8), "ROOT")
				}
				n++
				state := "?"
				if known {
					state = "stopped"
					if live[p.ID] {
						state = "running"
					}
				}
				status := ui.Pad(p.Status, 9)
				stateCol := ui.Pad(state, 8)
				if styled {
					if p.Status != workspace.StatusActive {
						status = st.Dim(status)
					}
					if state == "running" {
						stateCol = st.Green(stateCol)
					} else {
						stateCol = st.Dim(stateCol)
					}
				}
				_, _ = fmt.Fprintf(out, "%s %s %s %s %s\n", ui.Pad(p.ID, 16), status, stateCol, ui.Pad(fmt.Sprint(len(p.Windows)), 8), p.Root)
			}
			if n == 0 {
				_, _ = fmt.Fprintln(out, "no projects; add one: cairn project add <id> --root <dir>")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include archived projects")
	return cmd
}

// readProjectFile returns the dirs, the path and the content of project id.
// With forWrite, a project file that is a symlink is refused: cairn would
// replace the link with a regular file.
func readProjectFile(id string, forWrite bool) (xdg.Dirs, string, []byte, error) {
	if err := checkID(id); err != nil {
		return xdg.Dirs{}, "", nil, err
	}
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return dirs, "", nil, err
	}
	path := projectFile(dirs, id)
	if fi, err := wsRoot.Lstat(path); forWrite && err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return dirs, path, nil, fmt.Errorf("%s is a symlink; edit the target directly", path)
	}
	data, err := wsRoot.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return dirs, path, nil, fmt.Errorf("project %q is not in the config (%s); create it with: cairn project add %s", id, path, id)
	}
	return dirs, path, data, err
}

// checkProjectData lists the problems of a project file's content: decode
// errors, an id that differs from the file name, and validation errors by
// key.
func checkProjectData(id string, data []byte, home string) []string {
	p, err := workspace.Decode(data)
	if err != nil {
		return []string{err.Error()}
	}
	var probs []string
	if p.ID != id {
		probs = append(probs, fmt.Sprintf("id: %q must equal the file name %q", p.ID, id))
	}
	p.ApplyDefaults(home)
	for _, fe := range p.Validate() {
		probs = append(probs, fe.Error())
	}
	return probs
}

// editorArgv is $VISUAL, else $EDITOR, else vi, split on whitespace.
func editorArgv() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(env)); len(f) > 0 {
			return f
		}
	}
	return []string{"vi"}
}

func newProjectEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <id>",
		Short: "Edit a project file in $VISUAL/$EDITOR and validate it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			dirs, path, before, err := readProjectFile(id, true)
			if err != nil {
				return err
			}
			argv := append(editorArgv(), path)
			edErr := runEditor(argv)
			after, rerr := wsRoot.ReadFile(path)
			if bytes.Equal(after, before) && rerr == nil {
				if edErr != nil {
					return fmt.Errorf("editor %s: %w", argv[0], edErr)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no changes to %s\n", path)
				return nil
			}
			var probs []string
			switch {
			case edErr != nil:
				probs = []string{fmt.Sprintf("editor %s failed: %v", argv[0], edErr)}
			case rerr != nil:
				probs = []string{rerr.Error()}
			default:
				probs = checkProjectData(id, after, dirs.Home)
			}
			if len(probs) > 0 {
				if err := wsRoot.WriteAtomic(path, before, 0o644); err != nil {
					return fmt.Errorf("%s is invalid and restoring the previous content failed: %w", path, err)
				}
				w := cmd.ErrOrStderr()
				_, _ = fmt.Fprintf(w, "%s is invalid; the previous content was restored:\n", path)
				for _, p := range probs {
					_, _ = fmt.Fprintf(w, "  %s: %s\n", path, p)
				}
				return &ExitError{Code: 1}
			}
			// Rewrite through WriteAtomic so the saved content is fsynced.
			if err := wsRoot.WriteAtomic(path, after, 0o644); err != nil {
				return err
			}
			if _, err := historyStore(dirs).Append("project.edit", "edit project "+id, []history.FileChange{
				{Path: path, Before: before, After: after, BeforeExists: true, AfterExists: true}}); err != nil {
				return fmt.Errorf("saved %s but not recorded in history: %w", path, err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "saved %s\nRun: cairn apply %s\n", path, id)
			return nil
		},
	}
}

var statusLineRe = regexp.MustCompile(`^\s*status\s*=`)

// rewriteStatus sets the top-level status key in a project file's text,
// keeping everything else (comments, order) as it is.
func rewriteStatus(data []byte, status string) []byte {
	lines := strings.Split(string(data), "\n")
	header := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			header = i
			break
		}
	}
	line := fmt.Sprintf("status = %q", status)
	last := -1
	for i := 0; i < header; i++ {
		if statusLineRe.MatchString(lines[i]) {
			lines[i] = line
			return []byte(strings.Join(lines, "\n"))
		}
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			last = i
		}
	}
	lines = append(lines[:last+1], append([]string{line}, lines[last+1:]...)...)
	return []byte(strings.Join(lines, "\n"))
}

// withStatus returns data with status set: a textual edit when it decodes
// to exactly the old project with the new status, else a re-marshal.
func withStatus(data []byte, p workspace.Project, status string) ([]byte, error) {
	want := p
	want.Status = status
	nu := rewriteStatus(data, status)
	if q, err := workspace.Decode(nu); err == nil && reflect.DeepEqual(q, want) {
		return nu, nil
	}
	if status == workspace.StatusActive {
		want.Status = ""
	}
	return workspace.Marshal(want)
}

func newProjectStatusCmd(use, status, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			dirs, path, data, err := readProjectFile(id, true)
			if err != nil {
				return err
			}
			p, err := workspace.Decode(data)
			if err != nil {
				return fmt.Errorf("%s is invalid (%v); fix it with: cairn project edit %s", path, err, id)
			}
			cur := p.Status
			if cur == "" {
				cur = workspace.StatusActive
			}
			out := cmd.OutOrStdout()
			if cur == status {
				_, _ = fmt.Fprintf(out, "project %s is already %s\n", id, status)
				return nil
			}
			nu, err := withStatus(data, p, status)
			if err != nil {
				return err
			}
			if err := wsRoot.WriteAtomic(path, nu, 0o644); err != nil {
				return err
			}
			if _, err := historyStore(dirs).Append("project."+use, use+" project "+id, []history.FileChange{
				{Path: path, Before: data, After: nu, BeforeExists: true, AfterExists: true}}); err != nil {
				return fmt.Errorf("saved %s but not recorded in history: %w", path, err)
			}
			if status == workspace.StatusArchived {
				_, _ = fmt.Fprintf(out, "archived project %s; its tmux session (if any) is left as it is\n", id)
				return nil
			}
			_, _ = fmt.Fprintf(out, "project %s is active\nRun: cairn apply %s\n", id, id)
			return nil
		},
	}
}

func newProjectRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove an archived project's config (undo brings it back)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			dirs, path, data, err := readProjectFile(id, false)
			if err != nil {
				return err
			}
			p, err := workspace.Decode(data)
			if err != nil {
				return fmt.Errorf("%s is invalid (%v); fix it with: cairn project edit %s", path, err, id)
			}
			if p.Status != workspace.StatusArchived {
				st := p.Status
				if st == "" {
					st = workspace.StatusActive
				}
				return fmt.Errorf("project %s is %s; only archived projects can be removed: cairn project archive %s", id, st, id)
			}
			out := cmd.OutOrStdout()
			if !yes && !interactive(cmd.InOrStdin()) {
				return fmt.Errorf("removing %s needs confirmation: re-run with --yes", path)
			}
			if !yes && !ask(cmd.InOrStdin(), out, "Remove "+path+"?") {
				_, _ = fmt.Fprintln(out, "nothing removed")
				return nil
			}
			// History first: the content is kept before the file goes.
			if _, err := historyStore(dirs).Append("project.rm", "remove project "+id, []history.FileChange{
				{Path: path, Before: data, BeforeExists: true}}); err != nil {
				return err
			}
			if err := wsRoot.Remove(path); err != nil {
				return err
			}
			if err := syncDir(wsRoot.Path(projectsDir(dirs))); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "removed project %s (cairn undo brings it back)\n", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// syncDir fsyncs a directory so a removal in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
