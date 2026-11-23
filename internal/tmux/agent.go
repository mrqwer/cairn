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

// AgentOption is the pane option holding an agent pane's conversation id.
const AgentOption = "@cairn_agent"

// ValidAgentID reports whether s is a usable conversation id marker
// ([A-Za-z0-9][A-Za-z0-9_-]{0,62}).
func ValidAgentID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || (i > 0 && (c == '_' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// agentMarker keeps only a valid @cairn_agent value.
func agentMarker(v string) string {
	if ValidAgentID(v) {
		return v
	}
	return ""
}
