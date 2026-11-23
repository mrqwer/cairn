package reconcile

import (
	"slices"
	"testing"

	"github.com/mrqwer/cairn/internal/workspace"
)

func TestDesiredAgentPaneArgv(t *testing.T) {
	old := AgentCommand
	t.Cleanup(func() { AgentCommand = old })
	AgentCommand = func() string { return "/opt/cairn/bin/cairn" }
	p := workspace.Project{ID: "demo", Root: "/home/u/demo", Windows: []workspace.Window{{Key: "main"}}}
	p.AddAgentPane("01JTEST")
	p.ApplyDefaults("/home/u")
	d := Desired(p, NewSize, func(string) bool { return true })
	dp := d.Windows[1].Panes[0]
	want := []string{"/opt/cairn/bin/cairn", "agents", "pane", "01JTEST"}
	if !slices.Equal(dp.Spec.Argv, want) {
		t.Fatalf("argv %v, want %v", dp.Spec.Argv, want)
	}
	if !dp.Spec.RemainOnExit || dp.Key != "demo/agents/1" {
		t.Fatalf("spec %+v key %s", dp.Spec, dp.Key)
	}
}
