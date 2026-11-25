// Package reconcile compares the declared projects with a tmux snapshot,
// plans the operations that make tmux match (Plan, pure) and runs them
// (Apply, the only side-effecting part).
package reconcile

import (
	"fmt"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

// NewSize is the size of the detached sessions (and so of their windows)
// apply creates.
var NewSize = layout.Size{W: tmux.DefaultWidth, H: tmux.DefaultHeight}

// AgentCommand returns the cairn executable an agent pane runs as
// "<AgentCommand()> agents pane <conversation id>". The CLI sets it once, at
// init, to a function returning its own path.
var AgentCommand = func() string { return "cairn" }

// DesiredPane is one configured pane with its computed values.
type DesiredPane struct {
	Key     string // "<project>/<window>/<n>", the @cairn_p marker
	Pos     int    // 1-based position in config
	Spec    tmux.PaneSpec
	Restart string
	Split   string // "h" or "v" ("" for the first pane)
	From    int    // 1-based position of the pane it is split from (0 for the first)
	Size    int    // percent given to the new pane
}

// HasRun reports whether the pane has a configured command.
func (p DesiredPane) HasRun() bool { return len(p.Spec.Argv) > 0 }

// DesiredWindow is one configured window.
type DesiredWindow struct {
	Key   string // window key in config
	Spec  tmux.WindowSpec
	Panes []DesiredPane
	Tree  *layout.Node
	// Layout is the tmux layout string for the size Desired was called
	// with; LayoutErr says why there is none (bad split sequence or a
	// window too small for it).
	Layout    string
	LayoutErr error
}

// FullKey is the @cairn_w marker "<project>/<window>".
func (w DesiredWindow) FullKey() string { return w.Spec.Key }

// LayoutFor renders the window's layout string for size.
func (w DesiredWindow) LayoutFor(size layout.Size) (string, error) {
	if w.Tree == nil {
		return "", w.LayoutErr
	}
	return layout.TmuxString(w.Tree, size.W, size.H)
}

// Order returns the pane keys in layout traversal order: the order the
// window's pane list must have before select-layout (decision E6).
func (w DesiredWindow) Order() []string {
	if w.Tree == nil {
		return nil
	}
	var keys []string
	for _, pos := range layout.Order(w.Tree) {
		keys = append(keys, w.Panes[pos-1].Key)
	}
	return keys
}

// pane returns the pane with key k.
func (w DesiredWindow) pane(k string) (DesiredPane, bool) {
	for _, p := range w.Panes {
		if p.Key == k {
			return p, true
		}
	}
	return DesiredPane{}, false
}

// DesiredProject is the computed target state of one project.
type DesiredProject struct {
	ID, Session, Root string
	RootMissing       bool
	Windows           []DesiredWindow
}

// window returns the window with config key k.
func (d DesiredProject) window(k string) (DesiredWindow, bool) {
	for _, w := range d.Windows {
		if w.Key == k {
			return w, true
		}
	}
	return DesiredWindow{}, false
}

// Desired computes the target state of a defaulted project: per window the
// tmux specs (markers, cwd, argv, remain-on-exit for panes with a command),
// the layout tree and its layout string for size. rootExists nil checks the
// real filesystem.
func Desired(p workspace.Project, size layout.Size, rootExists func(string) bool) DesiredProject {
	if rootExists == nil {
		rootExists = dirExists
	}
	d := DesiredProject{ID: p.ID, Session: p.Session, Root: p.Root, RootMissing: !rootExists(p.Root)}
	if d.Session == "" {
		d.Session = p.ID
	}
	for _, w := range p.Windows {
		wkey := p.ID + "/" + w.Key
		dw := DesiredWindow{Key: w.Key}
		for i, pn := range w.Panes {
			from := pn.From
			if i > 0 && from == 0 {
				from = i
			}
			sz := pn.Size
			if i > 0 && sz == 0 {
				sz = workspace.DefaultSize
			}
			argv := append([]string(nil), pn.Run...)
			if pn.Role == workspace.RoleAgent {
				argv = agents.PaneArgv(AgentCommand(), pn.Agent)
			}
			dp := DesiredPane{
				Key: fmt.Sprintf("%s/%d", wkey, i+1), Pos: i + 1, Restart: pn.Restart,
				Split: pn.Split, From: from, Size: sz,
				Spec: tmux.PaneSpec{Cwd: pn.Cwd, Argv: argv},
			}
			if pn.Role == workspace.RoleAgent {
				dp.Spec.Agent = pn.Agent
			}
			if i == 0 {
				dp.Split, dp.From, dp.Size = "", 0, 0
			}
			dp.Spec.Key = dp.Key
			dp.Spec.RemainOnExit = len(dp.Spec.Argv) > 0
			dw.Panes = append(dw.Panes, dp)
		}
		name := w.Name
		if name == "" {
			name = w.Key
		}
		var first tmux.PaneSpec
		if len(dw.Panes) > 0 {
			first = dw.Panes[0].Spec
		}
		dw.Spec = tmux.WindowSpec{Key: wkey, Name: name, PaneSpec: first}
		dw.Tree, dw.LayoutErr = layout.Build(w.Panes)
		if dw.LayoutErr == nil {
			dw.Layout, dw.LayoutErr = dw.LayoutFor(size)
		}
		d.Windows = append(d.Windows, dw)
	}
	return d
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := fsx.Root{Dir: "/"}.Stat(path)
	return err == nil && fi.IsDir()
}
