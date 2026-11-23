package cli

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/tmux"
)

var _ wsDriver = tmux.Client{}

// fakeDriver models a tmux server well enough for the commands: sessions,
// windows and panes with ids and markers; splits insert after the target.
// Every call is recorded.
type fakeDriver struct {
	socket     string
	socketPath string // SocketPath answer
	calls      []string
	sessions   []*fSession
	seq        int
	idle       bool // PaneIdle answer for every pane
	// live, when set, is returned (as a deep copy) by Snapshot instead of
	// the modelled sessions (capture tests need pane geometry and paths);
	// SetOption and UnsetOption change its markers.
	live *tmux.Snapshot
	// failOption makes SetOption on this target fail, failUnset
	// UnsetOption; afterSet runs after every SetOption.
	failOption, failUnset string
	failKill              bool
	afterSet              func()
}

type fSession struct {
	id, name, key string
	windows       []*fWindow
}

type fWindow struct {
	id, key, name string
	panes         []*fPane
}

type fPane struct {
	id, key string
	pid     int
	dead    bool
	cmd     string
	argv    []string
	agent   string
}

func (f *fakeDriver) next() int { f.seq++; return f.seq }

func (f *fakeDriver) rec(format string, a ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

// mutations returns the recorded calls other than Snapshot and PaneIdle.
func (f *fakeDriver) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if c != "Snapshot" && !strings.HasPrefix(c, "PaneIdle") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDriver) called(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func (f *fakeDriver) newPane(p tmux.PaneSpec) *fPane {
	n := f.next()
	cmd := "sh"
	if len(p.Argv) > 0 {
		cmd = p.Argv[0]
	}
	return &fPane{id: fmt.Sprintf("%%%d", n), key: p.Key, pid: 100 + n, cmd: cmd, argv: slices.Clone(p.Argv), agent: p.Agent}
}

func (f *fakeDriver) Snapshot(context.Context) (tmux.Snapshot, error) {
	f.rec("Snapshot")
	if f.live != nil {
		out := tmux.Snapshot{Skipped: slices.Clone(f.live.Skipped)}
		for _, se := range f.live.Sessions {
			ws := make([]tmux.Window, 0, len(se.Windows))
			for _, w := range se.Windows {
				w.Panes = slices.Clone(w.Panes)
				ws = append(ws, w)
			}
			se.Windows = ws
			out.Sessions = append(out.Sessions, se)
		}
		return out, nil
	}
	var snap tmux.Snapshot
	for _, s := range f.sessions {
		ts := tmux.Session{ID: s.id, Name: s.name, Key: s.key}
		for wi, w := range s.windows {
			tw := tmux.Window{ID: w.id, Key: w.key, Name: w.name, Index: wi, W: tmux.DefaultWidth, H: tmux.DefaultHeight}
			for pi, p := range w.panes {
				tw.Panes = append(tw.Panes, tmux.Pane{ID: p.id, Key: p.key, Agent: p.agent, Index: pi, PID: p.pid, Dead: p.dead, Cmd: p.cmd})
			}
			ts.Windows = append(ts.Windows, tw)
		}
		snap.Sessions = append(snap.Sessions, ts)
	}
	return snap, nil
}

func (f *fakeDriver) NewSession(_ context.Context, name string, _, _ int, win tmux.WindowSpec) (string, string, string, error) {
	f.rec("NewSession %s %s", name, win.Key)
	project, _, _ := strings.Cut(win.Key, "/")
	s := &fSession{id: fmt.Sprintf("$%d", f.next()), name: name, key: project}
	p := f.newPane(win.PaneSpec)
	w := &fWindow{id: fmt.Sprintf("@%d", f.next()), key: win.Key, name: win.Name, panes: []*fPane{p}}
	s.windows = []*fWindow{w}
	f.sessions = append(f.sessions, s)
	return s.id, w.id, p.id, nil
}

func (f *fakeDriver) NewWindow(_ context.Context, sessionID string, win tmux.WindowSpec) (string, string, error) {
	f.rec("NewWindow %s %s", sessionID, win.Key)
	for _, s := range f.sessions {
		if s.id == sessionID {
			p := f.newPane(win.PaneSpec)
			w := &fWindow{id: fmt.Sprintf("@%d", f.next()), key: win.Key, name: win.Name, panes: []*fPane{p}}
			s.windows = append(s.windows, w)
			return w.id, p.id, nil
		}
	}
	return "", "", fmt.Errorf("no session %s", sessionID)
}

func (f *fakeDriver) SplitPane(_ context.Context, target, dir string, percent int, p tmux.PaneSpec) (string, error) {
	f.rec("SplitPane %s %s %d %s", target, dir, percent, p.Key)
	for _, s := range f.sessions {
		for _, w := range s.windows {
			for i, tp := range w.panes {
				if tp.id == target {
					np := f.newPane(p)
					w.panes = slices.Insert(w.panes, i+1, np)
					return np.id, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no pane %s", target)
}

func (f *fakeDriver) SelectLayout(_ context.Context, windowID, _ string) error {
	f.rec("SelectLayout %s", windowID)
	return nil
}

func (f *fakeDriver) SetOption(_ context.Context, scope, target, name, value string) error {
	f.rec("SetOption %s %s %s %s", scope, target, name, value)
	if f.afterSet != nil {
		defer f.afterSet()
	}
	if target == f.failOption {
		return fmt.Errorf("tmux set: no such %s %s", scope, target)
	}
	if k := f.liveKey(scope, target); k != nil {
		*k = value
	}
	if scope == "pane" {
		for _, s := range f.sessions {
			for _, w := range s.windows {
				for _, p := range w.panes {
					if p.id != target {
						continue
					}
					switch name {
					case "@cairn_p":
						p.key = value
					case tmux.AgentOption:
						p.agent = value
					}
				}
			}
		}
	}
	return nil
}

func (f *fakeDriver) UnsetOption(_ context.Context, scope, target, name string) error {
	f.rec("UnsetOption %s %s %s", scope, target, name)
	if target == f.failUnset {
		return fmt.Errorf("tmux set -u: no such %s %s", scope, target)
	}
	if k := f.liveKey(scope, target); k != nil {
		*k = ""
	}
	return nil
}

// liveKey points at the marker of a live object (nil without live or when
// the object does not exist).
func (f *fakeDriver) liveKey(scope, target string) *string {
	if f.live == nil {
		return nil
	}
	for si := range f.live.Sessions {
		s := &f.live.Sessions[si]
		if scope == "session" && s.ID == target {
			return &s.Key
		}
		for wi := range s.Windows {
			w := &s.Windows[wi]
			if scope == "window" && w.ID == target {
				return &w.Key
			}
			for pi := range w.Panes {
				if scope == "pane" && w.Panes[pi].ID == target {
					return &w.Panes[pi].Key
				}
			}
		}
	}
	return nil
}

func (f *fakeDriver) RespawnPane(_ context.Context, paneID string, argv []string) error {
	f.rec("RespawnPane %s %v", paneID, argv)
	return nil
}

func (f *fakeDriver) SelectPane(_ context.Context, id string) error {
	f.rec("SelectPane %s", id)
	return nil
}

// pane returns the fake pane marked key, or nil.
func (f *fakeDriver) pane(key string) *fPane {
	for _, s := range f.sessions {
		for _, w := range s.windows {
			for _, p := range w.panes {
				if p.key == key {
					return p
				}
			}
		}
	}
	return nil
}

func (f *fakeDriver) SwapPanes(_ context.Context, a, b string) error {
	f.rec("SwapPanes %s %s", a, b)
	return nil
}

func (f *fakeDriver) KillWindow(_ context.Context, id string) error {
	f.rec("KillWindow %s", id)
	for _, s := range f.sessions {
		s.windows = slices.DeleteFunc(s.windows, func(w *fWindow) bool { return w.id == id })
	}
	return nil
}

func (f *fakeDriver) KillPane(_ context.Context, id string) error {
	f.rec("KillPane %s", id)
	if f.failKill {
		return fmt.Errorf("tmux kill-pane: %s: simulated failure", id)
	}
	for _, s := range f.sessions {
		for _, w := range s.windows {
			w.panes = slices.DeleteFunc(w.panes, func(p *fPane) bool { return p.id == id })
		}
	}
	return nil
}

func (f *fakeDriver) PaneIdle(pid int) (bool, error) {
	f.rec("PaneIdle %d", pid)
	return f.idle, nil
}

func (f *fakeDriver) SessionIDByKey(_ context.Context, key string) (string, bool, error) {
	f.rec("SessionIDByKey %s", key)
	for _, s := range f.sessions {
		if s.key == key {
			return s.id, true, nil
		}
	}
	return "", false, nil
}

func (f *fakeDriver) SocketPath(context.Context) (string, error) {
	f.rec("SocketPath")
	return f.socketPath, nil
}

func (f *fakeDriver) SwitchClientHere(_ context.Context, session, tmuxEnv, tmuxPane string) error {
	f.rec("SwitchClientHere %s TMUX=%s TMUX_PANE=%s", session, tmuxEnv, tmuxPane)
	return nil
}

// session returns the fake session marked key.
func (f *fakeDriver) session(t *testing.T, key string) *fSession {
	t.Helper()
	for _, s := range f.sessions {
		if s.key == key {
			return s
		}
	}
	t.Fatalf("no session %s; calls: %v", key, f.calls)
	return nil
}

// wsTestEnv isolates the workspace commands: temp XDG dirs, no TMUX in the
// environment, a fake tmux driver and a recording exec.
func wsTestEnv(t *testing.T) (string, *fakeDriver) {
	t.Helper()
	dir := hostTestEnv(t)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("CAIRN_TMUX_SOCKET", "")
	fd := &fakeDriver{}
	oldDriver, oldExec, oldEditor, oldInteractive := newDriver, execProcess, runEditor, interactive
	t.Cleanup(func() { newDriver, execProcess, runEditor, interactive = oldDriver, oldExec, oldEditor, oldInteractive })
	newDriver = func(socket string) (wsDriver, error) {
		fd.socket = socket
		return fd, nil
	}
	execProcess = func(argv []string) error {
		fd.rec("exec %s", strings.Join(argv, " "))
		return nil
	}
	runEditor = func([]string) error {
		t.Fatal("editor not expected")
		return nil
	}
	return dir, fd
}

// runSplit runs cairn with stdout and stderr captured separately.
func runSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewRootCmd()
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return o.String(), e.String(), err
}

// useRealTmux switches the commands to a real tmux driver on the private
// socket (also exported as CAIRN_TMUX_SOCKET) and fails the test loudly if a
// command ever resolves an empty or non-test socket, i.e. would reach the
// default tmux server.
func useRealTmux(t *testing.T, socket string) {
	t.Helper()
	if !strings.HasPrefix(socket, "cairn-test-") {
		t.Fatalf("real tmux tests need a cairn-test-* socket, got %q", socket)
	}
	t.Setenv("CAIRN_TMUX_SOCKET", socket)
	newDriver = func(s string) (wsDriver, error) {
		if s != socket {
			t.Fatalf("real tmux test would use socket %q instead of %q (the default server?)", s, socket)
		}
		return realDriver(s)
	}
}
