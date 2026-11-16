package workspace_test

import (
	"testing"

	"github.com/mrqwer/cairn/internal/workspace"
)

func valid() workspace.Project {
	return workspace.Project{
		ID: "demo", Root: "/home/u/demo",
		Windows: []workspace.Window{{Key: "main", Panes: []workspace.Pane{{}, {Split: "h"}}}},
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*workspace.Project)
		key  string
	}{
		{"bad slug", func(p *workspace.Project) { p.ID = "Bad_ID!" }, "id"},
		{"relative root", func(p *workspace.Project) { p.Root = "code/x" }, "root"},
		{"first pane split", func(p *workspace.Project) { p.Windows[0].Panes[0].Split = "h" }, "windows[0].panes[0].split"},
		{"from forward", func(p *workspace.Project) { p.Windows[0].Panes[1].From = 2 }, "windows[0].panes[1].from"},
		{"size 3", func(p *workspace.Project) { p.Windows[0].Panes[1].Size = 3 }, "windows[0].panes[1].size"},
		{"duplicate window keys", func(p *workspace.Project) {
			p.Windows = append(p.Windows, workspace.Window{Key: "main", Panes: []workspace.Pane{{}}})
		}, "windows[1].key"},
		{"session colon", func(p *workspace.Project) { p.Session = "a:b" }, "session"},
		{"missing split", func(p *workspace.Project) { p.Windows[0].Panes[1].Split = "" }, "windows[0].panes[1].split"},
		{"bad role", func(p *workspace.Project) { p.Windows[0].Panes[0].Role = "x" }, "windows[0].panes[0].role"},
		{"bad status", func(p *workspace.Project) { p.Status = "x" }, "status"},
		{"empty run", func(p *workspace.Project) { p.Windows[0].Panes[0].Run = []string{"a", ""} }, "windows[0].panes[0].run[1]"},
		{"no windows", func(p *workspace.Project) { p.Windows = nil }, "windows"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid()
			c.mod(&p)
			p.ApplyDefaults("/home/u")
			// ApplyDefaults fills from/size; re-apply the case's override.
			c.mod(&p)
			errs := p.Validate()
			for _, e := range errs {
				if e.Key == c.key {
					return
				}
			}
			t.Fatalf("want error on %q, got %v", c.key, errs)
		})
	}
	p := valid()
	p.ApplyDefaults("/home/u")
	if errs := p.Validate(); len(errs) != 0 {
		t.Fatalf("valid project: %v", errs)
	}
}
