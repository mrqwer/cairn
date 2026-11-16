package capture

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/workspace/layout"
)

const winW, winH = 200, 50

// lp describes one pane of a live window by its config position.
type lp struct {
	cwd, cmd string
	pid      int
	dead     bool
}

// liveWindow lays the config split sequence out like tmux would: geometry
// from layout.Geometry, panes listed in traversal order (pane_index). Pane
// details are given by config position.
func liveWindow(t *testing.T, name string, cfg []workspace.Pane, ps []lp) tmux.Window {
	t.Helper()
	tree, err := layout.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rects := layout.Geometry(tree, winW, winH)
	w := tmux.Window{ID: "@1", Name: name, W: winW, H: winH}
	for idx, pos := range layout.Order(tree) {
		r, d := rects[pos-1], ps[pos-1]
		w.Panes = append(w.Panes, tmux.Pane{ID: "%" + string(rune('a'+idx)), Index: idx, PID: d.pid,
			Left: r.X, Top: r.Y, W: r.W, H: r.H, Cwd: d.cwd, Cmd: d.cmd, Dead: d.dead})
	}
	return w
}

func shells(cwds ...string) []lp {
	out := make([]lp, len(cwds))
	for i, c := range cwds {
		out[i] = lp{cwd: c, cmd: "zsh", pid: 100 + i}
	}
	return out
}

func session(name string, ws ...tmux.Window) tmux.Session {
	return tmux.Session{ID: "$1", Name: name, Windows: ws}
}

func mustCapture(t *testing.T, s tmux.Session, opts Options) (workspace.Project, []string) {
	t.Helper()
	p, notes, err := Session(s, opts)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return p, notes
}

// layoutOf strips everything but the split sequence of a window.
func layoutOf(w workspace.Window) []workspace.Pane {
	out := make([]workspace.Pane, len(w.Panes))
	for i, p := range w.Panes {
		out[i] = workspace.Pane{Split: p.Split, From: p.From, Size: p.Size}
	}
	return out
}

func sameLayout(t *testing.T, got, want []workspace.Pane, tol int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d panes %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range got {
		g, w := got[i], want[i]
		wf, ws := w.From, w.Size
		if i > 0 && wf == 0 {
			wf = i
		}
		if i > 0 && ws == 0 {
			ws = workspace.DefaultSize
		}
		if g.Split != w.Split || g.From != wf || g.Size < ws-tol || g.Size > ws+tol {
			t.Fatalf("pane %d: got %+v, want %+v (±%d)\nall: %+v", i+1, g, w, tol, got)
		}
	}
}

func TestSinglePane(t *testing.T) {
	s := session("demo", liveWindow(t, "main", []workspace.Pane{{}}, shells("/home/u/demo")))
	p, notes := mustCapture(t, s, Options{})
	if p.ID != "demo" || p.Name != "demo" || p.Root != "/home/u/demo" || p.Session != "" {
		t.Fatalf("project: %+v", p)
	}
	if len(p.Windows) != 1 || p.Windows[0].Key != "main" || p.Windows[0].Name != "main" {
		t.Fatalf("windows: %+v", p.Windows)
	}
	want := []workspace.Pane{{Role: workspace.RoleShell}}
	if !reflect.DeepEqual(p.Windows[0].Panes, want) {
		t.Fatalf("panes: %+v", p.Windows[0].Panes)
	}
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
}

func TestSplitSequences(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  []workspace.Pane
		// canonical: the config is already in the top-down order capture
		// emits, so the sequence itself must come back.
		canonical bool
	}{
		{"side by side 35/65", []workspace.Pane{{}, {Split: "h", Size: 65}}, true},
		{"h then v", []workspace.Pane{{}, {Split: "h", From: 1, Size: 40}, {Split: "v", From: 2, Size: 50}}, true},
		{"v then h from first", []workspace.Pane{{}, {Split: "v", From: 1, Size: 30}, {Split: "h", From: 1, Size: 25}}, true},
		{"four deep", []workspace.Pane{{}, {Split: "h", From: 1, Size: 60}, {Split: "v", From: 2, Size: 50}, {Split: "h", From: 2, Size: 30}, {Split: "v", From: 1, Size: 20}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwds := make([]string, len(tc.cfg))
			for i := range cwds {
				cwds[i] = "/home/u/demo"
			}
			p, notes := mustCapture(t, session("demo", liveWindow(t, "code", tc.cfg, shells(cwds...))), Options{})
			got := layoutOf(p.Windows[0])
			if tc.canonical {
				sameLayout(t, got, tc.cfg, 1)
			}
			sameGeometry(t, got, tc.cfg)
			if len(notes) != 0 {
				t.Fatalf("notes: %v", notes)
			}
		})
	}
}

