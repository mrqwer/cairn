package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mrqwer/cairn/internal/execx"
)

// SocketPath returns the socket path of the server c talks to; it is an
// error when no server is running.
func (c Client) SocketPath(ctx context.Context) (string, error) {
	out, err := c.output(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		if noServer(out) {
			return "", errors.New("no tmux server running")
		}
		return "", err
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return "", errors.New("tmux display-message: empty socket path")
	}
	return p, nil
}

// hereRunner is the runner with TMUX and TMUX_PANE set to the invoking
// client's values (and no other copies of them).
func (c Client) hereRunner(tmuxEnv, tmuxPane string) execx.Runner {
	r, ok := c.R.(execx.OSRunner)
	if !ok {
		return c.R
	}
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	env = append(cleanEnv(env), "TMUX="+tmuxEnv)
	if tmuxPane != "" {
		env = append(env, "TMUX_PANE="+tmuxPane)
	}
	r.Env = env
	return r
}

// SwitchClientHere switches the client the command was invoked from to the
// session sessionID ("$n"). It runs with the invoking client's TMUX and
// TMUX_PANE, so tmux resolves the client from the invoking pane rather
// than picking the most recently active one. The caller must have checked
// that TMUX names the server c talks to (see SocketPath).
func (c Client) SwitchClientHere(ctx context.Context, sessionID, tmuxEnv, tmuxPane string) error {
	if !strings.HasPrefix(sessionID, "$") {
		return fmt.Errorf("tmux switch-client: %q is not a session id", sessionID)
	}
	if tmuxEnv == "" {
		return errors.New("tmux switch-client: not inside a tmux client (TMUX is empty)")
	}
	args := append(c.base(), "switch-client", "-t", sessionID)
	out, err := c.hereRunner(tmuxEnv, tmuxPane).Run(ctx, "tmux", args...)
	if err != nil {
		return fmt.Errorf("tmux switch-client: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
