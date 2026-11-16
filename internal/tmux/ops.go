package tmux

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PaneSpec describes a pane to create. Argv of length 1 runs as a shell
// string; longer argv runs directly without a shell. Panes with Argv (or with
// RemainOnExit) keep their dead pane when the program exits.
type PaneSpec struct {
	Key, Cwd     string
	Argv         []string
	Env          map[string]string
	RemainOnExit bool
}

// WindowSpec describes a window and its first pane. Key is
// "<project>/<window key>"; its project part becomes the session marker.
type WindowSpec struct {
	Key, Name string
	PaneSpec  PaneSpec
}

// Default size of a new session when the caller passes 0.
const (
	DefaultWidth  = 200
	DefaultHeight = 50
)

// escSemi keeps a trailing ';' literal. tmux treats an argument ending in
// ';' as a command separator and drops the ';'; when the remaining text ends
// in '\' that one '\' becomes the ';' instead (tmux 3.8, verified: "a\;" ->
// "a;", "a\;" -> "a\;"). So inserting one '\' before a trailing ';' always
// round-trips, including values that already end in "\;".
func escSemi(s string) string {
	if !strings.HasSuffix(s, ";") {
		return s
	}
	return s[:len(s)-1] + `\;`
}

// escFormat is for values tmux expands as formats (-n, -s, -c): '#' is
// doubled, then the trailing ';' is escaped. Pane argv and -e values are not
// expanded (verified on tmux 3.8) and only get escSemi.
func escFormat(s string) string {
	return escSemi(strings.ReplaceAll(s, "#", "##"))
}

// checkArgv rejects empty argv elements.
func checkArgv(argv []string) error {
	for i, a := range argv {
		if a == "" {
			return fmt.Errorf("tmux: argv element %d is empty", i)
		}
	}
	return nil
}

func checkPane(p PaneSpec) error {
	if p.Key != "" && !threeParts(p.Key) {
		return fmt.Errorf("tmux: pane key %q must be <project>/<window>/<n>", p.Key)
	}
	return checkArgv(p.Argv)
}

// threeParts reports whether key is "a/b/c" with no empty part.
func threeParts(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// checkWindow requires a managed window key "<project>/<window>".
func checkWindow(win WindowSpec) error {
	project, wkey, ok := strings.Cut(win.Key, "/")
	if !ok || project == "" || wkey == "" || strings.Contains(wkey, "/") {
		return fmt.Errorf("tmux: window key %q must be <project>/<window>", win.Key)
	}
	return checkPane(win.PaneSpec)
}

// paneArgs returns "-c cwd -e K=V..." for p, escaped for tmux. The command
// is not part of it: panes are created running the default shell and started
// with startCommand once their options and markers are set (see there).
func paneArgs(p PaneSpec) []string {
	var a []string
	if p.Cwd != "" {
		a = append(a, "-c", escFormat(p.Cwd))
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", escSemi(k+"="+p.Env[k]))
	}
	return a
}

// startCommand runs p's command in the pane id. The pane is created without
// the command because a program that exits at once would take its pane (and
// window) away before remain-on-exit and the markers are set, which made
// apply fail with "no such pane". Created as the default shell, the pane
// survives until it is configured; respawn-pane -k then replaces the shell
// with the command, and the dead pane stays (remain-on-exit) when it exits.
func (c Client) startCommand(ctx context.Context, id string, p PaneSpec) error {
	if len(p.Argv) == 0 {
		return nil
	}
	args := append([]string{"respawn-pane", "-k", "-t", id}, paneArgs(p)...)
	args = append(args, "--")
	for _, v := range p.Argv {
		args = append(args, escSemi(v))
	}
	return c.run(ctx, args...)
}

// ids splits -P -F output into exactly n ids.
func ids(out string, n int) ([]string, error) {
	f := strings.Fields(out)
	if len(f) != n {
		return nil, fmt.Errorf("tmux: want %d ids, got %q", n, out)
	}
	return f, nil
}

// remainOnExit keeps the pane after its program exits; it runs right after
// the pane is created, before the command is started (see startCommand).
func (c Client) remainOnExit(ctx context.Context, id string, p PaneSpec) error {
	if len(p.Argv) == 0 && !p.RemainOnExit {
		return nil
	}
	return c.SetOption(ctx, "pane", id, "remain-on-exit", "on")
}

func (c Client) paneMarker(ctx context.Context, id string, p PaneSpec) error {
	if p.Key == "" {
		return nil
	}
	return c.SetOption(ctx, "pane", id, "@cairn_p", p.Key)
}

func (c Client) setupWindow(ctx context.Context, id string, win WindowSpec) error {
	if err := c.SetOption(ctx, "window", id, "@cairn_w", win.Key); err != nil {
		return err
	}
	return c.SetOption(ctx, "window", id, "automatic-rename", "off")
}

// NewSession creates a detached session of w x h (DefaultWidth x
// DefaultHeight when 0) with its first window and pane, and sets the markers.
// win.Key must be "<project>/<window>"; the project part becomes @cairn_s.
func (c Client) NewSession(ctx context.Context, name string, w, h int, win WindowSpec) (sessionID, windowID, paneID string, err error) {
	if err := checkWindow(win); err != nil {
		return "", "", "", err
	}
	if w <= 0 {
		w = DefaultWidth
	}
	if h <= 0 {
		h = DefaultHeight
	}
	// Snapshot of the session ids before the call: if ctx is cancelled while
	// new-session runs, the session may exist without a marker, and this is
	// how it is told apart from one that was there before.
	before, err := c.sessionIDs(ctx)
	if err != nil {
		return "", "", "", err
	}
	started := time.Now().Unix()
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}",
		"-s", escFormat(name), "-x", strconv.Itoa(w), "-y", strconv.Itoa(h), "-n", escFormat(win.Name)}
	out, err := c.output(ctx, append(args, paneArgs(win.PaneSpec)...)...)
	if err != nil {
		if ctx.Err() != nil {
			err = c.cleanupInterruptedNewSession(context.WithoutCancel(ctx), name, before, started, err)
		}
		return "", "", "", err
	}
	id, err := ids(out, 3)
	if err != nil {
		return "", "", "", err
	}
	sessionID, windowID, paneID = id[0], id[1], id[2]
	// The session is provably ours (its id came from -P). The setup steps run
	// detached from cancellation so Ctrl-C cannot stop them half way; if one
	// fails, or ctx was cancelled meanwhile, the session is killed so no
	// unmarked session is left behind.
	setupCtx := context.WithoutCancel(ctx)
	err = c.remainOnExit(setupCtx, paneID, win.PaneSpec)
	if err == nil {
		project, _, _ := strings.Cut(win.Key, "/")
		err = c.SetOption(setupCtx, "session", sessionID, "@cairn_s", project)
	}
	if err == nil {
		err = c.setupWindow(setupCtx, windowID, win)
	}
	if err == nil {
		err = c.paneMarker(setupCtx, paneID, win.PaneSpec)
	}
	if err == nil {
		err = c.startCommand(setupCtx, paneID, win.PaneSpec)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = c.KillSession(setupCtx, sessionID)
		return "", "", "", err
	}
	return sessionID, windowID, paneID, nil
}