// sameGeometry compares the pane rects of two split sequences as sets,
// within the FromRects precision (position ±1, size ±2).
func sameGeometry(t *testing.T, got, want []workspace.Pane) {
	t.Helper()
	rects := func(ps []workspace.Pane) []layout.Rect {
		tree, err := layout.Build(ps)
		if err != nil {
			t.Fatal(err)
		}
		rs := layout.Geometry(tree, winW, winH)
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].X != rs[j].X {
				return rs[i].X < rs[j].X
			}
			return rs[i].Y < rs[j].Y
		})
		return rs
	}
	g, w := rects(got), rects(want)
	for i := range w {
		d := func(a, b, tol int) bool { return a-b > tol || b-a > tol }
		if d(g[i].X, w[i].X, 1) || d(g[i].Y, w[i].Y, 1) || d(g[i].W, w[i].W, 2) || d(g[i].H, w[i].H, 2) {
			t.Fatalf("geometry differs:\n got %v\nwant %v", g, w)
		}
	}
}

// A pane split from a non-last pane: tmux lists it out of creation order,
// so a mapping that pairs config panes with tmux panes by index would give
// the panes the wrong directories.
func TestAsymmetricMappingKeepsPaneDetails(t *testing.T) {
	cfg := []workspace.Pane{{}, {Split: "v", From: 1, Size: 40}, {Split: "h", From: 1, Size: 30}}
	ps := []lp{
		{cwd: "/home/u/demo", cmd: "zsh", pid: 1},
		{cwd: "/home/u/demo/bottom", cmd: "npm", pid: 2},
		{cwd: "/home/u/demo/right", cmd: "nvim", pid: 3},
	}
	w := liveWindow(t, "code", cfg, ps)
	if w.Panes[1].Cwd != "/home/u/demo/right" {
		t.Fatalf("fixture: tmux index 1 should be the top right pane: %+v", w.Panes)
	}
	p, notes := mustCapture(t, session("demo", w), Options{})
	sameLayout(t, layoutOf(p.Windows[0]), cfg, 1)
	got := p.Windows[0].Panes
	if got[0].Cwd != "" || got[1].Cwd != "bottom" || got[2].Cwd != "right" {
		t.Fatalf("cwds: %q %q %q", got[0].Cwd, got[1].Cwd, got[2].Cwd)
	}
	if got[1].Role != workspace.RoleCommand || got[2].Role != workspace.RoleEditor || !reflect.DeepEqual(got[2].Run, []string{"nvim"}) {
		t.Fatalf("roles: %+v", got)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `window code pane 2: running "npm"`) {
		t.Fatalf("notes: %v", notes)
	}
}

