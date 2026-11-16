// Package tmux drives a tmux server through execx.Runner: it reads a snapshot
// of sessions, windows and panes, runs the operations the reconciler needs
// and checks whether a pane is idle. Every target is a tmux id ($n, @n, %n).
package tmux

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mrqwer/cairn/internal/execx"
)

// Client runs "tmux [-L Socket] ..." through R. An empty Socket means the
// default server.
type Client struct {
	R      execx.Runner
	Socket string
}

// NewClient returns a Client running the real tmux with an environment
// without TMUX and TMUX_PANE, so the surrounding session is never targeted
// implicitly.
func NewClient(socket string) Client {
	return Client{R: execx.OSRunner{Env: cleanEnv(os.Environ())}, Socket: socket}
}

// cleanEnv returns a copy of env without TMUX and TMUX_PANE.
func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runner returns R, with TMUX and TMUX_PANE removed from the environment when
// R is an OSRunner (a nil Env would otherwise inherit them).
func (c Client) runner() execx.Runner {
	r, ok := c.R.(execx.OSRunner)
	if !ok {
		return c.R
	}
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	r.Env = cleanEnv(env)
	return r
}

func (c Client) base() []string {
	if c.Socket == "" {
		return nil
	}
	return []string{"-L", c.Socket}
}

// output runs tmux with args and returns its output; a failure carries the
// command and tmux's message.
func (c Client) output(ctx context.Context, args ...string) (string, error) {
	out, err := c.runner().Run(ctx, "tmux", append(c.base(), args...)...)
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (c Client) run(ctx context.Context, args ...string) error {
	_, err := c.output(ctx, args...)
	return err
}

// AttachCommand returns the argv that attaches a terminal to session; the CLI
// execs it (it is not run through a Runner).
func AttachCommand(socket, session string) []string {
	argv := []string{"tmux"}
	if socket != "" {
		argv = append(argv, "-L", socket)
	}
	return append(argv, "attach-session", "-t", session)
}

// AttachArgs is AttachCommand.
func AttachArgs(socket, session string) []string { return AttachCommand(socket, session) }
