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
	KindCreateSession = "create-session"
	KindCreateWindow  = "create-window"
	KindCreatePane    = "create-pane"
	KindSetLayout     = "set-layout"
	KindRespawnPane   = "respawn-pane"
	// KindRekeyPanes moves the @cairn_p markers of a window's panes so that
	// agent panes, matched by their @cairn_agent identity, carry the key of
	// their conversation's position, and moves an unmatched pane off a
	// configured key (to "<window>/orphan-<n>").
	KindRekeyPanes      = "rekey-panes"
	KindKillWindow      = "kill-window"
	KindKillPane        = "kill-pane"
	KindReportExtra     = "report-extra"
	KindReportDead      = "report-dead"
	KindSkipMissingRoot = "skip-missing-root"
	KindSkipNameClash   = "skip-name-clash"
	KindSkipLayout      = "skip-layout"
	KindSkipInvalid     = "skip-invalid"
	// KindNoteMissingCwd says a pane about to be created or restarted has a
	// cwd that does not exist; tmux then starts it in the home directory
	// (verified on tmux 3.8, for new-session, new-window and split-window).
	KindNoteMissingCwd = "note-missing-cwd"
)

// Op is one planned change (or note). Target is a tmux id for existing
// objects ($n, @n, %n), the session name for create-session and the root for
// skip-missing-root; WindowKey is the window key in config and PaneKey the
// full @cairn_p marker.
type Op struct {
	Kind, Project, Target, Detail string
	WindowKey, PaneKey            string
	Destructive                   bool
	// Agent is the conversation of an agent pane a report-extra is about.
	Agent string
}

// IsNote reports whether the op only reports something.
func (o Op) IsNote() bool {
	switch o.Kind {
	case KindReportExtra, KindReportDead, KindSkipMissingRoot, KindSkipNameClash, KindSkipLayout, KindSkipInvalid, KindNoteMissingCwd:
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
// order. It never touches anything without a cairn marker. Projects must be
// defaulted and valid (workspace.Project.Validate); an invalid one gets a
// single skip-invalid note.
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
	if err := validate(p); err != nil {
		return []Op{{Kind: KindSkipInvalid, Project: p.ID, Detail: err.Error()}}
	}
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
			ops = append(ops, missingCwdNotes(d, w, w.Panes, d.Root, opts)...)
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
			ops = append(ops, missingCwdNotes(d, dw, dw.Panes, d.Root, opts)...)
			continue
		}
		ops = append(ops, planWindow(d, dw, tw, opts)...)
	}
	for _, w := range extras {
		k := strings.TrimPrefix(w.Key, prefix)
		if opts.Prune && windowPrunable(w, opts) {
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

// missingCwdNotes returns a note for each of panes whose cwd is not an
// existing directory. The root is skipped: a missing root is a skip-missing-root
// of the whole project.
func missingCwdNotes(d DesiredProject, w DesiredWindow, panes []DesiredPane, root string, opts Options) []Op {
	exists := opts.RootExists
	if exists == nil {
		exists = dirExists
	}
	var ops []Op
	for _, p := range panes {
		cwd := p.Spec.Cwd
		if cwd == "" || cwd == root || exists(cwd) {
			continue
		}
		ops = append(ops, Op{Kind: KindNoteMissingCwd, Project: d.ID, Target: cwd, WindowKey: w.Key, PaneKey: p.Key,
			Detail: fmt.Sprintf("pane %s: cwd %s does not exist; the pane is still created and tmux starts it in the home directory", p.Key, cwd)})
	}
	return ops
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
	byKey, extras, rekey := matchWindow(dw, tw)
	foreign := false // a pane that will stay but is not in the config
	changed := rekey
	prefix := dw.FullKey() + "/"
	for _, tp := range tw.Panes {
		if !strings.HasPrefix(tp.Key, prefix) {
			foreign = true // the user's own pane (or another window's marker): left alone
		}
	}
	if rekey {
		ops = append(ops, Op{Kind: KindRekeyPanes, Project: d.ID, Target: tw.ID, WindowKey: dw.Key,
			Detail: fmt.Sprintf("window %s: move pane markers to match agent panes by conversation", dw.FullKey())})
	}
	for _, tp := range extras {
		_, configured := dw.pane(tp.Key)
		// With no configured pane left in the window, an extra pane may be
		// its last one: it stays this round and the missing panes are
		// created first.
		if configured || !opts.Prune || len(byKey) == 0 || !prunable(tp, opts) {
			foreign = true
			detail := fmt.Sprintf("pane %s (%s) is not in the config", tp.Key, tp.ID)
			if tp.Agent != "" {
				detail = fmt.Sprintf("pane %s (%s) of conversation %s is not in the config", tp.Key, tp.ID, tp.Agent)
			}
			ops = append(ops, Op{Kind: KindReportExtra, Project: d.ID, Target: tp.ID, WindowKey: dw.Key, PaneKey: tp.Key, Detail: detail, Agent: tp.Agent})
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
			ops = append(ops, missingCwdNotes(d, dw, []DesiredPane{dp}, d.Root, opts)...)
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

// matchWindow pairs the managed panes of tw with the configured panes of
// dw. An agent pane is matched only by its @cairn_agent identity, wherever
// its key points; any other pane only by key, and never to an agent pane's
// key. Unmatched managed panes are extras. rekey is true when a matched
// pane's key differs from its configured one, or an extra sits on a
// configured key: the markers must be moved before anything else.
func matchWindow(dw DesiredWindow, tw tmux.Window) (byKey map[string]tmux.Pane, extras []tmux.Pane, rekey bool) {
	prefix := dw.FullKey() + "/"
	byKey = map[string]tmux.Pane{}
	byAgent := map[string]DesiredPane{}
	for _, dp := range dw.Panes {
		if dp.Spec.Agent != "" {
			byAgent[dp.Spec.Agent] = dp
		}
	}
	for _, tp := range tw.Panes {
		if !strings.HasPrefix(tp.Key, prefix) {
			continue
		}
		var dp DesiredPane
		var ok bool
		if tp.Agent != "" {
			dp, ok = byAgent[tp.Agent]
		} else if dp, ok = dw.pane(tp.Key); ok && dp.Spec.Agent != "" {
			ok = false
		}
		if _, seen := byKey[dp.Key]; ok && !seen {
			byKey[dp.Key] = tp
			rekey = rekey || tp.Key != dp.Key
			continue
		}
		extras = append(extras, tp)
		if _, configured := dw.pane(tp.Key); configured {
			rekey = true
		}
	}
	return byKey, extras, rekey
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

// windowPrunable reports whether an extra managed window may be killed by
// --prune: it has panes, every one carries this window's marker prefix (a
// user's pane, or one of another window or project, is never taken along)
// and every one is prunable.
func windowPrunable(w tmux.Window, opts Options) bool {
	if len(w.Panes) == 0 {
		return false
	}
	for _, p := range w.Panes {
		if !strings.HasPrefix(p.Key, w.Key+"/") || !prunable(p, opts) {
			return false
		}
	}
	return true
}

// validate checks the precondition of Plan and Apply: a defaulted, valid
// project (at least one window, each with at least one pane, a valid split
// sequence).
func validate(p workspace.Project) error {
	errs := p.Validate()
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("invalid project %s: %s", p.ID, strings.Join(msgs, "; "))
}