func TestRoles(t *testing.T) {
	cfg := []workspace.Pane{{}, {Split: "h"}, {Split: "h"}, {Split: "h"}, {Split: "h"}, {Split: "h"}}
	ps := []lp{
		{cwd: "/home/u/demo", cmd: "zsh", pid: 10},              // idle shell
		{cwd: "/home/u/demo", cmd: "nvim", pid: 11},             // editor
		{cwd: "/home/u/demo", cmd: "npm", pid: 12},              // command
		{cwd: "/home/u/demo", cmd: "bash", pid: 13},             // busy shell
		{cwd: "/home/u/demo", cmd: "hx", pid: 14},               // editor
		{cwd: "/home/u/demo", cmd: "make", pid: 15, dead: true}, // dead
	}
	idle := func(pid int) bool { return pid == 10 }
	p, notes := mustCapture(t, session("demo", liveWindow(t, "code", cfg, ps)), Options{IsIdle: idle})
	got := p.Windows[0].Panes
	wantRoles := []string{"shell", "editor", "command", "command", "editor", "shell"}
	for i, r := range wantRoles {
		if got[i].Role != r {
			t.Fatalf("pane %d role %q, want %q (%+v)", i+1, got[i].Role, r, got)
		}
	}
	if !reflect.DeepEqual(got[1].Run, []string{"nvim"}) || !reflect.DeepEqual(got[4].Run, []string{"hx"}) {
		t.Fatalf("editor run: %v %v", got[1].Run, got[4].Run)
	}
	for _, i := range []int{0, 2, 3, 5} {
		if got[i].Run != nil {
			t.Fatalf("pane %d has run %v; argv must never be guessed", i+1, got[i].Run)
		}
	}
	want := []string{
		`window code pane 3: running "npm" — add run = [...] to restart it automatically`,
		`window code pane 4: running "bash" — add run = [...] to restart it automatically`,
		`window code pane 6: the pane is dead; captured as a shell`,
	}
	if !reflect.DeepEqual(notes, want) {
		t.Fatalf("notes:\n%s\nwant:\n%s", strings.Join(notes, "\n"), strings.Join(want, "\n"))
	}

	// Without an idle check every shell counts as a shell.
	p, _ = mustCapture(t, session("demo", liveWindow(t, "code", cfg, ps)), Options{})
	if p.Windows[0].Panes[3].Role != workspace.RoleShell {
		t.Fatalf("nil IsIdle: %+v", p.Windows[0].Panes[3])
	}
}

func TestCwdRelativeToRoot(t *testing.T) {
	cfg := []workspace.Pane{{}, {Split: "h"}, {Split: "h"}, {Split: "h"}, {Split: "h"}}
	ps := shells("/home/u/demo", "/home/u/demo/web app", "/home/u/demo-other", "/srv/data", "/home/u/demo/gone/../api")
	p, _ := mustCapture(t, session("demo", liveWindow(t, "main", cfg, ps)), Options{Root: "/home/u/demo/"})
	if p.Root != "/home/u/demo" {
		t.Fatalf("root %q", p.Root)
	}
	var got []string
	for _, pn := range p.Windows[0].Panes {
		got = append(got, pn.Cwd)
	}
	want := []string{"", "web app", "/home/u/demo-other", "/srv/data", "api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cwds %q, want %q", got, want)
	}
}

func TestDefaultRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		cwds []string
		want string
	}{
		{"majority subtree", []string{"/home/u/demo", "/home/u/demo/web", "/home/u/demo/api", "/tmp"}, "/home/u/demo"},
		{"common parent", []string{"/home/u/demo/web", "/home/u/demo/api"}, "/home/u/demo"},
		{"deepest majority", []string{"/home/u/demo/web", "/home/u/demo/web/src", "/home/u/demo/api"}, "/home/u/demo/web"},
		{"wide disagreement", []string{"/srv/a", "/home/u/b", "/opt/c"}, "/srv/a"},
		{"boundary aware", []string{"/home/u/ab", "/home/u/a", "/home/u/a"}, "/home/u/a"},
		{"dead pane without cwd", []string{"", "/home/u/demo"}, "/home/u/demo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := []workspace.Pane{{}}
			for range tc.cwds[1:] {
				cfg = append(cfg, workspace.Pane{Split: "h"})
			}
			p, _ := mustCapture(t, session("demo", liveWindow(t, "main", cfg, shells(tc.cwds...))), Options{})
			if p.Root != tc.want {
				t.Fatalf("root %q, want %q", p.Root, tc.want)
			}
		})
	}
	// Across windows: the first pane of the first window breaks a wide
	// disagreement.
	s := session("demo",
		liveWindow(t, "a", []workspace.Pane{{}}, shells("/opt/x")),
		liveWindow(t, "b", []workspace.Pane{{}}, shells("/srv/y")))
	if p, _ := mustCapture(t, s, Options{}); p.Root != "/opt/x" {
		t.Fatalf("root %q", p.Root)
	}
}

func TestWindowKeys(t *testing.T) {
	one := []workspace.Pane{{}}
	names := []string{"code", "Code", "code", "日本語", "!!!", "window-4", "run"}
	var ws []tmux.Window
	for _, n := range names {
		ws = append(ws, liveWindow(t, n, one, shells("/home/u/demo")))
	}
	p, _ := mustCapture(t, session("demo", ws...), Options{})
	var keys, gotNames []string
	for _, w := range p.Windows {
		keys = append(keys, w.Key)
		gotNames = append(gotNames, w.Name)
	}
	wantKeys := []string{"code", "code-2", "code-3", "window-4", "window-5", "window-4-2", "run"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("keys %q, want %q", keys, wantKeys)
	}
	if !reflect.DeepEqual(gotNames, names) {
		t.Fatalf("names %q, want %q", gotNames, names)
	}
}

