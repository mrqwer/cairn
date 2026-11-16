package reconcile

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

// devProject returns the defaulted "dev" template as project id, with a
// restart policy on the editor pane.
func devProject(id string) workspace.Project {
	p, _ := workspace.Template("dev")
	p.ID = id
	p.Root = "/home/u/" + id
	p.Windows[0].Panes[0].Restart = workspace.RestartOnFailure
	p.ApplyDefaults("/home/u")
	return p
}

func allRoots(string) bool { return true }

// live builds a tmux session that matches the desired project exactly:
// markers, traversal pane order, geometry of a 200x50 window.
func live(t *testing.T, p workspace.Project, sid int) tmux.Session {
	t.Helper()
	d := Desired(p, NewSize, allRoots)
	s := tmux.Session{ID: fmt.Sprintf("$%d", sid), Name: d.Session, Key: d.ID}
	for wi, w := range d.Windows {
		win := tmux.Window{ID: fmt.Sprintf("@%d%d", sid, wi), Key: w.FullKey(), Name: w.Spec.Name, Index: wi, W: NewSize.W, H: NewSize.H}
		rects := layout.Geometry(w.Tree, NewSize.W, NewSize.H)
		for i, k := range w.Order() {
			dp, _ := w.pane(k)
			r := rects[dp.Pos-1]
			cmd := "sh"
			if dp.HasRun() {
				cmd = dp.Spec.Argv[0]
			}
			win.Panes = append(win.Panes, tmux.Pane{
				ID: fmt.Sprintf("%%%d%d%d", sid, wi, dp.Pos), Key: k, Index: i, PID: 1000*sid + 100*wi + dp.Pos,
				Left: r.X, Top: r.Y, W: r.W, H: r.H, Cwd: dp.Spec.Cwd, Cmd: cmd,
			})
		}
		s.Windows = append(s.Windows, win)
	}
	return s
}

