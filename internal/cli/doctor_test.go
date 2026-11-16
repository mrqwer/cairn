package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/tmux"
)

type stubCheck struct{ r doctor.Result }

func (s stubCheck) ID() string                        { return s.r.ID }
func (s stubCheck) Title() string                     { return s.r.Title }
func (s stubCheck) Run(context.Context) doctor.Result { return s.r }

func runDoctor(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestDoctorNoChecks(t *testing.T) {
	old := hostChecks
	t.Cleanup(func() { hostChecks = old })
	hostChecks = func(context.Context) ([]doctor.Check, error) { return nil, nil }
	out, err := runDoctor(t, "doctor", "host")
	if err != nil || strings.TrimSpace(out) != "no checks registered" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestDoctorCritExitAndJSON(t *testing.T) {
	old := hostChecks
	t.Cleanup(func() { hostChecks = old })
	hostChecks = func(context.Context) ([]doctor.Check, error) {
		return []doctor.Check{stubCheck{doctor.Result{ID: "x", Title: "X", Status: doctor.Crit, Evidence: "bad", Fix: "fix it"}}}, nil
	}
	oldWS := workspaceChecks
	t.Cleanup(func() { workspaceChecks = oldWS })
	workspaceChecks = func(context.Context, string) ([]doctor.Check, error) { return nil, nil }
	out, err := runDoctor(t, "doctor", "--json")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(out, `"status": "CRIT"`) || !strings.Contains(out, `"fix": "fix it"`) {
		t.Fatalf("out=%s", out)
	}
	out, _ = runDoctor(t, "doctor")
	if !strings.Contains(out, "CRIT  X  bad\n      fix: fix it\n") {
		t.Fatalf("out=%q", out)
	}
}

// roRunner answers tmux and systemctl queries and records every call.
type roRunner struct {
	calls   [][]string
	version string // tmux -V answer; "" means tmux is missing
}

func (r *roRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch {
	case name == "systemctl":
		return []byte("disabled\n"), errors.New("exit status 1")
	case name != "tmux":
		return nil, errors.New("unexpected command " + name)
	case r.version == "":
		return nil, errors.New("executable file not found")
	case len(args) == 1 && args[0] == "-V":
		return []byte("tmux " + r.version + "\n"), nil
	}
	return []byte("no server running on /tmp/tmux-0/x\n"), errors.New("exit status 1")
}

func stubWorkspaceEnv(t *testing.T, r *roRunner) {
	t.Helper()
	oldClient, oldRunner := newTmuxClient, hostRunner
	t.Cleanup(func() { newTmuxClient, hostRunner = oldClient, oldRunner })
	newTmuxClient = func(socket string) tmux.Client { return tmux.Client{R: r, Socket: socket} }
	hostRunner = r
}

func TestDoctorWorkspaceIsReadOnly(t *testing.T) {
	dir, _ := wsTestEnv(t)
	addProject(t, "demo", dir)
	r := &roRunner{version: "3.4"}
	stubWorkspaceEnv(t, r)

	out, err := runDoctor(t, "doctor", "workspace", "--socket", "cairn-test-doc")
	var ee *ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("err=%v\n%s", err, out)
	}
	for _, title := range []string{"tmux available", "Workspace config", "Project roots", "Workspace in sync", "tmux user service"} {
		if !strings.Contains(out, title) {
			t.Errorf("missing %q in\n%s", title, out)
		}
	}
	if !strings.Contains(out, "1 project needs `cairn apply`") {
		t.Fatalf("empty server must read as drift, not error:\n%s", out)
	}
	if len(r.calls) == 0 {
		t.Fatal("no commands ran")
	}
	allowed := map[string]bool{"-V": true, "list-sessions": true, "list-windows": true, "list-panes": true, "display-message": true}
	for _, c := range r.calls {
		switch c[0] {
		case "tmux":
			args := c[1:]
			if len(args) >= 2 && args[0] == "-L" {
				if args[1] != "cairn-test-doc" {
					t.Errorf("wrong socket in %v", c)
				}
				args = args[2:]
			}
			if len(args) == 0 || !allowed[args[0]] {
				t.Errorf("mutating or unknown tmux command: %v", c)
			}
		case "systemctl":
			if len(c) < 4 || c[1] != "--user" || (c[2] != "is-enabled" && c[2] != "is-active") {
				t.Errorf("non-query systemctl: %v", c)
			}
		default:
			t.Errorf("unexpected command: %v", c)
		}
	}
}

func TestDoctorWorkspaceMissingTmuxIsCrit(t *testing.T) {
	wsTestEnv(t)
	stubWorkspaceEnv(t, &roRunner{})
	out, err := runDoctor(t, "doctor", "workspace")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 || !strings.Contains(out, "CRIT  tmux available") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestDoctorWithoutArgRunsHostAndWorkspace(t *testing.T) {
	wsTestEnv(t)
	oldHost := hostChecks
	t.Cleanup(func() { hostChecks = oldHost })
	hostChecks = func(context.Context) ([]doctor.Check, error) {
		return []doctor.Check{stubCheck{doctor.Result{ID: "h", Title: "HostCheck"}}}, nil
	}
	stubWorkspaceEnv(t, &roRunner{version: "3.4"})
	out, _ := runDoctor(t, "doctor", "--json")
	if !strings.Contains(out, `"id": "h"`) || !strings.Contains(out, `"id": "workspace.tmux"`) {
		t.Fatalf("%s", out)
	}
	out, _ = runDoctor(t, "doctor", "host")
	if strings.Contains(out, "workspace") {
		t.Fatalf("doctor host must not run workspace checks:\n%s", out)
	}
}

func TestDoctorWorkspaceErrorsKeepHostResults(t *testing.T) {
	wsTestEnv(t)
	oldHost := hostChecks
	t.Cleanup(func() { hostChecks = oldHost })
	hostChecks = func(context.Context) ([]doctor.Check, error) {
		return []doctor.Check{stubCheck{doctor.Result{ID: "h", Title: "HostCheck"}}}, nil
	}
	oldWS := workspaceChecks
	t.Cleanup(func() { workspaceChecks = oldWS })
	workspaceChecks = func(context.Context, string) ([]doctor.Check, error) {
		return nil, errors.New("projects dir unreadable")
	}
	out, err := runDoctor(t, "doctor")
	var ee *ExitError
	if !errors.As(err, &ee) || !strings.Contains(out, "HostCheck") || !strings.Contains(out, "projects dir unreadable") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

// failSnap answers tmux -V but fails every list command with a non-"no server" error.
type failSnap struct{ roRunner }

func (f *failSnap) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "tmux" && (len(args) != 1 || args[0] != "-V") {
		return []byte("permission denied\n"), errors.New("exit status 1")
	}
	return f.roRunner.Run(ctx, name, args...)
}

func TestDoctorWorkspaceSnapshotErrorIsAResult(t *testing.T) {
	wsTestEnv(t)
	stubWorkspaceEnv(t, &roRunner{version: "3.4"})
	r := &failSnap{roRunner{version: "3.4"}}
	newTmuxClient = func(socket string) tmux.Client { return tmux.Client{R: r, Socket: socket} }
	out, _ := runDoctor(t, "doctor", "workspace")
	if !strings.Contains(out, "permission denied") || !strings.Contains(out, "Workspace config") {
		t.Fatalf("%s", out)
	}
	if strings.Contains(out, "Workspace in sync") {
		t.Fatalf("drift must not run on a failed snapshot:\n%s", out)
	}
}
