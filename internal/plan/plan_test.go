package plan_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const sleepConf = "/etc/systemd/sleep.conf.d/50-cairn.conf"

func newRoot(t *testing.T) fsx.Root {
	t.Helper()
	return fsx.Root{Dir: t.TempDir()}
}

func put(t *testing.T, root fsx.Root, p, content string) {
	t.Helper()
	if err := root.WriteAtomic(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func state(t *testing.T, a plan.Action, root fsx.Root, inv *inventory.Inventory) plan.State {
	t.Helper()
	p := plan.Resolve([]plan.Action{a}, root, inv)
	if len(p.Actions) != 1 {
		t.Fatalf("got %d actions", len(p.Actions))
	}
	return p.Actions[0].State
}

func TestResolveStates(t *testing.T) {
	root := newRoot(t)
	want := "[Sleep]\nAllowSuspendThenHibernate=yes\n"
	a := plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte(want), NeedsRoot: true}
	inv := &inventory.Inventory{}

	if s := state(t, a, root, inv); s != plan.Create {
		t.Fatalf("missing file: state %q, want create", s)
	}

	put(t, root, sleepConf, want)
	if s := state(t, a, root, inv); s != plan.Foreign {
		t.Fatalf("present, not in inventory: state %q, want foreign", s)
	}

	inv.Upsert(inventory.Entry{Path: sleepConf, SHA256: fsx.SHA256([]byte(want))})
	if s := state(t, a, root, inv); s != plan.Unchanged {
		t.Fatalf("same content, in inventory: state %q, want unchanged", s)
	}

	b := a
	b.Content = []byte("[Sleep]\nHibernateDelaySec=2h\n")
	if s := state(t, b, root, inv); s != plan.Update {
		t.Fatalf("desired content changed: state %q, want update", s)
	}

	put(t, root, sleepConf, "edited by hand\n")
	if s := state(t, a, root, inv); s != plan.ModifiedByUser {
		t.Fatalf("sha differs from inventory: state %q, want modified-by-user", s)
	}

	put(t, root, sleepConf, want)
	if err := os.Chmod(root.Path(sleepConf), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := state(t, a, root, inv); s != plan.Update {
		t.Fatalf("mode differs: state %q, want update", s)
	}
}

func TestResolveOtherKinds(t *testing.T) {
	root := newRoot(t)
	inv := &inventory.Inventory{}
	unit := plan.Action{Kind: plan.EnableUnit, Unit: "cairn-host-notify.path", UserUnit: true}
	if s := state(t, unit, root, inv); s != plan.Create {
		t.Fatalf("enable not in inventory: %q", s)
	}
	dis := plan.Action{Kind: plan.DisableUnit, Unit: "cairn-host-notify.path", UserUnit: true}
	if s := state(t, dis, root, inv); s != plan.Unchanged {
		t.Fatalf("disable of unmanaged unit: %q", s)
	}
	rm := plan.Action{Kind: plan.RemoveFile, Path: sleepConf, NeedsRoot: true}
	if s := state(t, rm, root, inv); s != plan.Unchanged {
		t.Fatalf("remove of missing file: %q", s)
	}
	dir := plan.Action{Kind: plan.MakeDir, Path: "/var/lib/cairn/host", Mode: 0o755, NeedsRoot: true}
	if s := state(t, dir, root, inv); s != plan.Create {
		t.Fatalf("missing dir: %q", s)
	}
	// A command is pending only when another action on the same side is.
	reload := plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true}
	userReload := plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "--user", "daemon-reload"}}
	p := plan.Resolve([]plan.Action{dir, reload, userReload}, root, inv)
	if p.Actions[1].State != plan.Update || p.Actions[2].State != plan.Unchanged {
		t.Fatalf("command states: root %q user %q", p.Actions[1].State, p.Actions[2].State)
	}
	if p.Mode != plan.ModeApply {
		t.Fatalf("default mode %q", p.Mode)
	}
}

