package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SessionAttached reports whether the session of pane paneID ("%n") has at
// least one client attached (control-mode clients count).
func (c Client) SessionAttached(ctx context.Context, paneID string) (bool, error) {
	if !strings.HasPrefix(paneID, "%") {
		return false, fmt.Errorf("tmux: %q is not a pane id", paneID)
	}
	out, err := c.output(ctx, "display-message", "-p", "-t", paneID, "#{session_attached}")
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("tmux display-message: unexpected session_attached %q", strings.TrimSpace(out))
	}
	return n > 0, nil
}

// SelectPane makes pane paneID ("%n") the active pane of its window and that
// window the current one of its session.
func (c Client) SelectPane(ctx context.Context, paneID string) error {
	if !strings.HasPrefix(paneID, "%") {
		return fmt.Errorf("tmux: %q is not a pane id", paneID)
	}
	if err := c.run(ctx, "select-window", "-t", paneID); err != nil {
		return err
	}
	return c.run(ctx, "select-pane", "-t", paneID)
}