func kinds(ops []Op) string {
	var out []string
	for _, o := range ops {
		s := o.Project + ":" + o.Kind
		if o.WindowKey != "" {
			s += ":" + o.WindowKey
		}
		if o.PaneKey != "" {
			s += ":" + o.PaneKey
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func idleSet(pids ...int) func(int) (bool, error) {
	m := map[int]bool{}
	for _, p := range pids {
		m[p] = true
	}
	return func(pid int) (bool, error) { return m[pid], nil }
}

func TestPlan(t *testing.T) {
	demo := devProject("demo")
	web := devProject("web")
	paused := devProject("api")
	paused.Status = workspace.StatusPaused

	type tc struct {
		name string
		ps   []workspace.Project
		snap func() tmux.Snapshot
		opts Options
		want string
	}
	cases := []tc{
		{
			name: "empty snapshot creates the session",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot { return tmux.Snapshot{} },
			want: "demo:create-session:code",
		},
		{
			name: "matching snapshot is a no-op",
			ps:   []workspace.Project{demo, web},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{live(t, web, 2), live(t, demo, 1)}}
			},
			want: "",
		},
		{
			name: "missing second window",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows = s.Windows[:1]
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:create-window:run",
		},
		{
			name: "missing pane is created and laid out",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes = s.Windows[0].Panes[:1]
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:create-pane:code:demo/code/2 demo:set-layout:code",
		},
		{
			name: "dead pane on-failure with non-zero status is respawned",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes[0].Dead, s.Windows[0].Panes[0].DeadStatus = true, 2
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:respawn-pane:code:demo/code/1",
		},
		{
			name: "dead pane on-failure with status 0 is reported",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes[0].Dead = true
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:report-dead:code:demo/code/1",
		},
		{
			name: "dead pane always is respawned",
			ps: func() []workspace.Project {
				p := devProject("demo")
				p.Windows[0].Panes[0].Restart = workspace.RestartAlways
				return []workspace.Project{p}
			}(),
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes[0].Dead = true
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:respawn-pane:code:demo/code/1",
		},
		{
			name: "dead pane never is reported",
			ps: func() []workspace.Project {
				p := devProject("demo")
				p.Windows[0].Panes[0].Restart = workspace.RestartNever
				return []workspace.Project{p}
			}(),
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes[0].Dead, s.Windows[0].Panes[0].DeadStatus = true, 1
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:report-dead:code:demo/code/1",
		},
		{
			name: "dead pane without a configured command is reported",
			ps: func() []workspace.Project {
				p := devProject("demo")
				p.Windows[0].Panes[1].Restart = workspace.RestartAlways
				return []workspace.Project{p}
			}(),
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes[1].Dead, s.Windows[0].Panes[1].DeadStatus = true, 1
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:report-dead:code:demo/code/2",
		},
		{
			name: "extra managed window is reported",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{withExtraWindow(live(t, demo, 1))}}
			},
			opts: Options{PaneIdle: idleSet(9001)},
			want: "demo:report-extra:old",
		},
		{
			name: "prune kills an extra window of idle shells",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{withExtraWindow(live(t, demo, 1))}}
			},
			opts: Options{Prune: true, PaneIdle: idleSet(9001)},
			want: "demo:kill-window:old",
		},
		{
			name: "prune keeps an extra window with a busy pane",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{withExtraWindow(live(t, demo, 1))}}
			},
			opts: Options{Prune: true, PaneIdle: idleSet()},
			want: "demo:report-extra:old",
		},
		{
			name: "prune without an idle check never kills",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{withExtraWindow(live(t, demo, 1))}}
			},
			opts: Options{Prune: true},
			want: "demo:report-extra:old",
		},
		{
			name: "prune kills an idle extra pane and re-lays out",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{withExtraPane(live(t, demo, 1), "sh")}}
			},
			opts: Options{Prune: true, PaneIdle: idleSet(9002)},
			want: "demo:kill-pane:code:demo/code/3 demo:set-layout:code",
		},
		{
			name: "prune keeps an extra pane running a program even if reported idle",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{withExtraPane(live(t, demo, 1), "htop")}}
			},
			opts: Options{Prune: true, PaneIdle: idleSet(9002)},
			want: "demo:report-extra:code:demo/code/3",
		},
		{
			name: "E10: a configured pane with run is never pruned even when idle",
			ps: func() []workspace.Project {
				p := devProject("demo")
				p.Windows[0].Panes[0].Run = []string{"bash"}
				return []workspace.Project{p}
			}(),
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].Panes[0].Cmd = "bash"
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			opts: Options{Prune: true, PaneIdle: func(int) (bool, error) { return true, nil }},
			want: "",
		},
		{
			name: "unmarked window in a managed session is untouched",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows = append(s.Windows, tmux.Window{ID: "@99", Name: "mine", Index: 5, W: 200, H: 50,
					Panes: []tmux.Pane{{ID: "%99", PID: 9009, Cmd: "sh"}}})
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			opts: Options{Prune: true, PaneIdle: idleSet(9009)},
			want: "",
		},
		{
			name: "unmanaged pane in a managed window skips the layout",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				w := &s.Windows[0]
				w.Panes = []tmux.Pane{w.Panes[0], {ID: "%99", Index: 1, PID: 9009, Cmd: "sh"}}
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:create-pane:code:demo/code/2 demo:skip-layout:code",
		},
		{
			name: "window too small skips the layout",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows[0].W, s.Windows[0].H = 2, 2
				s.Windows[0].Panes = s.Windows[0].Panes[:1]
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "demo:create-pane:code:demo/code/2 demo:skip-layout:code",
		},
		{
			name: "unmanaged session with the same name is a clash",
			ps:   []workspace.Project{demo, web},
			snap: func() tmux.Snapshot {
				return tmux.Snapshot{Sessions: []tmux.Session{{ID: "$7", Name: "demo", Windows: []tmux.Window{{ID: "@70"}}}}}
			},
			want: "demo:skip-name-clash web:create-session:code",
		},
		{
			name: "paused project is skipped",
			ps:   []workspace.Project{paused, demo},
			snap: func() tmux.Snapshot { return tmux.Snapshot{} },
			want: "demo:create-session:code",
		},
		{
			name: "missing root skips only that project",
			ps:   []workspace.Project{web, demo},
			snap: func() tmux.Snapshot { return tmux.Snapshot{} },
			opts: Options{RootExists: func(p string) bool { return p != "/home/u/demo" }},
			want: "demo:skip-missing-root web:create-session:code",
		},
		{
			name: "missing root skips even an existing session",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Windows = s.Windows[:1]
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			opts: Options{RootExists: func(string) bool { return false }},
			want: "demo:skip-missing-root",
		},
		{
			name: "only filters one project",
			ps:   []workspace.Project{web, demo},
			snap: func() tmux.Snapshot { return tmux.Snapshot{} },
			opts: Options{Only: "web"},
			want: "web:create-session:code",
		},
		{
			name: "renamed managed session is matched by marker",
			ps:   []workspace.Project{demo},
			snap: func() tmux.Snapshot {
				s := live(t, demo, 1)
				s.Name = "renamed"
				return tmux.Snapshot{Sessions: []tmux.Session{s}}
			},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			if opts.RootExists == nil {
				opts.RootExists = allRoots
			}
			got := kinds(Plan(c.ps, c.snap(), opts))
			if got != c.want {
				t.Fatalf("ops\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

func withExtraWindow(s tmux.Session) tmux.Session {
	s.Windows = append(s.Windows, tmux.Window{ID: "@98", Key: s.Key + "/old", Name: "old", Index: 9, W: 200, H: 50,
		Panes: []tmux.Pane{{ID: "%98", Key: s.Key + "/old/1", PID: 9001, Cmd: "zsh"}}})
	return s
}

func withExtraPane(s tmux.Session, cmd string) tmux.Session {
	w := &s.Windows[0]
	w.Panes = append(w.Panes, tmux.Pane{ID: "%97", Key: w.Key + "/3", Index: len(w.Panes), PID: 9002, Cmd: cmd})
	return s
}

func TestPlanOpsCarryTargets(t *testing.T) {
	demo := devProject("demo")
	s := live(t, demo, 1)
	s.Windows[0].Panes = s.Windows[0].Panes[:1]
	s.Windows[1].Panes[0].Dead = true
	ops := Plan([]workspace.Project{demo}, tmux.Snapshot{Sessions: []tmux.Session{s}}, Options{RootExists: allRoots})
	if len(ops) != 3 {
		t.Fatalf("ops %+v", ops)
	}
	if ops[0].Kind != KindCreatePane || ops[0].Target != s.Windows[0].Panes[0].ID {
		t.Errorf("create-pane should split the from pane: %+v", ops[0])
	}
	if ops[1].Kind != KindSetLayout || ops[1].Target != s.Windows[0].ID {
		t.Errorf("set-layout targets the window: %+v", ops[1])
	}
	if ops[2].Kind != KindReportDead || ops[2].Target != s.Windows[1].Panes[0].ID || ops[2].Destructive {
		t.Errorf("report-dead: %+v", ops[2])
	}
	for _, o := range ops {
		if o.Destructive {
			t.Errorf("non-prune op is destructive: %+v", o)
		}
	}
}

func TestPlanKillIsDestructive(t *testing.T) {
	demo := devProject("demo")
	ops := Plan([]workspace.Project{demo}, tmux.Snapshot{Sessions: []tmux.Session{withExtraWindow(live(t, demo, 1))}},
		Options{Prune: true, RootExists: allRoots, PaneIdle: idleSet(9001)})
	if len(ops) != 1 || ops[0].Kind != KindKillWindow || !ops[0].Destructive || ops[0].Target != "@98" {
		t.Fatalf("ops %+v", ops)
	}
}

func TestPlanNewSessionInfeasibleLayout(t *testing.T) {
	p := devProject("demo")
	var panes []workspace.Pane
	panes = append(panes, workspace.Pane{})
	for range 120 {
		panes = append(panes, workspace.Pane{Split: workspace.SplitH, Size: 50})
	}
	p.Windows[1].Panes = panes
	p.ApplyDefaults("/home/u")
	got := kinds(Plan([]workspace.Project{p}, tmux.Snapshot{}, Options{RootExists: allRoots}))
	if got != "demo:create-session:code demo:skip-layout:run" {
		t.Fatalf("ops %s", got)
	}
}

func TestDesired(t *testing.T) {
	d := Desired(devProject("demo"), NewSize, allRoots)
	if d.ID != "demo" || d.Session != "demo" || d.RootMissing || len(d.Windows) != 2 {
		t.Fatalf("desired %+v", d)
	}
	code := d.Windows[0]
	if code.Spec.Key != "demo/code" || code.Spec.Name != "code" || code.Spec.PaneSpec.Key != "demo/code/1" {
		t.Errorf("window spec %+v", code.Spec)
	}
	ed := code.Panes[0]
	if ed.Spec.Cwd != "/home/u/demo" || strings.Join(ed.Spec.Argv, " ") != "nvim" || !ed.Spec.RemainOnExit || ed.Restart != workspace.RestartOnFailure {
		t.Errorf("editor %+v", ed)
	}
	sh := code.Panes[1]
	if sh.Key != "demo/code/2" || sh.Split != "h" || sh.From != 1 || sh.Size != 35 || sh.Spec.RemainOnExit || sh.HasRun() {
		t.Errorf("shell %+v", sh)
	}
	want, err := layout.TmuxString(code.Tree, 200, 50)
	if err != nil || code.Layout != want || code.LayoutErr != nil {
		t.Errorf("layout %q want %q (%v)", code.Layout, want, err)
	}
	if !Desired(devProject("demo"), NewSize, func(string) bool { return false }).RootMissing {
		t.Error("root missing not detected")
	}
}