func TestRenderGroups(t *testing.T) {
	root := newRoot(t)
	p := plan.Resolve([]plan.Action{
		{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true, Reason: "sleep settings"},
		{Kind: plan.SetGSetting, Schema: "org.example", Key: "k", Value: "'v'", Reason: "idle action"},
	}, root, &inventory.Inventory{})
	var buf bytes.Buffer
	plan.Render(&buf, p)
	out := buf.String()
	for _, s := range []string{"user-level", "needs root", sleepConf, "sleep settings", "org.example k", "create", "WriteFile"} {
		if !strings.Contains(out, s) {
			t.Errorf("render missing %q:\n%s", s, out)
		}
	}
	if strings.Index(out, "user-level") > strings.Index(out, "needs root") {
		t.Errorf("user-level group should come first:\n%s", out)
	}
	buf.Reset()
	plan.Render(&buf, plan.Plan{})
	if strings.TrimSpace(buf.String()) != "nothing to do" {
		t.Fatalf("empty plan render %q", buf.String())
	}
}

func TestAllowedRootPath(t *testing.T) {
	extra := []string{"/home/.nvidia-suspend"}
	cases := []struct {
		path         string
		apply, repai bool
	}{
		{"/etc/systemd/sleep.conf.d/50-cairn.conf", true, true},
		{"/etc/systemd/logind.conf.d/50-cairn.conf", true, true},
		{"/etc/systemd/system/cairn-sleep-fallback.service", true, true},
		{"/etc/systemd/system/systemd-hibernate.service.d/50-cairn.conf", true, true},
		{"/usr/lib/systemd/system-sleep/50-cairn", true, true},
		{"/etc/UPower/UPower.conf.d/50-cairn.conf", true, true},
		{"/etc/modprobe.d/nvidia-utils.conf", true, true},
		{"/var/lib/cairn/host", true, true},
		{"/var/lib/cairn/host/backup/%etc%x", true, true},
		{"/home/.nvidia-suspend", true, true},
		{"/etc/passwd", false, false},
		{"/etc/systemd/sleep.conf.d/../../passwd", false, false},
		{"/var/lib/cairn/../../../etc/shadow", false, false},
		{"etc/systemd/sleep.conf.d/50-cairn.conf", false, false},
		{"/etc/systemd/sleep.conf.d", false, false},
		{"/etc/systemd/system/sshd.service", false, true},
		{"/etc/systemd/system/systemd-hibernate.service.d/other.conf", false, true},
		{"/etc/systemd/system/systemd-suspend-then-hibernate.service.d/fallback.conf", false, true},
		{"/etc/systemd/system/sshd.service.d/override.conf", false, false},
		{"/etc/systemd/system/multi-user.target.wants/sshd.service", false, false},
		{"/etc/systemd/system/sshd.service.d/50-cairn.conf", false, false},
		{"/etc/systemd/system/systemd-suspend-then-hibernate.service.d/50-cairn.conf", true, true},
		{"/etc/systemd/system/cairn-notify.path", false, false},
		{"/etc/systemd/system/cairn-sleep-fallback.service.d/50-cairn.conf", false, false},
		{"/boot/loader/entries/arch.conf", false, true},
		{"/boot/loader/loader.conf", false, false},
		{"/boot/loader/entries/sub/arch.conf", false, false},
		{"/home/.nvidia-suspend/x", false, false},
		{"/home/other", false, false},
		{"/var/lib/cairnx/host", false, false},
		{"/usr/lib/systemd/system-sleep/other", false, false},
	}
	for _, c := range cases {
		if got := plan.AllowedRootPath(c.path, extra, plan.ModeApply); got != c.apply {
			t.Errorf("apply mode %s: got %v, want %v", c.path, got, c.apply)
		}
		if got := plan.AllowedRootPath(c.path, extra, plan.ModeRepair); got != c.repai {
			t.Errorf("repair mode %s: got %v, want %v", c.path, got, c.repai)
		}
	}
	if plan.AllowedRootPath("/home/.nvidia-suspend", nil, plan.ModeApply) {
		t.Error("extra path allowed without extra list")
	}
	if plan.AllowedRootPath(sleepConf, nil, "bogus") {
		t.Error("unknown mode must reject everything")
	}
}

