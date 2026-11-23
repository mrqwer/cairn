package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/workspace"
)

// svcEnv isolates HOME/XDG under a temp dir, fakes systemctl and the cairn
// binary path. Nothing touches the real systemd or tmux.
func svcEnv(t *testing.T) (string, *execx.FakeRunner) {
	t.Helper()
	dir := hostTestEnv(t)
	r := &execx.FakeRunner{Responses: map[string]execx.Response{
		"systemctl --user daemon-reload":                 {},
		"systemctl --user is-enabled cairn-tmux.service": {Out: []byte("disabled\n"), Err: errors.New("exit status 1")},
		"systemctl --user is-enabled cairn-up.service":   {Out: []byte("disabled\n"), Err: errors.New("exit status 1")},
		"systemctl --user enable cairn-tmux.service":     {},
		"systemctl --user enable cairn-up.service":       {},
		"systemctl --user disable cairn-tmux.service":    {},
		"systemctl --user disable cairn-up.service":      {},
	}}
	oldRunner, oldExe := hostRunner, exePath
	t.Cleanup(func() { hostRunner, exePath = oldRunner, oldExe })
	hostRunner = r
	bin := filepath.Join(dir, "bin", "cairn")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	exePath = func() (string, error) { return bin, nil }
	return dir, r
}

func TestTmuxServiceInstallUninstall(t *testing.T) {
	dir, r := svcEnv(t)
	unitDir := filepath.Join(dir, "config/systemd/user")
	inv := filepath.Join(dir, "state/cairn/workspace/managed.json")

	out, err := runHost(t, "y\n", "tmux", "service", "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, f := range []string{"cairn-tmux.service", "cairn-up.service"} {
		b, err := os.ReadFile(filepath.Join(unitDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "ExecStart=") {
			t.Errorf("%s: %s", f, b)
		}
	}
	if _, err := os.Stat(inv); err != nil {
		t.Errorf("inventory not written: %v", err)
	}
	for _, c := range r.Calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "--now") || strings.Contains(j, "start") || strings.Contains(j, "stop") || c[0] == "tmux" {
			t.Errorf("forbidden command %q", j)
		}
	}

	out, err = runHost(t, "", "tmux", "service", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cairn-tmux.service") {
		t.Errorf("status output %q", out)
	}

	if out, err = runHost(t, "y\n", "tmux", "service", "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, f := range []string{"cairn-tmux.service", "cairn-up.service"} {
		if _, err := os.Stat(filepath.Join(unitDir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still present (%v)", f, err)
		}
	}
}

func TestTmuxServiceInstallDeclined(t *testing.T) {
	dir, r := svcEnv(t)
	out, err := runHost(t, "n\n", "tmux", "service", "install")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "config/systemd/user/cairn-tmux.service")); err == nil {
		t.Error("file written despite decline")
	}
	if len(r.Calls) != 0 {
		t.Errorf("commands run: %v", r.Calls)
	}
}

func TestTmuxServiceInstallYesWithSocket(t *testing.T) {
	dir, _ := svcEnv(t)
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", "work"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config/systemd/user/cairn-up.service"))
	if err != nil || !strings.Contains(string(b), "up --socket work") {
		t.Errorf("%v %s", err, b)
	}
}

