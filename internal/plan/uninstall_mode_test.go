package plan_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const pacmanHook = "/etc/pacman.d/hooks/50-cairn-reboot-required.hook"

func TestPacmanHookAllowed(t *testing.T) {
	a := plan.Action{Kind: plan.WriteFile, Path: pacmanHook, NeedsRoot: true}
	if err := plan.CheckRootAction(a, nil, plan.ModeApply); err != nil {
		t.Fatal(err)
	}
	if plan.AllowedRootPath("/etc/pacman.d/hooks/other.hook", nil, plan.ModeApply) {
		t.Fatal("other pacman hooks allowed")
	}
}

func TestCheckRootActionUninstallMode(t *testing.T) {
	extra := []string{nvidiaTmp}
	ok := []plan.Action{
		{Kind: plan.RemoveFile, Path: sleepConf, NeedsRoot: true},
		{Kind: plan.RemoveFile, Path: pacmanHook, NeedsRoot: true},
		{Kind: plan.RemoveFile, Path: nvidiaTmp, NeedsRoot: true},
		{Kind: plan.DisableUnit, Unit: "nvidia-resume.service", NeedsRoot: true},
		{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true},
	}
	for _, a := range ok {
		if err := plan.CheckRootAction(a, extra, plan.ModeUninstall); err != nil {
			t.Errorf("%s %s: %v", a.Kind, a.Path, err)
		}
	}
	bad := []plan.Action{
		{Kind: plan.WriteFile, Path: sleepConf, NeedsRoot: true},
		{Kind: plan.MakeDir, Path: nvidiaTmp, NeedsRoot: true},
		{Kind: plan.RemoveFile, Path: "/boot/loader/entries/arch.conf", NeedsRoot: true},
		{Kind: plan.RemoveFile, Path: conflicter, NeedsRoot: true},
		{Kind: plan.RemoveFile, Path: "/etc/passwd", NeedsRoot: true},
		{Kind: plan.EnableUnit, Unit: "nvidia-resume.service", NeedsRoot: true},
		{Kind: plan.DisableUnit, Unit: "sshd.service", NeedsRoot: true},
		{Kind: plan.RunCommand, Cmd: []string{"systemctl", "enable", "cairn-x.service"}, NeedsRoot: true},
	}
	for _, a := range bad {
		if err := plan.CheckRootAction(a, extra, plan.ModeUninstall); err == nil {
			t.Errorf("%s %s %s accepted in uninstall mode", a.Kind, a.Path, a.Unit)
		}
	}
}

func TestApplyRootUninstallOnlyRemovesInventoryPaths(t *testing.T) {
	env := rootEnv(t)
	host := validHost()
	put(t, env.Root, sleepConf, "managed\n")
	put(t, env.Root, pacmanHook, "not ours\n")
	withBuilder(t, returns(
		plan.Action{Kind: plan.RemoveFile, Path: sleepConf, NeedsRoot: true},
		plan.Action{Kind: plan.RemoveFile, Path: pacmanHook, NeedsRoot: true},
	))
	env.SysInv.Upsert(inventory.Entry{Path: sleepConf, SHA256: fsx.SHA256([]byte("managed\n"))})
	err := applyRoot(env, plan.ModeUninstall, host, false)
	if err == nil || !strings.Contains(err.Error(), pacmanHook) {
		t.Fatalf("err=%v, want refusal of a path not in the inventory", err)
	}
	if !env.Root.Exists(sleepConf) || !env.Root.Exists(pacmanHook) {
		t.Fatal("rejected uninstall removed files")
	}

	withBuilder(t, returns(plan.Action{Kind: plan.RemoveFile, Path: sleepConf, NeedsRoot: true}))
	if err := applyRoot(env, plan.ModeUninstall, host, false); err != nil {
		t.Fatal(err)
	}
	if env.Root.Exists(sleepConf) {
		t.Fatal("managed file not removed")
	}
	if env.Root.Exists(plan.HostConfigPath) {
		t.Fatal("uninstall wrote the host config snapshot")
	}
}

