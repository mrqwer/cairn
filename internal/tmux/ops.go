package tmux

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
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

// paneArgs returns "-c cwd -e K=V... [-- argv...]" for p.
func paneArgs(p PaneSpec) []string {
	var a []string
	if p.Cwd != "" {
		a = append(a, "-c", p.Cwd)
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k+"="+p.Env[k])
	}
	if len(p.Argv) > 0 {
		a = append(a, "--")
		a = append(a, p.Argv...)
	}
	return a
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
// the pane is created, before anything else.
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
	if win.Key != "" {
		if err := c.SetOption(ctx, "window", id, "@cairn_w", win.Key); err != nil {
			return err
		}
	}
	return c.SetOption(ctx, "window", id, "automatic-rename", "off")
}

// NewSession creates a detached session of w x h (DefaultWidth x
// DefaultHeight when 0) with its first window and pane, and sets the markers.
func (c Client) NewSession(ctx context.Context, name string, w, h int, win WindowSpec) (sessionID, windowID, paneID string, err error) {
	if w <= 0 {
		w = DefaultWidth
	}
	if h <= 0 {
		h = DefaultHeight
	}
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id} #{window_id} #{pane_id}",
		"-s", name, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h), "-n", win.Name}
	out, err := c.output(ctx, append(args, paneArgs(win.PaneSpec)...)...)
	if err != nil {
		return "", "", "", err
	}
	id, err := ids(out, 3)
	if err != nil {
		return "", "", "", err
	}
	sessionID, windowID, paneID = id[0], id[1], id[2]
	if err := c.remainOnExit(ctx, paneID, win.PaneSpec); err != nil {
		return sessionID, windowID, paneID, err
	}
	if project, _, ok := strings.Cut(win.Key, "/"); ok && project != "" {
		if err := c.SetOption(ctx, "session", sessionID, "@cairn_s", project); err != nil {
			return sessionID, windowID, paneID, err
		}
	}
	if err := c.setupWindow(ctx, windowID, win); err != nil {
		return sessionID, windowID, paneID, err
	}
	return sessionID, windowID, paneID, c.paneMarker(ctx, paneID, win.PaneSpec)
}

// NewWindow creates a window at the end of sessionID with its first pane and
// sets the window and pane markers.
func (c Client) NewWindow(ctx context.Context, sessionID string, win WindowSpec) (windowID, paneID string, err error) {
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id} #{pane_id}", "-t", sessionID + ":", "-n", win.Name}
	out, err := c.output(ctx, append(args, paneArgs(win.PaneSpec)...)...)
	if err != nil {
		return "", "", err
	}
	id, err := ids(out, 2)
	if err != nil {
		return "", "", err
	}
	windowID, paneID = id[0], id[1]
	if err := c.remainOnExit(ctx, paneID, win.PaneSpec); err != nil {
		return windowID, paneID, err
	}
	if err := c.setupWindow(ctx, windowID, win); err != nil {
		return windowID, paneID, err
	}
	return windowID, paneID, c.paneMarker(ctx, paneID, win.PaneSpec)
}

// SplitPane splits targetPane ("h" left/right, "v" top/bottom), giving the new
// pane percent of the space, and sets its marker.
func (c Client) SplitPane(ctx context.Context, targetPane string, dir string, percent int, p PaneSpec) (paneID string, err error) {
	if dir != "h" && dir != "v" {
		return "", fmt.Errorf("tmux: split direction %q must be h or v", dir)
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
	if err := c.remainOnExit(ctx, id[0], p); err != nil {
		return id[0], err
	}
	return id[0], c.paneMarker(ctx, id[0], p)
}

// SelectLayout applies a classic layout string to windowID.
func (c Client) SelectLayout(ctx context.Context, windowID, layoutString string) error {
	return c.run(ctx, "select-layout", "-t", windowID, layoutString)
}

// SetOption sets an option at scope "session", "window" or "pane".
func (c Client) SetOption(ctx context.Context, scope, target, name, value string) error {
	var flags []string
	switch scope {
	case "session":
		flags = []string{"-t"}
	case "window":
		flags = []string{"-w", "-t"}
	case "pane":
		flags = []string{"-p", "-t"}
	default:
		return fmt.Errorf("tmux: option scope %q must be session, window or pane", scope)
	}
	args := append([]string{"set"}, flags...)
	return c.run(ctx, append(args, target, name, value)...)
}

// RespawnPane restarts paneID, with argv or its original command when empty.
func (c Client) RespawnPane(ctx context.Context, paneID string, argv []string) error {
	args := []string{"respawn-pane", "-k", "-t", paneID}
	if len(argv) > 0 {
		args = append(append(args, "--"), argv...)
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

// SwitchClient switches the current client to sessionName.
func (c Client) SwitchClient(ctx context.Context, sessionName string) error {
	return c.run(ctx, "switch-client", "-t", sessionName)
}