func TestTmuxServiceRejectsUnsafeSelf(t *testing.T) {
	dir, _ := svcEnv(t)
	bad := filepath.Join(dir, "my cairn")
	if err := os.WriteFile(bad, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	exePath = func() (string, error) { return bad, nil }
	if _, err := runHost(t, "", "tmux", "service", "install", "--yes"); err == nil {
		t.Error("unsafe self accepted")
	}
}

func TestTmuxServiceRoundTripPlan(t *testing.T) {
	dir, _ := svcEnv(t)
	snap := func() []string {
		s, err := loadSvc()
		if err != nil {
			t.Fatal(err)
		}
		self, err := selfPath()
		if err != nil {
			t.Fatal(err)
		}
		p := plan.Resolve(workspace.ServiceActions(self, "", s.unitDir), wsRoot, s.inv)
		var out []string
		for _, a := range p.Actions {
			out = append(out, string(a.Kind)+" "+a.Path+a.Unit+" "+string(a.State))
		}
		return out
	}
	before := snap()
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if mid := snap(); strings.Join(mid, "|") == strings.Join(before, "|") {
		t.Error("plan unchanged after install")
	}
	if out, err := runHost(t, "", "tmux", "service", "uninstall", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := snap(); strings.Join(after, "|") != strings.Join(before, "|") {
		t.Errorf("plan after uninstall %v != before %v", after, before)
	}
	// Only the inventory file and now-empty dirs may remain.
	var left []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && !strings.HasSuffix(p, "/bin/cairn") {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 1 || !strings.HasSuffix(left[0], "workspace/managed.json") {
		t.Errorf("leftover files: %v", left)
	}
}

func TestTmuxServiceStatusOutput(t *testing.T) {
	dir, r := svcEnv(t)
	r.Responses["systemctl --user is-enabled cairn-tmux.service"] = execx.Response{Out: []byte("enabled\n")}
	r.Responses["systemctl --user is-active cairn-tmux.service"] = execx.Response{Out: []byte("active\n")}
	r.Responses["systemctl --user is-enabled cairn-up.service"] = execx.Response{Out: []byte("not-found\n"), Err: errors.New("exit status 1")}
	r.Responses["systemctl --user is-active cairn-up.service"] = execx.Response{Out: []byte("failed\n"), Err: errors.New("exit status 3")}
	unitDir := filepath.Join(dir, "config/systemd/user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "cairn-tmux.service"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runHost(t, "", "tmux", "service", "status")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 ||
		!strings.Contains(lines[0], "cairn-tmux.service") || !strings.Contains(lines[0], "present") ||
		!strings.Contains(lines[0], "enabled: enabled") || !strings.Contains(lines[0], "active: active") ||
		!strings.Contains(lines[1], "missing") || !strings.Contains(lines[1], "enabled: not-found") || !strings.Contains(lines[1], "active: failed") {
		t.Errorf("status output:\n%s", out)
	}
}

func TestTmuxServiceUnsafeSocket(t *testing.T) {
	_, r := svcEnv(t)
	for _, sock := range []string{"a b", "a%b", "..", "a/b"} {
		if _, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", sock); err == nil {
			t.Errorf("socket %q accepted", sock)
		}
	}
	if len(r.Calls) != 0 {
		t.Errorf("commands run: %v", r.Calls)
	}
}

func TestTmuxServiceUninstallDeclined(t *testing.T) {
	dir, _ := svcEnv(t)
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := runHost(t, "n\n", "tmux", "service", "uninstall")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"cairn-tmux.service", "cairn-up.service"} {
		if _, err := os.Stat(filepath.Join(dir, "config/systemd/user", f)); err != nil {
			t.Errorf("%s removed despite decline: %v", f, err)
		}
	}
}

func TestTmuxServiceSocketChange(t *testing.T) {
	dir, _ := svcEnv(t)
	up := filepath.Join(dir, "config/systemd/user/cairn-up.service")
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", "a"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Same socket again: fine with --yes.
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", "a"); err != nil {
		t.Fatalf("same socket: %v\n%s", err, out)
	}
	// Different socket: --yes alone is refused, files unchanged.
	out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--socket", "b")
	if err == nil || !strings.Contains(out, "replaces units for socket a") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if b, _ := os.ReadFile(up); !strings.Contains(string(b), "--socket a") {
		t.Errorf("unit changed despite refusal: %s", b)
	}
	// Interactive confirmation is accepted.
	if out, err := runHost(t, "y\n", "tmux", "service", "install", "--socket", "b"); err != nil {
		t.Fatalf("interactive: %v\n%s", err, out)
	}
	// And --force with --yes.
	if out, err := runHost(t, "", "tmux", "service", "install", "--yes", "--force", "--socket", "a"); err != nil {
		t.Fatalf("force: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(up); !strings.Contains(string(b), "--socket a") {
		t.Errorf("force did not replace: %s", b)
	}
}
