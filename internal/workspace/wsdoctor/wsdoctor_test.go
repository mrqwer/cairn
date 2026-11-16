package wsdoctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/tmux"
	"github.com/mrqwer/cairn/internal/workspace"
)

func proj(id, root string) workspace.Project {
	p := workspace.Project{ID: id, Root: root, Windows: []workspace.Window{{Name: "main"}}}
	p.ApplyDefaults("/home/u")
	return p
}

func exists(string) bool { return true }

func byID(t *testing.T, cs []doctor.Check, id string) doctor.Result {
	t.Helper()
	for _, c := range cs {
		if c.ID() == id {
			r := c.Run(t.Context())
			if r.ID == "" {
				r.ID = id
			}
			return r
		}
	}
	t.Fatalf("no check %s", id)
	return doctor.Result{}
}

func TestTmuxVersion(t *testing.T) {
	for _, tc := range []struct {
		ver  string
		want doctor.Status
	}{
		{"3.1", doctor.Warn}, {"next-3.8", doctor.OK}, {"3.3a", doctor.OK}, {"3.8", doctor.OK},
		{"tmux 3.2", doctor.OK}, {"2.9", doctor.Warn}, {"", doctor.Crit}, {"master", doctor.Warn},
	} {
		r := byID(t, Checks(nil, nil, tmux.Snapshot{}, tc.ver, exists), IDTmux)
		if r.Status != tc.want {
			t.Errorf("%q: got %v (%s), want %v", tc.ver, r.Status, r.Evidence, tc.want)
		}
	}
	if r := byID(t, Checks(nil, nil, tmux.Snapshot{}, "", exists), IDTmux); r.Fix == "" {
		t.Error("missing tmux has no fix")
	}
}

func TestConfig(t *testing.T) {
	if r := byID(t, Checks(nil, nil, tmux.Snapshot{}, "3.4", exists), IDConfig); r.Status != doctor.OK {
		t.Fatalf("clean: %+v", r)
	}
	errs := []workspace.LoadError{
		{File: "projects/a.toml", Err: errors.New("unknown key foo")},
		{File: "projects/b.toml", Err: errors.New("root: must be absolute")},
	}
	r := byID(t, Checks(nil, errs, tmux.Snapshot{}, "3.4", exists), IDConfig)
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "projects/a.toml") ||
		!strings.Contains(r.Evidence, "unknown key foo") || !strings.Contains(r.Evidence, "projects/b.toml") {
		t.Fatalf("%+v", r)
	}
	bad := proj("api", "/srv/api")
	bad.Root = "relative"
	r = byID(t, Checks([]workspace.Project{bad}, nil, tmux.Snapshot{}, "3.4", exists), IDConfig)
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "projects/api.toml") || !strings.Contains(r.Evidence, "root") {
		t.Fatalf("validation: %+v", r)
	}
}

func TestRoots(t *testing.T) {
	ps := []workspace.Project{proj("api", "/srv/api"), proj("web", "/srv/web")}
	r := byID(t, Checks(ps, nil, tmux.Snapshot{}, "3.4", exists), IDRoots)
	if r.Status != doctor.OK {
		t.Fatalf("%+v", r)
	}
	r = byID(t, Checks(ps, nil, tmux.Snapshot{}, "3.4", func(p string) bool { return p != "/srv/web" }), IDRoots)
	if r.Status != doctor.Warn || !strings.Contains(r.Evidence, "web") || !strings.Contains(r.Evidence, "/srv/web") ||
		strings.Contains(r.Evidence, "/srv/api") {
		t.Fatalf("%+v", r)
	}
	archived := proj("old", "/srv/old")
	archived.Status = workspace.StatusArchived
	r = byID(t, Checks([]workspace.Project{archived}, nil, tmux.Snapshot{}, "3.4", func(string) bool { return false }), IDRoots)
	if r.Status != doctor.OK {
		t.Fatalf("archived project must be ignored: %+v", r)
	}
}

