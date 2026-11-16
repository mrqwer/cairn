package reconcile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

// Op kinds. The skip-* and report-* kinds are notes: Apply does nothing for
// them.
const (
	KindCreateSession   = "create-session"
	KindCreateWindow    = "create-window"
	KindCreatePane      = "create-pane"
	KindSetLayout       = "set-layout"
	KindRespawnPane     = "respawn-pane"
	KindKillWindow      = "kill-window"
	KindKillPane        = "kill-pane"
	KindReportExtra     = "report-extra"
	KindReportDead      = "report-dead"
	KindSkipMissingRoot = "skip-missing-root"
	KindSkipNameClash   = "skip-name-clash"
	KindSkipLayout      = "skip-layout"
)

// Op is one planned change (or note). Target is a tmux id for existing
// objects ($n, @n, %n), the session name for create-session and the root for
// skip-missing-root; WindowKey is the window key in config and PaneKey the
// full @cairn_p marker.
type Op struct {
	Kind, Project, Target, Detail string
	WindowKey, PaneKey            string
	Destructive                   bool
}

// IsNote reports whether the op only reports something.
func (o Op) IsNote() bool {
	switch o.Kind {
	case KindReportExtra, KindReportDead, KindSkipMissingRoot, KindSkipNameClash, KindSkipLayout:
		return true
	}
	return false
}

// Options tune Plan. RootExists nil checks the filesystem. PaneIdle answers
// whether a pane process is an idle shell (tmux.Client.PaneIdle); nil means
// no pane counts as idle, so Prune then kills nothing.
type Options struct {
	Prune      bool
	Only       string
	RootExists func(string) bool
	PaneIdle   func(pid int) (bool, error)
}

