package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/history"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

// wsDriver is the part of tmux.Client the workspace commands use.
type wsDriver interface {
	Snapshot(ctx context.Context) (tmux.Snapshot, error)
	NewSession(ctx context.Context, name string, w, h int, win tmux.WindowSpec) (sessionID, windowID, paneID string, err error)
	NewWindow(ctx context.Context, sessionID string, win tmux.WindowSpec) (windowID, paneID string, err error)
	SplitPane(ctx context.Context, targetPane, dir string, percent int, p tmux.PaneSpec) (paneID string, err error)
	SelectLayout(ctx context.Context, windowID, layoutString string) error
	SetOption(ctx context.Context, scope, target, name, value string) error
	RespawnPane(ctx context.Context, paneID string, argv []string) error
	SwapPanes(ctx context.Context, paneA, paneB string) error
	KillWindow(ctx context.Context, id string) error
	KillPane(ctx context.Context, id string) error
	PaneIdle(pid int) (bool, error)
	SessionIDByKey(ctx context.Context, key string) (string, bool, error)
	SwitchClient(ctx context.Context, session string) error
}

var errNoTmux = errors.New("tmux not found in PATH")

// realDriver returns the tmux client for socket ("" = tmux's default server).
func realDriver(socket string) (wsDriver, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, errNoTmux
	}
	return tmux.NewClient(socket), nil
}

// Seams for tests: the tmux driver, process replacement (attach) and the
// editor run by project edit.
var (
	newDriver   = realDriver
	execProcess = func(argv []string) error {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return errNoTmux
		}
		return syscall.Exec(path, argv, os.Environ())
	}
	runEditor = func(argv []string) error {
		c := exec.Command(argv[0], argv[1:]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
)

// wsRoot is the filesystem the workspace commands read and write.
var wsRoot = fsx.Root{Dir: "/"}

// socketName is --socket, else $CAIRN_TMUX_SOCKET, else "" (default server).
func socketName(cmd *cobra.Command) string {
	if s, _ := cmd.Flags().GetString("socket"); s != "" {
		return s
	}
	return os.Getenv("CAIRN_TMUX_SOCKET")
}

// wsState is the loaded workspace configuration. Projects holds every file
// that loaded and validated (defaulted); LoadErrs the files that did not.
type wsState struct {
	dirs     xdg.Dirs
	projects []workspace.Project
	loadErrs []workspace.LoadError
}

// loadWorkspace resolves the XDG dirs and loads every project file. A broken
// file is reported in loadErrs; the other projects stay usable.
func loadWorkspace() (*wsState, error) {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	ps, lerrs, err := workspace.Load(wsRoot, dirs)
	if err != nil {
		return nil, err
	}
	return &wsState{dirs: dirs, projects: ps, loadErrs: lerrs}, nil
}

// project returns the loaded project id.
func (ws *wsState) project(id string) (workspace.Project, bool) {
	for _, p := range ws.projects {
		if p.ID == id {
			return p, true
		}
	}
	return workspace.Project{}, false
}

// requireProject returns project id or an error explaining why it is not
// usable (a broken file, or no such project).
func (ws *wsState) requireProject(id string) (workspace.Project, error) {
	if p, ok := ws.project(id); ok {
		return p, nil
	}
	for _, le := range ws.loadErrs {
		if filepath.Base(le.File) == id+".toml" {
			return workspace.Project{}, fmt.Errorf("project %s cannot be loaded: %w", id, le)
		}
	}
	return workspace.Project{}, fmt.Errorf("project %q is not in the config; create it with: cairn project add %s --root <dir>", id, id)
}

// printLoadErrors warns about project files that could not be loaded.
func printLoadErrors(w io.Writer, lerrs []workspace.LoadError) {
	st, styled := styleFor(w)
	for _, le := range lerrs {
		if styled {
			_, _ = fmt.Fprintf(w, " %s %s\n", st.Yellow("!"), le.Error())
			continue
		}
		_, _ = fmt.Fprintf(w, "skipped %s\n", le.Error())
	}
}

func projectsDir(d xdg.Dirs) string { return filepath.Join(d.Config, "projects") }

// projectFile is the config file of id. Callers must have checked id with
// checkID, so it cannot contain '/' or "..".
func projectFile(d xdg.Dirs, id string) string { return filepath.Join(projectsDir(d), id+".toml") }

// checkID applies the project id slug rule before any path is built from id.
func checkID(id string) error {
	for _, fe := range (workspace.Project{ID: id}).Validate() {
		if fe.Key == "id" {
			return fmt.Errorf("invalid project id: %s", fe.Msg)
		}
	}
	return nil
}

func historyStore(d xdg.Dirs) history.Store {
	return history.Store{Root: wsRoot, Path: filepath.Join(d.State, "history.jsonl"), AllowedRoots: []string{projectsDir(d)}}
}

// ask prints question and reports whether the answer is yes.
func ask(in io.Reader, out io.Writer, question string) bool {
	_, _ = fmt.Fprint(out, question+" [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// inTmux reports whether cairn runs inside a tmux client.
func inTmux() bool { return os.Getenv("TMUX") != "" }
