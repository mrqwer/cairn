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
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/agents/task"
	"github.com/mrqwer/cairn/internal/config"
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
	UnsetOption(ctx context.Context, scope, target, name string) error
	RespawnPane(ctx context.Context, paneID string, argv []string) error
	SwapPanes(ctx context.Context, paneA, paneB string) error
	KillWindow(ctx context.Context, id string) error
	KillPane(ctx context.Context, id string) error
	PaneIdle(pid int) (bool, error)
	SessionIDByKey(ctx context.Context, key string) (string, bool, error)
	SocketPath(ctx context.Context) (string, error)
	SwitchClientHere(ctx context.Context, sessionID, tmuxEnv, tmuxPane string) error
	SelectPane(ctx context.Context, paneID string) error
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
	// interactive reports whether answers can be read from in (a terminal).
	interactive = func(in io.Reader) bool {
		f, ok := in.(*os.File)
		if !ok {
			return false
		}
		return isTerminal(f)
	}
)

// isTerminal reports whether f is a terminal: /dev/null and other character
// devices answer no, only a tty accepts the termios query.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

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

// historyStore is the config history. Undo may write project files and the
// curated overlays of agent conversations (names, tags, archive, removal).
func historyStore(d xdg.Dirs) history.Store {
	roots := []string{projectsDir(d), agentStore(d).CuratedDir(), task.Dir(d), knowledgeDir(d, "")}
	// Finish records are history events too: their directory is allowed
	// as configured now (an unreadable configuration allows the default only).
	if cfg, _, err := config.Load(wsRoot, d, os.Getenv); err == nil && cfg.Knowledge.Root != "" && len(cfg.Validate()) == 0 {
		roots = append(roots, knowledgeDir(d, cfg.Knowledge.Root))
	}
	return history.Store{Root: wsRoot, Path: filepath.Join(d.State, "history.jsonl"), AllowedRoots: roots}
}

// prompting is true while ask waits for an answer; main does not announce
// "finishing the current step" for an interrupt that lands on a prompt.
var prompting atomic.Bool

// Prompting reports whether a confirmation prompt is waiting for input.
func Prompting() bool { return prompting.Load() }

// exitInterrupted is the exit code of a prompt abandoned with Ctrl-C (128+SIGINT).
const exitInterrupted = 130

// ask prints question and reports whether the answer is yes. When ctx is
// cancelled (Ctrl-C, SIGTERM) before an answer arrives it returns an
// ExitError with code 130 and nothing is applied; the goroutine still
// reading in is left behind and ends with the process.
func ask(ctx context.Context, in io.Reader, out io.Writer, question string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, &ExitError{Code: exitInterrupted}
	}
	_, _ = fmt.Fprint(out, question+" [y/N] ")
	prompting.Store(true)
	defer prompting.Store(false)
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		lines <- line
	}()
	select {
	case <-ctx.Done():
		_, _ = fmt.Fprintln(out, "\ninterrupted, nothing changed")
		return false, &ExitError{Code: exitInterrupted}
	case line := <-lines:
		ans := strings.ToLower(strings.TrimSpace(line))
		return ans == "y" || ans == "yes", nil
	}
}

// mayBeMidApply reports whether c, with its flags as parsed, can be in the
// middle of changing the system or the tmux server.
func mayBeMidApply(c *cobra.Command) bool {
	if c == nil {
		return false
	}
	switch c.CommandPath() {
	case "cairn up", "cairn apply", "cairn open", "cairn adopt", "cairn undo",
		"cairn history restore", "cairn host apply", "cairn host uninstall", "cairn host repair",
		"cairn tmux service install", "cairn tmux service uninstall", "cairn init",
		"cairn agents new", "cairn agents resume", "cairn agents archive", "cairn agents rm",
		"cairn task new", "cairn task open", "cairn task recover", "cairn task finish":
		return true
	case "cairn capture":
		w, _ := c.Flags().GetBool("write")
		return w
	}
	return false
}

// running is the command main is executing (set before its RunE).
var running atomic.Pointer[cobra.Command]

// MayBeMidApply reports whether the running command is one that can be
// interrupted in the middle of applying changes, so that main may say it is
// finishing the current step. Prompts and read-only commands are not.
func MayBeMidApply() bool { return !Prompting() && mayBeMidApply(running.Load()) }

// tmuxClientSocket is the socket path of the tmux client cairn runs in
// (TMUX is "<socket path>,<pid>,<session>"), or "" outside tmux.
func tmuxClientSocket() string {
	path, _, _ := strings.Cut(os.Getenv("TMUX"), ",")
	return path
}