// sessionIDs returns the ids of all sessions; none when no server runs.
func (c Client) sessionIDs(ctx context.Context) (map[string]bool, error) {
	lines, _, err := c.list(ctx, "list-sessions", "-F", "#{session_id}")
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, l := range lines {
		ids[strings.TrimSpace(l)] = true
	}
	return ids, nil
}

// cleanupInterruptedNewSession handles a new-session call that ended with ctx
// done: tmux may or may not have created the session before the client was
// stopped. A session named name is killed only when it can be shown to come
// from this call: its id is not in before, it was created at or after
// started, it has no marker and holds a single window. Anything else (a
// session that was already there, a marked one) is left alone. cause is
// returned, extended when the outcome is uncertain so the user knows.
func (c Client) cleanupInterruptedNewSession(ctx context.Context, name string, before map[string]bool, started int64, cause error) error {
	lines, _, err := c.list(ctx, "list-sessions", "-F", "#{session_id}\t#{session_name}\t#{@cairn_s}\t#{session_created}\t#{session_windows}")
	if err != nil {
		return fmt.Errorf("%w; could not check whether tmux left an unmarked session %q behind (%v): look with tmux list-sessions", cause, name, err)
	}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 5 || f[1] != name {
			continue
		}
		created, cerr := strconv.ParseInt(f[3], 10, 64)
		if before[f[0]] || f[2] != "" || cerr != nil || created < started || f[4] != "1" {
			return fmt.Errorf("%w; a session named %q exists and was not removed because it cannot be shown to be the one this call created", cause, name)
		}
		if kerr := c.KillSession(ctx, f[0]); kerr != nil {
			return fmt.Errorf("%w; could not remove the unmarked session %q (%s) it left behind (%v): remove it with tmux kill-session", cause, name, f[0], kerr)
		}
		return cause
	}
	return cause
}

// NewWindow creates a window at the end of sessionID with its first pane and
// sets the window and pane markers.
// win.Key must be "<project>/<window>".
func (c Client) NewWindow(ctx context.Context, sessionID string, win WindowSpec) (windowID, paneID string, err error) {
	if err := checkWindow(win); err != nil {
		return "", "", err
	}
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id} #{pane_id}", "-t", sessionID + ":", "-n", escFormat(win.Name)}
	out, err := c.output(ctx, append(args, paneArgs(win.PaneSpec)...)...)
	if err != nil {
		return "", "", err
	}
	id, err := ids(out, 2)
	if err != nil {
		return "", "", err
	}
	windowID, paneID = id[0], id[1]
	// The window id came from this call's -P, so it is provably ours. The
	// setup runs detached from cancellation (Ctrl-C must not leave an
	// unmarked window behind) and any failure removes the window again.
	setupCtx, cancel := startContext(ctx)
	defer cancel()
	err = c.remainOnExit(setupCtx, paneID, win.PaneSpec)
	if err == nil {
		err = c.setupWindow(setupCtx, windowID, win)
	}
	if err == nil {
		err = c.paneMarker(setupCtx, paneID, win.PaneSpec)
	}
	if err == nil {
		// A marked pane that never got its command would look in sync
		// forever, hence the removal on failure.
		err = c.startCommand(setupCtx, paneID, win.PaneSpec)
	}
	if err != nil {
		_ = c.KillWindow(setupCtx, windowID)
		return "", "", err
	}
	return windowID, paneID, nil
}