func TestCheckRootActionKinds(t *testing.T) {
	extra := []string{"/home/.nvidia-suspend"}
	cases := []struct {
		name string
		a    plan.Action
		mode string
		ok   bool
	}{
		{"makedir extra", plan.Action{Kind: plan.MakeDir, Path: "/home/.nvidia-suspend"}, plan.ModeApply, true},
		{"write extra", plan.Action{Kind: plan.WriteFile, Path: "/home/.nvidia-suspend"}, plan.ModeApply, false},
		{"repair remove unit", plan.Action{Kind: plan.RemoveFile, Path: "/etc/systemd/system/sleep-fallback.service"}, plan.ModeRepair, true},
		{"repair write unit", plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/system/sleep-fallback.service"}, plan.ModeRepair, false},
		{"apply remove unit", plan.Action{Kind: plan.RemoveFile, Path: "/etc/systemd/system/sleep-fallback.service"}, plan.ModeApply, false},
		{"repair remove dropin", plan.Action{Kind: plan.RemoveFile, Path: "/etc/systemd/system/systemd-hibernate.service.d/fallback.conf"}, plan.ModeRepair, true},
		{"repair write foreign dropin", plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/system/systemd-hibernate.service.d/fallback.conf"}, plan.ModeRepair, false},
		{"repair write boot entry", plan.Action{Kind: plan.WriteFile, Path: "/boot/loader/entries/arch.conf"}, plan.ModeRepair, true},
		{"repair remove boot entry", plan.Action{Kind: plan.RemoveFile, Path: "/boot/loader/entries/arch.conf"}, plan.ModeRepair, false},
		{"apply write boot entry", plan.Action{Kind: plan.WriteFile, Path: "/boot/loader/entries/arch.conf"}, plan.ModeApply, false},
		{"gsetting as root", plan.Action{Kind: plan.SetGSetting, Schema: "org.example", Key: "k", Value: "1"}, plan.ModeApply, false},
		{"root enable user unit", plan.Action{Kind: plan.EnableUnit, Unit: "cairn-x.service", UserUnit: true}, plan.ModeApply, false},
		{"root enable cairn unit", plan.Action{Kind: plan.EnableUnit, Unit: "cairn-sleep-fallback.service"}, plan.ModeApply, true},
		{"root enable nvidia unit", plan.Action{Kind: plan.EnableUnit, Unit: "nvidia-resume.service"}, plan.ModeApply, true},
		{"root enable sshd", plan.Action{Kind: plan.EnableUnit, Unit: "sshd.service"}, plan.ModeApply, false},
		{"root disable paccache", plan.Action{Kind: plan.DisableUnit, Unit: "paccache.timer"}, plan.ModeApply, true},
		{"unknown kind", plan.Action{Kind: "Chown", Path: sleepConf}, plan.ModeApply, false},
		{"world-writable file", plan.Action{Kind: plan.WriteFile, Path: sleepConf, Mode: 0o666}, plan.ModeApply, false},
		{"group-writable dir", plan.Action{Kind: plan.MakeDir, Path: "/var/lib/cairn/host", Mode: 0o775}, plan.ModeApply, false},
		{"setuid file", plan.Action{Kind: plan.WriteFile, Path: "/usr/lib/systemd/system-sleep/50-cairn", Mode: 0o755 | os.ModeSetuid}, plan.ModeApply, false},
		{"sticky dir", plan.Action{Kind: plan.MakeDir, Path: "/var/lib/cairn/host", Mode: 0o755 | os.ModeSticky}, plan.ModeApply, false},
		{"executable hook", plan.Action{Kind: plan.WriteFile, Path: "/usr/lib/systemd/system-sleep/50-cairn", Mode: 0o755}, plan.ModeApply, true},
		{"foreign service dropin", plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/system/sshd.service.d/50-cairn.conf"}, plan.ModeApply, false},
	}
	for _, c := range cases {
		err := plan.CheckRootAction(c.a, extra, c.mode)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestCommandAllowlist(t *testing.T) {
	root := []struct {
		cmd []string
		ok  bool
	}{
		{[]string{"systemctl", "daemon-reload"}, true},
		{[]string{"systemctl", "enable", "cairn-sleep-fallback.service"}, true},
		{[]string{"systemctl", "disable", "nvidia-suspend.service"}, true},
		{[]string{"systemctl", "enable", "nvidia-suspend-then-hibernate.service"}, true},
		{[]string{"systemctl", "enable", "paccache.timer"}, true},
		{[]string{"systemctl", "enable", "sshd.service"}, false},
		{[]string{"systemctl", "enable", "--now", "cairn-x.service"}, false},
		{[]string{"systemctl", "enable", "cairn-x.service", "sshd.service"}, false},
		{[]string{"systemctl", "enable", "cairn-../x.service"}, false},
		{[]string{"systemctl", "mask", "cairn-x.service"}, false},
		{[]string{"systemctl", "--user", "daemon-reload"}, false},
		{[]string{"gsettings", "set", "org.example", "k", "1"}, false},
		{[]string{"rm", "-rf", "/"}, false},
		{[]string{"/usr/bin/systemctl", "daemon-reload"}, false},
		{nil, false},
	}
	for _, c := range root {
		err := plan.CheckRootAction(plan.Action{Kind: plan.RunCommand, Cmd: c.cmd, NeedsRoot: true}, nil, plan.ModeApply)
		if (err == nil) != c.ok {
			t.Errorf("root %q: err=%v, want ok=%v", c.cmd, err, c.ok)
		}
	}
	user := []struct {
		cmd []string
		ok  bool
	}{
		{[]string{"systemctl", "--user", "daemon-reload"}, true},
		{[]string{"systemctl", "--user", "enable", "cairn-host-notify.path"}, true},
		{[]string{"systemctl", "--user", "disable", "cairn-host-boot-check.service"}, true},
		{[]string{"systemctl", "--user", "enable", "pipewire.service"}, false},
		{[]string{"systemctl", "daemon-reload"}, false},
		{[]string{"gsettings", "get", "org.gnome.settings-daemon.plugins.power", "sleep-inactive-battery-type"}, true},
		{[]string{"gsettings", "set", "org.gnome.settings-daemon.plugins.power", "sleep-inactive-battery-type", "'hibernate'"}, true},
		{[]string{"gsettings", "reset", "org.example", "k"}, false},
		{[]string{"gsettings", "set", "--schemadir", "x", "k"}, false},
		{[]string{"sh", "-c", "true"}, false},
	}
	for _, c := range user {
		err := plan.CheckUserAction(plan.Action{Kind: plan.RunCommand, Cmd: c.cmd})
		if (err == nil) != c.ok {
			t.Errorf("user %q: err=%v, want ok=%v", c.cmd, err, c.ok)
		}
	}
	if err := plan.CheckUserAction(plan.Action{Kind: plan.EnableUnit, Unit: "cairn-x.service"}); err == nil {
		t.Error("user-side system unit enable must be rejected")
	}
	if err := plan.CheckUserAction(plan.Action{Kind: plan.EnableUnit, Unit: "cairn-x.service", UserUnit: true}); err != nil {
		t.Errorf("user unit enable: %v", err)
	}
}

func TestCheckSelfOwnership(t *testing.T) {
	root := newRoot(t)
	put(t, root, "/bin/cairn", "binary")
	if err := os.Chmod(root.Path("/bin/cairn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		err := plan.CheckSelfOwnership(root, "/bin/cairn")
		if err == nil || !strings.Contains(err.Error(), "sudo install") || !strings.Contains(err.Error(), "just install") {
			t.Fatalf("user-owned binary: err=%v", err)
		}
		return
	}
	for _, d := range []string{root.Dir, filepath.Dir(root.Path("/bin/cairn"))} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := plan.CheckSelfOwnership(root, "/bin/cairn"); err != nil {
		t.Fatalf("root-owned binary: %v", err)
	}
	if err := os.Chmod(root.Path("/bin/cairn"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := plan.CheckSelfOwnership(root, "/bin/cairn"); err == nil {
		t.Fatal("group-writable binary accepted")
	}
}
