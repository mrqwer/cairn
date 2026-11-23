package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/ui"
	"github.com/mrqwer/cairn/internal/xdg"
)

// agentsStatus is the agent part of status --json.
type agentsStatus struct {
	// Counts is the number of conversations per state, archived ones left
	// out. A live state (starting, working, waiting, idle) whose tool is not
	// running is counted as stopped: it is left over from a crash or reboot.
	Counts  map[string]int      `json:"counts"`
	Waiting []waitingConvStatus `json:"waiting"`
	// Risky are the live conversations (tool running) launched with flags
	// that skip the tool's permission checks (the "!" badge); agents ls
	// shows the mark on every conversation.
	Risky []riskyConvStatus `json:"risky"`
}

type riskyConvStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project"`
	State   string `json:"state"`
}

type waitingConvStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project"`
	Task    string `json:"task"`
	Reason  string `json:"reason"`
}

func liveState(s string) bool {
	switch s {
	case conv.StateStarting, conv.StateWorking, conv.StateWaiting, conv.StateIdle:
		return true
	}
	return false
}

func stateOf(c conv.Conversation) string {
	if c.State == "" {
		return conv.StateStarting
	}
	return c.State
}

// readAgentsStatus folds the records into counts. It reads the record file,
// the curated overlays and, for live states, the conversation's lock file;
// it never calls tmux or a tool. liveOnly reads the overlays of records in
// a live state only and counts nothing else (enough for the badge).
func readAgentsStatus(s conv.Store, liveOnly bool) (agentsStatus, error) {
	st := agentsStatus{Counts: map[string]int{}, Waiting: []waitingConvStatus{}, Risky: []riskyConvStatus{}}
	var cs []conv.Conversation
	var err error
	if liveOnly {
		cs, err = s.Scan(func(c conv.Conversation) bool { return liveState(stateOf(c)) })
	} else {
		cs, err = s.List(conv.Filter{})
	}
	if err != nil {
		return st, err
	}
	for _, c := range cs {
		state := stateOf(c)
		if liveState(state) && !s.LockLive(c.Tool, "cairn-"+c.ID) {
			state = conv.StateStopped
		}
		st.Counts[state]++
		if c.Risk && liveState(state) {
			st.Risky = append(st.Risky, riskyConvStatus{ID: c.ID, Name: c.Name, Project: c.Project, State: state})
		}
		if state == conv.StateWaiting {
			st.Waiting = append(st.Waiting, waitingConvStatus{ID: c.ID, Name: c.Name, Project: c.Project, Task: c.Task, Reason: c.WaitingReason})
		}
	}
	return st, nil
}

// badge is "2 waiting · 3 working", or "" when nobody waits or works.
func (st agentsStatus) badge() string {
	var parts []string
	for _, k := range []string{conv.StateWaiting, conv.StateWorking} {
		if n := st.Counts[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	return strings.Join(parts, " · ")
}

func (st agentsStatus) total() int {
	n := 0
	for _, v := range st.Counts {
		n += v
	}
	return n
}

// printStatusTmux is status --tmux: the badge for a tmux status line.
// Errors print nothing (a status line must not show them) and exit 0.
func printStatusTmux(out, errOut io.Writer) {
	dirs, err := stateDirs()
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "cairn status:", err)
		return
	}
	st, err := readAgentsStatus(agentStore(dirs), true)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "cairn status:", err)
		return
	}
	if b := st.badge(); b != "" {
		for _, r := range st.Risky {
			if r.State == conv.StateWorking || r.State == conv.StateWaiting {
				b = "! " + b
				break
			}
		}
		_, _ = fmt.Fprintln(out, b)
	}
}

// printAgentsStatus appends the agent summary to the plain status; nothing
// when there are no conversations.
func printAgentsStatus(w io.Writer, st agentsStatus) {
	if st.total() == 0 {
		return
	}
	b := st.badge()
	if b == "" {
		b = "none waiting or working"
	}
	_, _ = fmt.Fprintf(w, "\nagents: %s\n", b)
	for _, c := range st.Waiting {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		_, _ = fmt.Fprintf(w, "  waiting  %s %s %s\n", ui.Pad(name, 24), ui.Pad(c.Project, 12), c.Reason)
	}
	for _, c := range st.Risky {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		_, _ = fmt.Fprintf(w, "  ! %s %s %s, launched with flags that skip permission checks\n",
			ui.Pad(ui.SafeLine(name), 24), ui.Pad(ui.SafeLine(c.Project), 12), c.State)
	}
}

// stateDirs resolves the XDG dirs for commands that use only the state dir
// (hooks, the tmux status line). They may run with a reduced environment
// (a tool's hook, tmux's #()), so a missing XDG_RUNTIME_DIR is not an error.
func stateDirs() (xdg.Dirs, error) {
	return xdg.Resolve(func(k string) string {
		v := os.Getenv(k)
		if k == "XDG_RUNTIME_DIR" && !filepath.IsAbs(v) {
			return "/run/user/" + strconv.Itoa(os.Getuid())
		}
		return v
	})
}