// startContext detaches the command start from the caller's cancellation,
// bounded so a stuck tmux cannot hang it.
func startContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// SplitPane splits targetPane ("h" left/right, "v" top/bottom), giving the new
// pane percent of the space, and sets its marker.
func (c Client) SplitPane(ctx context.Context, targetPane string, dir string, percent int, p PaneSpec) (paneID string, err error) {
	if dir != "h" && dir != "v" {
		return "", fmt.Errorf("tmux: split direction %q must be h or v", dir)
	}
	if err := checkPane(p); err != nil {
		return "", err
	}
	args := []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-t", targetPane, "-" + dir, "-l", strconv.Itoa(percent) + "%"}
	out, err := c.output(ctx, append(args, paneArgs(p)...)...)
	if err != nil {
		return "", err
	}
	id, err := ids(out, 1)
	if err != nil {
		return "", err
	}
	// The pane id came from this call's -P; setup is cancellation-proof and a
	// failure removes the pane again (see NewWindow).
	setupCtx, cancel := startContext(ctx)
	defer cancel()
	err = c.remainOnExit(setupCtx, id[0], p)
	if err == nil {
		err = c.paneMarker(setupCtx, id[0], p)
	}
	if err == nil {
		err = c.startCommand(setupCtx, id[0], p)
	}
	if err != nil {
		_ = c.KillPane(setupCtx, id[0])
		return "", err
	}
	return id[0], nil
}

// SelectLayout applies a classic layout string to windowID.
func (c Client) SelectLayout(ctx context.Context, windowID, layoutString string) error {
	return c.run(ctx, "select-layout", "-t", windowID, layoutString)
}

// optionFlags are the set-option flags selecting scope, before "-t".
func optionFlags(scope string) ([]string, error) {
	switch scope {
	case "session":
		return nil, nil
	case "window":
		return []string{"-w"}, nil
	case "pane":
		return []string{"-p"}, nil
	}
	return nil, fmt.Errorf("tmux: option scope %q must be session, window or pane", scope)
}

// SetOption sets an option at scope "session", "window" or "pane".
func (c Client) SetOption(ctx context.Context, scope, target, name, value string) error {
	flags, err := optionFlags(scope)
	if err != nil {
		return err
	}
	args := append(append([]string{"set"}, flags...), "-t", target, name, escSemi(value))
	return c.run(ctx, args...)
}

// UnsetOption removes an option at scope "session", "window" or "pane"
// from target only (set-option -u).
func (c Client) UnsetOption(ctx context.Context, scope, target, name string) error {
	flags, err := optionFlags(scope)
	if err != nil {
		return err
	}
	args := append(append([]string{"set", "-u"}, flags...), "-t", target, name)
	return c.run(ctx, args...)
}

// RespawnPane restarts the dead pane paneID, with argv or its original
// command when empty. Without -k tmux refuses a pane whose program is still
// running, so a live program is never restarted.
func (c Client) RespawnPane(ctx context.Context, paneID string, argv []string) error {
	if err := checkArgv(argv); err != nil {
		return err
	}
	args := []string{"respawn-pane", "-t", paneID}
	if len(argv) > 0 {
		args = append(args, "--")
		for _, v := range argv {
			args = append(args, escSemi(v))
		}
	}
	return c.run(ctx, args...)
}

// SwapPanes swaps the positions of two panes in their window's pane list; the
// ids stay with their processes.
func (c Client) SwapPanes(ctx context.Context, paneA, paneB string) error {
	return c.run(ctx, "swap-pane", "-d", "-s", paneA, "-t", paneB)
}

// KillWindow kills window id.
func (c Client) KillWindow(ctx context.Context, id string) error {
	return c.run(ctx, "kill-window", "-t", id)
}

// KillPane kills pane id.
func (c Client) KillPane(ctx context.Context, id string) error {
	return c.run(ctx, "kill-pane", "-t", id)
}

// KillSession kills session id.
func (c Client) KillSession(ctx context.Context, id string) error {
	return c.run(ctx, "kill-session", "-t", id)
}

// sessionTarget targets a session id as is and a name by exact match ("=name"),
// never by tmux's prefix or pattern matching.
func sessionTarget(s string) string {
	if isSessionID(s) {
		return s
	}
	return escSemi("=" + s)
}

// isSessionID reports whether s is a tmux session id: "$" and digits only.
// A name that merely starts with "$" is a name.
func isSessionID(s string) bool {
	if len(s) < 2 || s[0] != '$' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SessionIDByKey returns the id of the session whose @cairn_s marker is key.
func (c Client) SessionIDByKey(ctx context.Context, key string) (string, bool, error) {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return "", false, err
	}
	for _, s := range snap.Sessions {
		if s.Key == key {
			return s.ID, true, nil
		}
	}
	return "", false, nil
}
