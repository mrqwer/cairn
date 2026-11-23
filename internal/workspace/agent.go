package workspace

import (
	"fmt"
	"strings"
)

// AddAgentPane appends a pane bound to conversation convID to the
// AgentWindow window, creating that window when it is missing, and returns
// the pane's @cairn_p key. p need not be defaulted: new panes leave from and
// size to the defaults (split from the previous pane, half of it).
func (p *Project) AddAgentPane(convID string) string {
	return p.AddAgentPaneIn(AgentWindow, AgentWindow, convID)
}

// AddAgentPaneIn is AddAgentPane for window key (a task's window), created
// with name when missing.
func (p *Project) AddAgentPaneIn(key, name, convID string) string {
	wi := -1
	for i, w := range p.Windows {
		if w.Key == key {
			wi = i
			break
		}
	}
	if wi < 0 {
		p.Windows = append(p.Windows, Window{Key: key, Name: name})
		wi = len(p.Windows) - 1
	}
	w := &p.Windows[wi]
	pn := Pane{Role: RoleAgent, Agent: convID}
	if len(w.Panes) > 0 {
		pn.Split = SplitH
	}
	w.Panes = append(w.Panes, pn)
	return fmt.Sprintf("%s/%s/%d", p.ID, w.Key, len(w.Panes))
}

// AgentOnlyWindow reports whether window key is one cairn creates for
// agent panes (the agents window, a task window): it goes when its last
// agent pane does.
func AgentOnlyWindow(key string) bool {
	return key == AgentWindow || strings.HasPrefix(key, TaskWindowPrefix)
}

// AgentPaneKey returns the @cairn_p key of the pane bound to convID.
func (p Project) AgentPaneKey(convID string) (string, bool) {
	for _, w := range p.Windows {
		for i, pn := range w.Panes {
			if pn.Role == RoleAgent && pn.Agent == convID {
				return fmt.Sprintf("%s/%s/%d", p.ID, w.Key, i+1), true
			}
		}
	}
	return "", false
}

// RemoveAgentPane removes the pane bound to convID and returns its window
// key and 1-based position. Later panes keep their split sequence: a pane
// split from the removed one is split from its predecessor instead, and
// references past it move down by one. An agent-only window (AgentOnlyWindow)
// left empty is removed unless it is the project's only window.
func (p *Project) RemoveAgentPane(convID string) (window string, pos int, ok bool) {
	for wi := range p.Windows {
		w := &p.Windows[wi]
		for i, pn := range w.Panes {
			if pn.Role != RoleAgent || pn.Agent != convID {
				continue
			}
			k := i + 1
			w.Panes = append(w.Panes[:i], w.Panes[i+1:]...)
			for j := i; j < len(w.Panes); j++ {
				q := &w.Panes[j]
				switch {
				case q.From == k:
					q.From = 0
				case q.From > k:
					q.From--
				}
			}
			if len(w.Panes) > 0 && i == 0 {
				first := &w.Panes[0]
				first.Split, first.From, first.Size = "", 0, 0
			}
			window = w.Key
			if len(w.Panes) == 0 && AgentOnlyWindow(w.Key) && len(p.Windows) > 1 {
				p.Windows = append(p.Windows[:wi], p.Windows[wi+1:]...)
			}
			return window, k, true
		}
	}
	return "", 0, false
}
