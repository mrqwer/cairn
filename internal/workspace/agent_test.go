package workspace_test

import (
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/workspace"
)

func TestAgentPaneValidation(t *testing.T) {
	const id = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	cases := []struct {
		name string
		pane workspace.Pane
		key  string // "" = valid
	}{
		{"valid agent", workspace.Pane{Role: workspace.RoleAgent, Agent: id}, ""},
		{"agent role needs id", workspace.Pane{Role: workspace.RoleAgent}, "windows[0].panes[0].agent"},
		{"bad id", workspace.Pane{Role: workspace.RoleAgent, Agent: "../x"}, "windows[0].panes[0].agent"},
		{"agent without role", workspace.Pane{Role: workspace.RoleShell, Agent: id}, "windows[0].panes[0].agent"},
		{"agent with run", workspace.Pane{Role: workspace.RoleAgent, Agent: id, Run: []string{"x"}}, "windows[0].panes[0].run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := workspace.Project{ID: "demo", Root: "/home/u/demo",
				Windows: []workspace.Window{{Key: "agents", Panes: []workspace.Pane{c.pane}}}}
			p.ApplyDefaults("/home/u")
			errs := p.Validate()
			if c.key == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			for _, e := range errs {
				if e.Key == c.key {
					return
				}
			}
			t.Fatalf("no error for %s in %v", c.key, errs)
		})
	}
}

func TestAgentPaneDefaultsAndMarshal(t *testing.T) {
	p := workspace.Project{ID: "demo", Root: "/home/u/demo", Windows: []workspace.Window{{Key: "main"}}}
	key := p.AddAgentPane("A1")
	key2 := p.AddAgentPane("A2")
	if key != "demo/agents/1" || key2 != "demo/agents/2" {
		t.Fatalf("keys %q %q", key, key2)
	}
	data, err := workspace.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `agent = "A1"`) || !strings.Contains(string(data), `role = "agent"`) {
		t.Fatalf("marshal:\n%s", data)
	}
	back, err := workspace.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	back.ApplyDefaults("/home/u")
	if errs := back.Validate(); len(errs) != 0 {
		t.Fatalf("invalid: %v\n%s", errs, data)
	}
	if back.Windows[1].Panes[0].Restart != workspace.RestartNever {
		t.Fatalf("restart %q", back.Windows[1].Panes[0].Restart)
	}
}

func TestRemoveAgentPane(t *testing.T) {
	p := workspace.Project{ID: "demo", Root: "/home/u/demo", Windows: []workspace.Window{{Key: "main"}}}
	p.AddAgentPane("A1")
	p.AddAgentPane("A2")
	p.AddAgentPane("A3")
	w, pos, ok := p.RemoveAgentPane("A1")
	if !ok || w != "agents" || pos != 1 {
		t.Fatalf("remove: %q %d %v", w, pos, ok)
	}
	panes := p.Windows[1].Panes
	if len(panes) != 2 || panes[0].Agent != "A2" || panes[0].Split != "" || panes[0].From != 0 || panes[0].Size != 0 {
		t.Fatalf("panes %+v", panes)
	}
	if _, _, ok := p.RemoveAgentPane("nope"); ok {
		t.Fatal("removed unknown")
	}
	p.RemoveAgentPane("A2")
	p.RemoveAgentPane("A3")
	if len(p.Windows) != 1 {
		t.Fatalf("empty agents window kept: %+v", p.Windows)
	}
	if key, ok := p.AgentPaneKey("A3"); ok {
		t.Fatalf("still bound at %s", key)
	}
	cp := p
	cp.Windows = nil
	cp.AddAgentPane("B")
	if _, _, ok := cp.RemoveAgentPane("B"); !ok || len(cp.Windows) != 1 || len(cp.Windows[0].Panes) != 0 {
		t.Fatalf("last window must stay: %+v", cp.Windows)
	}
}