func TestLongWindowNameCollision(t *testing.T) {
	long := strings.Repeat("a", 70)
	one := []workspace.Pane{{}}
	p, _ := mustCapture(t, session("demo",
		liveWindow(t, long, one, shells("/home/u/demo")),
		liveWindow(t, long, one, shells("/home/u/demo"))), Options{})
	k := p.Windows[1].Key
	if len(k) > 63 || !strings.HasSuffix(k, "-2") || k == p.Windows[0].Key {
		t.Fatalf("keys %q %q", p.Windows[0].Key, k)
	}
}

// pinwheel is five panes around a centre: no full-length cut separates them.
func pinwheel() tmux.Window {
	rs := [][4]int{{0, 0, 13, 6}, {14, 0, 7, 13}, {8, 7, 5, 6}, {0, 7, 7, 14}, {8, 14, 13, 7}}
	w := tmux.Window{ID: "@2", Name: "wheel", W: 21, H: 21}
	for i, r := range rs {
		w.Panes = append(w.Panes, tmux.Pane{Index: i, PID: 200 + i, Left: r[0], Top: r[1], W: r[2], H: r[3],
			Cwd: "/home/u/demo/p" + string(rune('0'+i)), Cmd: "zsh"})
	}
	return w
}

func TestUnreconstructableLayoutFallsBack(t *testing.T) {
	p, notes := mustCapture(t, session("demo", pinwheel()), Options{Root: "/home/u/demo"})
	panes := p.Windows[0].Panes
	if len(panes) != 5 {
		t.Fatalf("panes: %+v", panes)
	}
	for i, pn := range panes {
		if pn.Cwd != "p"+string(rune('0'+i)) {
			t.Fatalf("pane %d cwd %q: fallback must keep index order", i+1, pn.Cwd)
		}
		if i == 0 {
			continue
		}
		if pn.Split != "h" || pn.From != i || pn.Size != 50 {
			t.Fatalf("pane %d: %+v", i+1, pn)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `window wheel`) || !strings.Contains(notes[0], "no clean split") {
		t.Fatalf("notes: %v", notes)
	}
	if errs := defaulted(p).Validate(); len(errs) != 0 {
		t.Fatalf("invalid: %v", errs)
	}
}

func defaulted(p workspace.Project) workspace.Project {
	data, err := workspace.Marshal(p)
	if err != nil {
		panic(err)
	}
	q, err := workspace.Decode(data)
	if err != nil {
		panic(err)
	}
	q.ApplyDefaults("/home/u")
	return q
}

// The dev template as tmux would show it: captured, it round-trips to an
// equivalent project.
func TestDevTemplateRoundTrip(t *testing.T) {
	tpl, _ := workspace.Template("dev")
	tpl.ID, tpl.Name, tpl.Root = "demo", "demo", "/home/u/demo"
	code := liveWindow(t, "code", tpl.Windows[0].Panes, []lp{{cwd: "/home/u/demo", cmd: "nvim", pid: 1}, {cwd: "/home/u/demo", cmd: "zsh", pid: 2}})
	run := liveWindow(t, "run", tpl.Windows[1].Panes, shells("/home/u/demo"))
	run.ID = "@2"
	p, notes := mustCapture(t, session("demo", code, run), Options{IsIdle: func(int) bool { return true }})
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
	got, want := defaulted(p), defaulted(tpl)
	if errs := got.Validate(); len(errs) != 0 {
		t.Fatalf("invalid: %v", errs)
	}
	if len(got.Windows) != len(want.Windows) {
		t.Fatalf("windows: %+v", got.Windows)
	}
	for wi := range want.Windows {
		gw, ww := got.Windows[wi], want.Windows[wi]
		if gw.Key != ww.Key || gw.Name != ww.Name {
			t.Fatalf("window %d: %+v vs %+v", wi, gw, ww)
		}
		sameLayout(t, layoutOf(gw), layoutOf(ww), 1)
		for i := range ww.Panes {
			g, w := gw.Panes[i], ww.Panes[i]
			if g.Role != w.Role || g.Cwd != w.Cwd || !reflect.DeepEqual(g.Run, w.Run) || g.Restart != w.Restart {
				t.Fatalf("window %s pane %d: %+v vs %+v", ww.Key, i+1, g, w)
			}
		}
	}
}

func TestMarshalledOutputValidates(t *testing.T) {
	s := session("demo",
		liveWindow(t, "code", []workspace.Pane{{}, {Split: "h", Size: 35}}, []lp{{cwd: "/home/u/demo", cmd: "nvim"}, {cwd: "/home/u/demo/web app", cmd: "npm"}}),
		pinwheel())
	p, _ := mustCapture(t, s, Options{Root: "/home/u/demo"})
	if errs := defaulted(p).Validate(); len(errs) != 0 {
		t.Fatalf("invalid: %v", errs)
	}
	data, _ := workspace.Marshal(p)
	if !strings.Contains(string(data), `cwd = "web app"`) {
		t.Fatalf("toml:\n%s", data)
	}
}

func TestProjectIdentity(t *testing.T) {
	w := liveWindow(t, "main", []workspace.Pane{{}}, shells("/home/u/demo"))

	// --id different from the session name keeps the session name.
	p, _ := mustCapture(t, session("web", w), Options{ID: "web-copy"})
	if p.ID != "web-copy" || p.Session != "web" || p.Name != "web" {
		t.Fatalf("project: %+v", p)
	}
	data, _ := workspace.Marshal(p)
	if !strings.Contains(string(data), `session = "web"`) {
		t.Fatalf("toml:\n%s", data)
	}

	// A session name that is not a valid cairn session name is dropped
	// with a note; the id is its slug.
	p, notes := mustCapture(t, session("My Web", w), Options{})
	if p.ID != "my-web" || p.Session != "" || p.Name != "My Web" {
		t.Fatalf("project: %+v", p)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"My Web"`) || !strings.Contains(notes[0], "my-web") {
		t.Fatalf("notes: %v", notes)
	}

	// A name that slugs to nothing needs --id.
	if _, _, err := Session(session("日本語", w), Options{}); err == nil || !strings.Contains(err.Error(), "--id") {
		t.Fatalf("empty slug: %v", err)
	}
	if p, _ := mustCapture(t, session("日本語", w), Options{ID: "jp"}); p.ID != "jp" || p.Session != "" {
		t.Fatalf("project: %+v", p)
	}
}

func TestRefusals(t *testing.T) {
	w := liveWindow(t, "main", []workspace.Pane{{}}, shells("/home/u/demo"))
	managed := session("demo", w)
	managed.Key = "demo"
	if _, _, err := Session(managed, Options{}); err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("managed: %v", err)
	}
	if _, _, err := Session(session("demo", w), Options{ID: "Bad Id"}); err == nil || !strings.Contains(err.Error(), "id") {
		t.Fatalf("bad id: %v", err)
	}
	if _, _, err := Session(session("demo", w), Options{Root: "rel/dir"}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative root: %v", err)
	}
	if _, _, err := Session(session("demo"), Options{}); err == nil {
		t.Fatal("session without windows accepted")
	}
}

// A pane whose directory no longer exists is captured as it is: capture
// never looks at the filesystem.
func TestMissingCwdStillCaptured(t *testing.T) {
	p, _ := mustCapture(t, session("demo", liveWindow(t, "main", []workspace.Pane{{}, {Split: "h"}},
		shells("/home/u/demo", "/home/u/demo/deleted dir"))), Options{})
	if p.Windows[0].Panes[1].Cwd != "deleted dir" {
		t.Fatalf("panes: %+v", p.Windows[0].Panes)
	}
}

// The returned project is undefaulted (decision E3): validation must not
// leak defaults into it.
func TestResultIsUndefaulted(t *testing.T) {
	p, _ := mustCapture(t, session("demo", liveWindow(t, "main", []workspace.Pane{{}, {Split: "h"}}, shells("/home/u/demo", "/home/u/demo"))), Options{})
	if p.Status != "" || p.Session != "" || p.Windows[0].Panes[0].Restart != "" || p.Windows[0].Panes[1].Cwd != "" {
		t.Fatalf("defaults leaked: %+v", p)
	}
}
