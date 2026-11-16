package workspace_test

import (
	"strings"
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

func TestValidateRejectsControlChars(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*workspace.Project)
		key  string
	}{
		{"id", func(p *workspace.Project) { p.ID = "a\tb" }, "id"},
		{"name", func(p *workspace.Project) { p.Name = "a\nb" }, "name"},
		{"session", func(p *workspace.Project) { p.Session = "a\x7fb" }, "session"},
		{"window key", func(p *workspace.Project) { p.Windows[0].Key = "a\nb" }, "windows[0].key"},
		{"window name", func(p *workspace.Project) { p.Windows[0].Name = "a\x01b" }, "windows[0].name"},
		{"pane cwd", func(p *workspace.Project) { p.Windows[0].Panes[0].Cwd = "/x\ny" }, "windows[0].panes[0].cwd"},
		{"pane run", func(p *workspace.Project) { p.Windows[0].Panes[0].Run = []string{"a\x1bb"} }, "windows[0].panes[0].run[0]"},
		{"pane title", func(p *workspace.Project) { p.Windows[0].Panes[1].Title = "a\tb" }, "windows[0].panes[1].title"},
		{"pane role", func(p *workspace.Project) { p.Windows[0].Panes[0].Role = "a\nb" }, "windows[0].panes[0].role"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid()
			p.ApplyDefaults("/home/u")
			c.mod(&p)
			for _, e := range p.Validate() {
				if e.Key == c.key && strings.Contains(e.Msg, "control character") {
					return
				}
			}
			t.Fatalf("want control-character error on %q, got %v", c.key, p.Validate())
		})
	}
}

func TestValidateRejectsTildeCwd(t *testing.T) {
	for _, cwd := range []string{"~", "~/code", "~user/x"} {
		p := valid()
		p.Windows[0].Panes[0].Cwd = cwd
		p.ApplyDefaults("/home/u")
		found := false
		for _, e := range p.Validate() {
			if e.Key == "windows[0].panes[0].cwd" && strings.Contains(e.Msg, "absolute") {
				found = true
			}
		}
		if !found {
			t.Fatalf("cwd %q: want a hint error, got %v", cwd, p.Validate())
		}
	}
	p := valid()
	p.Windows[0].Panes[0].Cwd = "sub"
	p.ApplyDefaults("/home/u")
	if errs := p.Validate(); len(errs) != 0 || p.Windows[0].Panes[0].Cwd != "/home/u/demo/sub" {
		t.Fatalf("relative cwd must stay relative to root: %v %q", errs, p.Windows[0].Panes[0].Cwd)
	}
}

func TestSlugAlwaysValid(t *testing.T) {
	for _, in := range []string{"_x", "__a_", "!!!", "", "  ", "Ünï", "a", "-_-"} {
		got := workspace.Slug(in)
		if got == "" || got[0] == '_' || got[0] == '-' {
			t.Errorf("Slug(%q) = %q", in, got)
		}
		p := workspace.Project{ID: got, Root: "/r", Windows: []workspace.Window{{Key: "k"}}}
		p.ApplyDefaults("/h")
		for _, e := range p.Validate() {
			if e.Key == "id" {
				t.Errorf("Slug(%q) = %q fails slugRe: %v", in, got, e)
			}
		}
	}
	if got := workspace.Slug("_Hello World_"); got != "hello-world" {
		t.Errorf("got %q", got)
	}
	if k, ok := workspace.TrySlug("!!!"); ok || k != "" {
		t.Errorf("TrySlug = %q %v", k, ok)
	}
}