// Plan returns the operations that make snap match the active projects ps,
// in a deterministic order: projects by id, windows and panes by config
// order. It never touches anything without a cairn marker.
func Plan(ps []workspace.Project, snap tmux.Snapshot, opts Options) []Op {
	sorted := append([]workspace.Project(nil), ps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var ops []Op
	for _, p := range sorted {
		if p.Status != workspace.StatusActive || (opts.Only != "" && p.ID != opts.Only) {
			continue
		}
		ops = append(ops, planProject(p, snap, opts)...)
	}
	return ops
}

func planProject(p workspace.Project, snap tmux.Snapshot, opts Options) []Op {
	d := Desired(p, NewSize, opts.RootExists)
	if d.RootMissing {
		return []Op{{Kind: KindSkipMissingRoot, Project: d.ID, Target: d.Root,
			Detail: fmt.Sprintf("root %s does not exist", d.Root)}}
	}
	var sess *tmux.Session
	var dupes []tmux.Session
	for i := range snap.Sessions {
		if snap.Sessions[i].Key != d.ID {
			continue
		}
		if sess == nil {
			sess = &snap.Sessions[i]
		} else {
			dupes = append(dupes, snap.Sessions[i])
		}
	}
	if sess == nil {
		for _, s := range snap.Sessions {
			if s.Name == d.Session {
				return []Op{{Kind: KindSkipNameClash, Project: d.ID, Target: d.Session,
					Detail: fmt.Sprintf("a session named %q exists that cairn does not manage; rename it or set session in the project", d.Session)}}
			}
		}
		ops := []Op{{Kind: KindCreateSession, Project: d.ID, Target: d.Session, WindowKey: d.Windows[0].Key,
			Detail: fmt.Sprintf("session %s: %d windows, %d panes", d.Session, len(d.Windows), countPanes(d))}}
		for _, w := range d.Windows {
			ops = append(ops, newWindowLayoutNote(d, w, NewSize)...)
		}
		return ops
	}

	var ops []Op
	prefix := d.ID + "/"
	windows := map[string]tmux.Window{}
	var extras []tmux.Window
	for _, w := range sess.Windows {
		if !strings.HasPrefix(w.Key, prefix) {
			continue // unmanaged or another project's window: ignored
		}
		k := strings.TrimPrefix(w.Key, prefix)
		if _, configured := d.window(k); !configured {
			extras = append(extras, w)
			continue
		}
		if _, seen := windows[k]; seen {
			ops = append(ops, Op{Kind: KindReportExtra, Project: d.ID, Target: w.ID, WindowKey: k,
				Detail: fmt.Sprintf("duplicate window %s (%s)", w.Key, w.ID)})
			continue
		}
		windows[k] = w
	}
	size := NewSize
	if len(sess.Windows) > 0 {
		size = layout.Size{W: sess.Windows[0].W, H: sess.Windows[0].H}
	}
	for _, dw := range d.Windows {
		tw, ok := windows[dw.Key]
		if !ok {
			ops = append(ops, Op{Kind: KindCreateWindow, Project: d.ID, Target: sess.ID, WindowKey: dw.Key,
				Detail: fmt.Sprintf("window %s: %d panes", dw.Key, len(dw.Panes))})
			ops = append(ops, newWindowLayoutNote(d, dw, size)...)
			continue
		}
		ops = append(ops, planWindow(d, dw, tw, opts)...)
	}
	for _, w := range extras {
		k := strings.TrimPrefix(w.Key, prefix)
		if opts.Prune && len(w.Panes) > 0 && allPrunable(w.Panes, opts) {
			ops = append(ops, Op{Kind: KindKillWindow, Project: d.ID, Target: w.ID, WindowKey: k, Destructive: true,
				Detail: fmt.Sprintf("window %s is not in the config and all its panes are idle shells", w.Key)})
			continue
		}
		ops = append(ops, Op{Kind: KindReportExtra, Project: d.ID, Target: w.ID, WindowKey: k,
			Detail: fmt.Sprintf("window %s is not in the config", w.Key)})
	}
	for _, s := range dupes {
		ops = append(ops, Op{Kind: KindReportExtra, Project: d.ID, Target: s.ID,
			Detail: fmt.Sprintf("another session (%s) carries the marker of project %s", s.Name, d.ID)})
	}
	return ops
}

// newWindowLayoutNote reports a window whose layout will not be applied
// when it is created at size.
func newWindowLayoutNote(d DesiredProject, w DesiredWindow, size layout.Size) []Op {
	if len(w.Panes) < 2 {
		return nil
	}
	if _, err := w.LayoutFor(size); err != nil {
		return []Op{{Kind: KindSkipLayout, Project: d.ID, WindowKey: w.Key,
			Detail: fmt.Sprintf("window %s: layout not applied: %v", w.Key, err)}}
	}
	return nil
}

func countPanes(d DesiredProject) int {
	n := 0
	for _, w := range d.Windows {
		n += len(w.Panes)
	}
	return n
}

// planWindow compares an existing managed window with its config.
func planWindow(d DesiredProject, dw DesiredWindow, tw tmux.Window, opts Options) []Op {
	var ops []Op
	prefix := dw.FullKey() + "/"
	byKey := map[string]tmux.Pane{}
	foreign := false // a pane that will stay but is not in the config
	changed := false
	for _, tp := range tw.Panes {
		if !strings.HasPrefix(tp.Key, prefix) {
			foreign = true // the user's own pane (or another window's marker): left alone
			continue
		}
		_, configured := dw.pane(tp.Key)
		if _, seen := byKey[tp.Key]; configured && !seen {
			byKey[tp.Key] = tp
			continue
		}
		if configured || !opts.Prune || !prunable(tp, opts) {
			foreign = true
			ops = append(ops, Op{Kind: KindReportExtra, Project: d.ID, Target: tp.ID, WindowKey: dw.Key, PaneKey: tp.Key,
				Detail: fmt.Sprintf("pane %s (%s) is not in the config", tp.Key, tp.ID)})
			continue
		}
		changed = true
		ops = append(ops, Op{Kind: KindKillPane, Project: d.ID, Target: tp.ID, WindowKey: dw.Key, PaneKey: tp.Key, Destructive: true,
			Detail: fmt.Sprintf("pane %s is not in the config and is an idle shell", tp.Key)})
	}
	for _, dp := range dw.Panes {
		tp, ok := byKey[dp.Key]
		if !ok {
			changed = true
			op := Op{Kind: KindCreatePane, Project: d.ID, WindowKey: dw.Key, PaneKey: dp.Key,
				Detail: fmt.Sprintf("pane %s", dp.Key)}
			if dp.From > 0 {
				if from, ok := byKey[dw.Panes[dp.From-1].Key]; ok {
					op.Target = from.ID
				}
				op.Detail = fmt.Sprintf("pane %s: split %s %d%% of pane %d", dp.Key, dp.Split, dp.Size, dp.From)
			}
			ops = append(ops, op)
			continue
		}
		if !tp.Dead {
			continue // a live pane is never restarted, whatever it runs
		}
		switch {
		case !dp.HasRun():
			ops = append(ops, Op{Kind: KindReportDead, Project: d.ID, Target: tp.ID, WindowKey: dw.Key, PaneKey: dp.Key,
				Detail: fmt.Sprintf("pane %s exited (status %d) and has no command to restart", dp.Key, tp.DeadStatus)})
		case dp.Restart == workspace.RestartAlways || (dp.Restart == workspace.RestartOnFailure && tp.DeadStatus != 0):
			ops = append(ops, Op{Kind: KindRespawnPane, Project: d.ID, Target: tp.ID, WindowKey: dw.Key, PaneKey: dp.Key,
				Detail: fmt.Sprintf("pane %s exited (status %d), restart=%s: %s", dp.Key, tp.DeadStatus, dp.Restart, strings.Join(dp.Spec.Argv, " "))})
		default:
			ops = append(ops, Op{Kind: KindReportDead, Project: d.ID, Target: tp.ID, WindowKey: dw.Key, PaneKey: dp.Key,
				Detail: fmt.Sprintf("pane %s exited (status %d), restart=%s", dp.Key, tp.DeadStatus, dp.Restart)})
		}
	}
	if !changed || len(dw.Panes) < 2 && !foreign {
		return ops
	}
	switch s, err := dw.LayoutFor(layout.Size{W: tw.W, H: tw.H}); {
	case foreign:
		ops = append(ops, Op{Kind: KindSkipLayout, Project: d.ID, Target: tw.ID, WindowKey: dw.Key,
			Detail: fmt.Sprintf("window %s: layout not applied: it has panes cairn does not manage", dw.FullKey())})
	case err != nil:
		ops = append(ops, Op{Kind: KindSkipLayout, Project: d.ID, Target: tw.ID, WindowKey: dw.Key,
			Detail: fmt.Sprintf("window %s: layout not applied: %v", dw.FullKey(), err)})
	default:
		ops = append(ops, Op{Kind: KindSetLayout, Project: d.ID, Target: tw.ID, WindowKey: dw.Key, Detail: s})
	}
	return ops
}

// prunable reports whether an extra managed pane (one not in the config, so
// with no configured command) may be killed by --prune: it is alive, its
// current command is a shell and PaneIdle says it sits at its prompt.
// Decision E10.
func prunable(tp tmux.Pane, opts Options) bool {
	if tp.Dead || !tmux.IsShell(tp.Cmd) || opts.PaneIdle == nil {
		return false
	}
	idle, err := opts.PaneIdle(tp.PID)
	return err == nil && idle
}

func allPrunable(ps []tmux.Pane, opts Options) bool {
	for _, p := range ps {
		if !prunable(p, opts) {
			return false
		}
	}
	return true
}
