package tmux

import (
	"context"
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
	// Skipped holds list lines that could not be parsed (e.g. a tab or
	// newline in a foreign pane's path); they are left out, not fatal.
	Skipped []string
}

const (
	sessionFormat = "#{session_id}\t#{session_name}\t#{@cairn_s}\t#{session_attached}"
	windowFormat  = "#{session_id}\t#{window_id}\t#{window_index}\t#{window_name}\t#{@cairn_w}\t#{window_width}\t#{window_height}"
	paneFormat    = "#{window_id}\t#{pane_id}\t#{pane_index}\t#{@cairn_p}\t#{pane_pid}\t#{pane_left}\t#{pane_top}\t#{pane_width}\t#{pane_height}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_dead_signal}\t#{pane_current_path}\t#{pane_current_command}"
)

// noServer reports whether tmux output says there is no server to talk to.
func noServer(out string) bool {
	if strings.Contains(out, "no server running") {
		return true
	}
	return strings.Contains(out, "error connecting") &&
		(strings.Contains(out, "No such file or directory") || strings.Contains(out, "Connection refused"))
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
		f, n, ok := parseLine(l, 14, 2, 4, 5, 6, 7, 8, 10, 11)
		if !ok {
			snap.Skipped = append(snap.Skipped, l)
			continue
		}
		status := n[6]
		if status == 0 && n[7] > 0 {
			status = 128 + n[7] // killed by a signal: tmux has no status, report it as the shell does
		}
		panes[f[0]] = append(panes[f[0]], Pane{
			ID: f[1], Key: markerKey(f[3], 2), Index: n[0], PID: n[1],
			Left: n[2], Top: n[3], W: n[4], H: n[5],
			Dead: f[9] == "1", DeadStatus: status, Cwd: f[12], Cmd: f[13],
		})
	}

	windows := map[string][]Window{}
	for _, l := range wl {
		f, n, ok := parseLine(l, 7, 2, 5, 6)
		if !ok {
			snap.Skipped = append(snap.Skipped, l)
			continue
		}
		ps := panes[f[1]]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Index < ps[j].Index })
		windows[f[0]] = append(windows[f[0]], Window{
			ID: f[1], Key: markerKey(f[4], 1), Name: f[3], Index: n[0], W: n[1], H: n[2], Panes: ps,
		})
	}

	for _, l := range sl {
		f, n, ok := parseLine(l, 4, 3)
		if !ok {
			snap.Skipped = append(snap.Skipped, l)
			continue
		}
		ws := windows[f[0]]
		sort.Slice(ws, func(i, j int) bool { return ws[i].Index < ws[j].Index })
		snap.Sessions = append(snap.Sessions, Session{
			ID: f[0], Name: f[1], Key: markerKey(f[2], 0), Attached: n[0], Windows: ws,
		})
	}
	return snap, nil
}

// parseLine splits a tab-separated line into exactly n fields and parses the
// fields at idx as integers (an empty field is 0: tmux prints no pid or
// status for some panes). ok is false for a malformed line.
func parseLine(line string, n int, idx ...int) (f []string, nums []int, ok bool) {
	f = strings.Split(line, "\t")
	if len(f) != n {
		return nil, nil, false
	}
	nums = make([]int, len(idx))
	for i, k := range idx {
		if f[k] == "" {
			continue
		}
		v, err := strconv.Atoi(f[k])
		if err != nil {
			return nil, nil, false
		}
		nums[i] = v
	}
	return f, nums, true
}