func TestApplyRootStaleSnapshotIsPending(t *testing.T) {
	host := validHost()
	withBuilder(t, returns())
	env := rootEnv(t)
	var out bytes.Buffer
	err := plan.ApplyRoot(context.Background(), plan.NewRootRequest(plan.ModeApply, host), env,
		plan.RootOptions{Self: selfBin, Out: &out, TTY: strings.NewReader("n\n")})
	if !errors.Is(err, plan.ErrRootDeclined) {
		t.Fatalf("stale snapshot must be confirmed: err=%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "update  config snapshot  "+plan.HostConfigPath) {
		t.Fatalf("snapshot line missing:\n%s", out.String())
	}
	if env.Root.Exists(plan.HostConfigPath) {
		t.Fatal("declined snapshot written")
	}
	if err := plan.ApplyRoot(context.Background(), plan.NewRootRequest(plan.ModeApply, host), env,
		plan.RootOptions{Self: selfBin, Out: io.Discard, TTY: strings.NewReader("y\n")}); err != nil {
		t.Fatal(err)
	}
	// Fresh snapshot and nothing pending: no question.
	out.Reset()
	if err := plan.ApplyRoot(context.Background(), plan.NewRootRequest(plan.ModeApply, host), env,
		plan.RootOptions{Self: selfBin, Out: &out, TTY: noTTY{t}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("out:\n%s", out.String())
	}
}

func TestApplyRootRepairPrintsRebootAndNotes(t *testing.T) {
	env := rootEnv(t)
	host := validHost()
	put(t, env.Root, "/boot/loader/entries/arch.conf", "options resume_offset=1\n")
	withBuilder(t, returns(plan.Action{Kind: plan.WriteFile, Path: "/boot/loader/entries/arch.conf",
		Content: []byte("options resume_offset=2\n"), NeedsRoot: true}))
	oldNotes := plan.RootNotes
	t.Cleanup(func() { plan.RootNotes = oldNotes })
	plan.RootNotes = func(mode string, _ config.Host, _ detect.System) []string {
		if mode != plan.ModeRepair {
			return nil
		}
		return []string{"loader.conf has no default entry"}
	}
	var out bytes.Buffer
	if err := plan.ApplyRoot(context.Background(), plan.NewRootRequest(plan.ModeRepair, host), env,
		plan.RootOptions{Yes: true, Self: selfBin, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "note: loader.conf has no default entry") || !strings.Contains(out.String(), "requires reboot") {
		t.Fatalf("out:\n%s", out.String())
	}
}

func TestRemoveManagedDirOnlyWhenEmpty(t *testing.T) {
	env, _ := newEnv(t)
	dir := "/var/lib/cairn/host/sub"
	put(t, env.Root, dir+"/keep", "x")
	env.SysInv.Upsert(inventory.Entry{Path: dir, Class: inventory.ManagedDir})
	p := plan.Resolve([]plan.Action{{Kind: plan.RemoveFile, Path: dir, NeedsRoot: true}}, env.Root, env.SysInv)
	if err := plan.Apply(context.Background(), p, env, plan.Options{RootOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !env.Root.Exists(dir+"/keep") || len(env.SysInv.Entries) != 1 {
		t.Fatalf("non-empty managed dir must stay recorded: %+v", env.SysInv.Entries)
	}
	if err := env.Root.Remove(dir + "/keep"); err != nil {
		t.Fatal(err)
	}
	p = plan.Resolve([]plan.Action{{Kind: plan.RemoveFile, Path: dir, NeedsRoot: true}}, env.Root, env.SysInv)
	if err := plan.Apply(context.Background(), p, env, plan.Options{RootOnly: true}); err != nil {
		t.Fatal(err)
	}
	if env.Root.Exists(dir) || len(env.SysInv.Entries) != 0 {
		t.Fatalf("empty managed dir not removed: %+v", env.SysInv.Entries)
	}
}