func TestDrift(t *testing.T) {
	api, web := proj("api", "/srv/api"), proj("web", "/srv/web")
	ps := []workspace.Project{api, web}
	r := byID(t, Checks(ps, nil, tmux.Snapshot{}, "3.4", exists), IDDrift)
	if r.Status != doctor.Warn || !strings.Contains(r.Evidence, "2 projects need `cairn apply`") {
		t.Fatalf("both missing: %+v", r)
	}
	// api is fully present, web is missing.
	snap := tmux.Snapshot{Sessions: []tmux.Session{{
		ID: "$1", Name: "api", Key: "api",
		Windows: []tmux.Window{{ID: "@1", Key: "api/main", Name: "main", W: 200, H: 50, Panes: []tmux.Pane{
			{ID: "%1", Key: "api/main/1", W: 200, H: 50, Cwd: "/srv/api", Cmd: "bash"},
		}}},
	}}}
	r = byID(t, Checks(ps, nil, snap, "3.4", exists), IDDrift)
	if r.Status != doctor.Warn || !strings.Contains(r.Evidence, "1 project needs `cairn apply`") {
		t.Fatalf("one drifting: %+v", r)
	}
	if r = byID(t, Checks([]workspace.Project{api}, nil, snap, "3.4", exists), IDDrift); r.Status != doctor.OK {
		t.Fatalf("in sync: %+v", r)
	}
	if r = byID(t, Checks(nil, nil, tmux.Snapshot{}, "3.4", exists), IDDrift); r.Status != doctor.OK {
		t.Fatalf("no projects: %+v", r)
	}
	if r = byID(t, Checks(ps, nil, tmux.Snapshot{}, "", exists), IDDrift); r.Status != doctor.Unknown {
		t.Fatalf("no tmux: %+v", r)
	}
}

func TestServiceCheck(t *testing.T) {
	run := func(u []workspace.UnitStatus) doctor.Result { return ServiceCheck(u).Run(t.Context()) }
	r := run([]workspace.UnitStatus{{Unit: workspace.TmuxUnit}, {Unit: workspace.UpUnit}})
	if r.ID != IDService || r.Status != doctor.Warn ||
		r.Evidence != "optional: cairn tmux service install keeps tmux alive across logouts and restores on login" {
		t.Fatalf("not installed: %+v", r)
	}
	ok := []workspace.UnitStatus{
		{Unit: workspace.TmuxUnit, File: true, Enabled: "enabled", Active: "active"},
		{Unit: workspace.UpUnit, File: true, Enabled: "enabled", Active: "inactive"},
	}
	if r = run(ok); r.Status != doctor.OK {
		t.Fatalf("installed: %+v", r)
	}
	ok[1].Enabled = "disabled"
	if r = run(ok); r.Status != doctor.Warn || !strings.Contains(r.Evidence, workspace.UpUnit) {
		t.Fatalf("disabled: %+v", r)
	}
}

func TestDriftAttention(t *testing.T) {
	api := proj("api", "/srv/api")
	// A live unmanaged session with the same name: the plan skips api.
	snap := tmux.Snapshot{Sessions: []tmux.Session{{ID: "$1", Name: "api"}}}
	r := byID(t, Checks([]workspace.Project{api}, nil, snap, "3.4", exists), IDDrift)
	if r.Status != doctor.Warn || !strings.Contains(r.Evidence, "1 project needs attention: cairn plan") {
		t.Fatalf("name clash: %+v", r)
	}
	if strings.Contains(r.Evidence, "cairn apply") {
		t.Fatalf("clash is not fixed by apply: %+v", r)
	}
}

func TestFailed(t *testing.T) {
	r := Failed(IDConfig, "Workspace config", doctor.Crit, errors.New("boom")).Run(t.Context())
	if r.ID != IDConfig || r.Status != doctor.Crit || !strings.Contains(r.Evidence, "boom") {
		t.Fatalf("%+v", r)
	}
}
