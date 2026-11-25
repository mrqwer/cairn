package reconcile

import (
	"slices"
	"strings"
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

// agentProject is demo with a main window and agent panes for ids.
func agentProject(ids ...string) workspace.Project {
	p := workspace.Project{ID: "demo", Root: "/home/u/demo", Windows: []workspace.Window{{Key: "main"}}}
	for _, id := range ids {
		p.AddAgentPane(id)
	}
	p.ApplyDefaults("/home/u")
	return p
}

// agentPanes lists "key=agent argv-id" of the agents window.
func agentPanes(f *fakeDriver) []string {
	var out []string
	w := f.window("demo/agents")
	if w == nil {
		return nil
	}
	for _, p := range w.panes {
		out = append(out, p.key+"="+p.agent+" runs "+p.argv[len(p.argv)-1])
	}
	slices.Sort(out)
	return out
}

func TestAgentPaneCarriesIdentity(t *testing.T) {
	ps := []workspace.Project{agentProject("A1", "B2")}
	f := &fakeDriver{idle: map[int]bool{}}
	mustApply(t, f, ps, f.plan(t, ps, Options{}))
	want := []string{"demo/agents/1=A1 runs A1", "demo/agents/2=B2 runs B2"}
	if got := agentPanes(f); !slices.Equal(got, want) {
		t.Fatalf("panes %v", got)
	}
}

// The I2 probe: after "rm one" renumbered the live panes, undo restores the
// config [one, two]. Panes are matched by identity: two's pane moves back
// to /2 and one gets a new pane; no pane runs the wrong conversation.
func TestAgentPanesMatchedByIdentityAfterUndo(t *testing.T) {
	two := []workspace.Project{agentProject("B2")}
	f := &fakeDriver{idle: map[int]bool{}}
	mustApply(t, f, two, f.plan(t, two, Options{}))
	if got := agentPanes(f); !slices.Equal(got, []string{"demo/agents/1=B2 runs B2"}) {
		t.Fatalf("setup %v", got)
	}
	both := []workspace.Project{agentProject("A1", "B2")}
	ops := f.plan(t, both, Options{})
	if !strings.Contains(kinds(ops), KindRekeyPanes) {
		t.Fatalf("no rekey planned: %s", kinds(ops))
	}
	mustApply(t, f, both, ops)
	want := []string{"demo/agents/1=A1 runs A1", "demo/agents/2=B2 runs B2"}
	if got := agentPanes(f); !slices.Equal(got, want) {
		t.Fatalf("panes %v, want %v", got, want)
	}
	if ops := f.plan(t, both, Options{}); actionableOps(ops) {
		t.Fatalf("not converged: %s", kinds(ops))
	}
}

// A pane of a conversation that is no longer in the config never takes the
// key of another conversation: it is moved to an orphan key and reported.
func TestAgentPaneOfUnknownConversationIsOrphaned(t *testing.T) {
	old := []workspace.Project{agentProject("C3")}
	f := &fakeDriver{idle: map[int]bool{}}
	mustApply(t, f, old, f.plan(t, old, Options{}))
	now := []workspace.Project{agentProject("A1")}
	ops := f.plan(t, now, Options{})
	mustApply(t, f, now, ops)
	got := agentPanes(f)
	if len(got) != 2 || !slices.Contains(got, "demo/agents/1=A1 runs A1") {
		t.Fatalf("panes %v", got)
	}
	for _, p := range got {
		if strings.HasSuffix(p, "runs C3") && !strings.Contains(p, "orphan-") {
			t.Fatalf("C3 pane keeps a configured key: %v", got)
		}
	}
	ops = f.plan(t, now, Options{})
	if !strings.Contains(kinds(ops), KindReportExtra) || actionableOps(ops) {
		t.Fatalf("orphan not reported or not converged: %s", kinds(ops))
	}
}

// An untagged pane sitting at an agent pane's key is not that agent.
func TestUntaggedPaneAtAgentKeyIsNotTheAgent(t *testing.T) {
	ps := []workspace.Project{agentProject("A1")}
	f := &fakeDriver{idle: map[int]bool{}}
	mustApply(t, f, ps, f.plan(t, ps, Options{}))
	f.window("demo/agents").panes[0].agent = ""
	mustApply(t, f, ps, f.plan(t, ps, Options{}))
	if got := agentPanes(f); !slices.Contains(got, "demo/agents/1=A1 runs A1") {
		t.Fatalf("panes %v", got)
	}
}

func actionableOps(ops []Op) bool {
	for _, o := range ops {
		if !o.IsNote() {
			return true
		}
	}
	return false
}
