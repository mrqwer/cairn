package tmux

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Pane is one tmux pane. Key is its @cairn_p marker ("" when unmanaged).
type Pane struct {
	ID, Key                     string
	Index, PID, Left, Top, W, H int
	Dead                        bool
	DeadStatus                  int
	Cwd, Cmd                    string
}

// Window is one tmux window. Key is its @cairn_w marker ("" when unmanaged).
type Window struct {
	ID, Key, Name string
	Index, W, H   int
	Panes         []Pane
}

// Session is one tmux session. Key is its @cairn_s marker ("" when unmanaged).
type Session struct {
	ID, Name, Key string
	Attached      int
	Windows       []Window
}

// Snapshot is the state of a tmux server.
type Snapshot struct {
	Sessions []Session
}

const (
	sessionFormat = "#{session_id}\t#{session_name}\t#{@cairn_s}\t#{session_attached}"
	windowFormat  = "#{session_id}\t#{window_id}\t#{window_index}\t#{window_name}\t#{@cairn_w}\t#{window_width}\t#{window_height}"
	paneFormat    = "#{window_id}\t#{pane_id}\t#{pane_index}\t#{@cairn_p}\t#{pane_pid}\t#{pane_left}\t#{pane_top}\t#{pane_width}\t#{pane_height}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_current_path}\t#{pane_current_command}"
)

// noServer reports whether tmux output says there is no server to talk to.
func noServer(out string) bool {
	return strings.Contains(out, "no server running") || strings.Contains(out, "error connecting")
}

// list runs a list command; ok is false when no server is running.
func (c Client) list(ctx context.Context, args ...string) (lines []string, ok bool, err error) {
	out, err := c.output(ctx, args...)
	if err != nil {
		if noServer(out) {
			return nil, false, nil
		}
		return nil, false, err
	}
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, true, nil
}

// markerKey keeps v only when it has exactly slashes '/' characters: tmux
// falls back from pane to window to session options in formats, so a value
// with the wrong shape belongs to an outer level.
func markerKey(v string, slashes int) string {
	if v == "" || strings.Count(v, "/") != slashes {
		return ""
	}
	return v
}

// Snapshot reads all sessions, windows and panes. A server that is not
// running yields an empty snapshot and no error.
func (c Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var snap Snapshot
	sl, ok, err := c.list(ctx, "list-sessions", "-F", sessionFormat)
	if err != nil || !ok {
		return snap, err
	}
	wl, ok, err := c.list(ctx, "list-windows", "-a", "-F", windowFormat)
	if err != nil || !ok {
		return Snapshot{}, err
	}
	pl, ok, err := c.list(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil || !ok {
		return Snapshot{}, err
	}

	panes := map[string][]Pane{}
	for _, l := range pl {
		f, err := fields(l, 13)
		if err != nil {
			return Snapshot{}, err
		}
		if f[4] == "" { // a dead pane has no pid
			f[4] = "0"
		}
		n, err := ints(f, 2, 4, 5, 6, 7, 8)
		if err != nil {
			return Snapshot{}, fmt.Errorf("tmux: pane line %q: %w", l, err)
		}
		status := 0
		if f[10] != "" {
			if status, err = strconv.Atoi(f[10]); err != nil {
				return Snapshot{}, fmt.Errorf("tmux: pane line %q: %w", l, err)
			}
		}
		panes[f[0]] = append(panes[f[0]], Pane{
			ID: f[1], Key: markerKey(f[3], 2), Index: n[0], PID: n[1],
			Left: n[2], Top: n[3], W: n[4], H: n[5],
			Dead: f[9] == "1", DeadStatus: status, Cwd: f[11], Cmd: f[12],
		})
	}

	windows := map[string][]Window{}
	for _, l := range wl {
		f, err := fields(l, 7)
		if err != nil {
			return Snapshot{}, err
		}
		n, err := ints(f, 2, 5, 6)
		if err != nil {
			return Snapshot{}, fmt.Errorf("tmux: window line %q: %w", l, err)
		}
		ps := panes[f[1]]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Index < ps[j].Index })
		windows[f[0]] = append(windows[f[0]], Window{
			ID: f[1], Key: markerKey(f[4], 1), Name: f[3], Index: n[0], W: n[1], H: n[2], Panes: ps,
		})
	}

	for _, l := range sl {
		f, err := fields(l, 4)
		if err != nil {
			return Snapshot{}, err
		}
		n, err := ints(f, 3)
		if err != nil {
			return Snapshot{}, fmt.Errorf("tmux: session line %q: %w", l, err)
		}
		ws := windows[f[0]]
		sort.Slice(ws, func(i, j int) bool { return ws[i].Index < ws[j].Index })
		snap.Sessions = append(snap.Sessions, Session{
			ID: f[0], Name: f[1], Key: markerKey(f[2], 0), Attached: n[0], Windows: ws,
		})
	}
	return snap, nil
}

// fields splits a tab-separated line into exactly n fields; extra tabs end up
// in the last field.
func fields(line string, n int) ([]string, error) {
	f := strings.SplitN(line, "\t", n)
	if len(f) != n {
		return nil, fmt.Errorf("tmux: want %d fields, got %d in %q", n, len(f), line)
	}
	return f, nil
}

func ints(f []string, idx ...int) ([]int, error) {
	out := make([]int, len(idx))
	for i, k := range idx {
		v, err := strconv.Atoi(f[k])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
