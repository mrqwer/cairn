package workspace

import "sort"

var templates = map[string]Project{
	"shell": {
		Windows: []Window{{Key: "main", Name: "main", Panes: []Pane{{}}}},
	},
	"dev": {
		Windows: []Window{
			{Key: "code", Name: "code", Panes: []Pane{
				{Role: RoleEditor, Run: []string{"nvim"}},
				{Split: SplitH, Size: 35},
			}},
			{Key: "run", Name: "run", Panes: []Pane{{}}},
		},
	},
	"service": {
		Windows: []Window{
			{Key: "main", Name: "main", Panes: []Pane{{}}},
			{Key: "logs", Name: "logs", Panes: []Pane{{Role: RoleService}}},
		},
	},
}

// TemplateNames lists the built-in templates, sorted.
func TemplateNames() []string {
	names := make([]string, 0, len(templates))
	for n := range templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Template returns a deep copy of the named built-in template. The result has
// no ID, name or root; callers fill those in.
func Template(name string) (Project, bool) {
	t, ok := templates[name]
	if !ok {
		return Project{}, false
	}
	out := t
	out.Windows = make([]Window, len(t.Windows))
	for i, w := range t.Windows {
		w.Panes = make([]Pane, len(w.Panes))
		for j, pn := range t.Windows[i].Panes {
			pn.Run = append([]string(nil), pn.Run...)
			if len(pn.Run) == 0 {
				pn.Run = nil
			}
			w.Panes[j] = pn
		}
		out.Windows[i] = w
	}
	return out, true
}
