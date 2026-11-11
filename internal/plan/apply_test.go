package plan_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const (
	userInvPath = "/state/cairn/host/managed.json"
	userUnit    = "/config/systemd/user/cairn-host-boot-check.service"
	modprobe    = "/etc/modprobe.d/nvidia-utils.conf"
	gsSchema    = "org.gnome.settings-daemon.plugins.power"
	gsKey       = "sleep-inactive-battery-type"
)

var errExit1 = errors.New("exit status 1")

func newEnv(t *testing.T) (plan.Env, *execx.FakeRunner) {
	t.Helper()
	fr := &execx.FakeRunner{Responses: map[string]execx.Response{
		"systemctl daemon-reload":                                   {},
		"systemctl --user daemon-reload":                            {},
		"systemctl --user is-enabled cairn-host-boot-check.service": {Out: []byte("disabled\n"), Err: errExit1},
		"systemctl --user enable cairn-host-boot-check.service":     {},
		"gsettings get " + gsSchema + " " + gsKey:                   {Out: []byte("'suspend'\n")},
		"gsettings set " + gsSchema + " " + gsKey + " 'hibernate'":  {},
		"gsettings set " + gsSchema + " " + gsKey + " 'suspend'":    {},
		"systemctl is-enabled nvidia-resume.service":                {Out: []byte("enabled\n")},
		"systemctl enable nvidia-resume.service":                    {},
	}}
	return plan.Env{
		Root:          newRoot(t),
		Runner:        fr,
		UserInv:       &inventory.Inventory{},
		SysInv:        &inventory.Inventory{},
		UserInvPath:   userInvPath,
		SysInvPath:    plan.SysInventoryPath,
		UserBackupDir: "/state/cairn/host/backup",
	}, fr
}

func desired() []plan.Action {
	return []plan.Action{
		{Kind: plan.MakeDir, Path: "/var/lib/cairn/host", Mode: 0o755, NeedsRoot: true, Class: inventory.ManagedDir, Policy: "sleep"},
		{Kind: plan.WriteFile, Path: sleepConf, Content: []byte(fsx.Header("sleep") + "[Sleep]\n"), NeedsRoot: true, Class: inventory.DropIn, Policy: "sleep"},
		{Kind: plan.WriteFile, Path: "/usr/lib/systemd/system-sleep/50-cairn", Content: []byte("#!/bin/sh\n"), Mode: 0o755, NeedsRoot: true, Class: inventory.DropIn, Policy: "sleep"},
		{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true},
		{Kind: plan.WriteFile, Path: userUnit, Content: []byte("[Service]\n"), Class: inventory.DropIn, Policy: "sleep"},
		{Kind: plan.RunCommand, Cmd: []string{"systemctl", "--user", "daemon-reload"}},
		{Kind: plan.EnableUnit, Unit: "cairn-host-boot-check.service", UserUnit: true, Policy: "sleep"},
		{Kind: plan.SetGSetting, Schema: gsSchema, Key: gsKey, Value: "'hibernate'", Policy: "sleep"},
	}
}

func reload(t *testing.T, env *plan.Env) {
	t.Helper()
	var err error
	if env.UserInv, err = inventory.Load(env.Root, env.UserInvPath); err != nil {
		t.Fatal(err)
	}
	if env.SysInv, err = inventory.Load(env.Root, env.SysInvPath); err != nil {
		t.Fatal(err)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	env, fr := newEnv(t)
	ctx := context.Background()
	p := plan.ResolveAll(desired(), env.Root, env.UserInv, env.SysInv)
	if len(p.Pending()) != len(desired()) {
		t.Fatalf("first plan: %d pending, want all %d", len(p.Pending()), len(desired()))
	}
	if err := plan.Apply(ctx, p, env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	st, err := env.Root.Stat("/usr/lib/systemd/system-sleep/50-cairn")
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("hook mode: %v %v", st, err)
	}

	reload(t, &env)
	unit, ok := env.UserInv.Get("user-unit:cairn-host-boot-check.service")
	if !ok || unit.Previous != "disabled" || unit.Class != inventory.UnitEnable {
		t.Fatalf("unit entry: %+v %v", unit, ok)
	}
	gs, ok := env.UserInv.Get("gsettings:" + gsSchema + " " + gsKey)
	if !ok || gs.Previous != "'suspend'" || gs.Class != inventory.UserSetting {
		t.Fatalf("gsetting entry: %+v %v", gs, ok)
	}
	if e, ok := env.SysInv.Get(sleepConf); !ok || e.Policy != "sleep" || e.Class != inventory.DropIn {
		t.Fatalf("sys entry: %+v %v", e, ok)
	}

	calls := len(fr.Calls)
	p2 := plan.ResolveAll(desired(), env.Root, env.UserInv, env.SysInv)
	if pend := p2.Pending(); len(pend) != 0 {
		t.Fatalf("second plan not empty: %+v", pend)
	}
	var buf bytes.Buffer
	plan.Render(&buf, p2)
	if strings.TrimSpace(buf.String()) != "nothing to do" {
		t.Fatalf("render: %q", buf.String())
	}
	if err := plan.Apply(ctx, p2, env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(fr.Calls) != calls {
		t.Fatalf("no-op apply ran commands: %q", fr.Calls[calls:])
	}
}

func TestApplyUserOnlyAndRootOnly(t *testing.T) {
	env, _ := newEnv(t)
	ctx := context.Background()
	p := plan.ResolveAll(desired(), env.Root, env.UserInv, env.SysInv)
	if err := plan.Apply(ctx, p, env, plan.Options{UserOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !env.Root.Exists(userUnit) || env.Root.Exists(sleepConf) {
		t.Fatal("UserOnly must write user files only")
	}
	if env.Root.Exists(plan.SysInventoryPath) {
		t.Fatal("UserOnly must not save the system inventory")
	}
	if err := plan.Apply(ctx, p, env, plan.Options{RootOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !env.Root.Exists(sleepConf) {
		t.Fatal("RootOnly did not write root file")
	}
}

func TestApplyRefusesModifiedByUser(t *testing.T) {
	env, _ := newEnv(t)
	ctx := context.Background()
	if err := plan.Apply(ctx, plan.ResolveAll(desired(), env.Root, env.UserInv, env.SysInv), env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	put(t, env.Root, sleepConf, "hand edit\n")
	p := plan.ResolveAll(desired(), env.Root, env.UserInv, env.SysInv)
	err := plan.Apply(ctx, p, env, plan.Options{})
	if !errors.Is(err, plan.ErrModifiedByUser) || !strings.Contains(err.Error(), sleepConf) {
		t.Fatalf("err=%v, want ErrModifiedByUser naming %s", err, sleepConf)
	}
	if got, _ := env.Root.ReadFile(sleepConf); string(got) != "hand edit\n" {
		t.Fatalf("file overwritten without force: %q", got)
	}

	if err := plan.Apply(ctx, p, env, plan.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	want := desired()[1].Content
	if got, _ := env.Root.ReadFile(sleepConf); !bytes.Equal(got, want) {
		t.Fatalf("force did not overwrite: %q", got)
	}
	if e, _ := env.SysInv.Get(sleepConf); e.SHA256 != fsx.SHA256(want) {
		t.Fatalf("inventory sha not updated: %+v", e)
	}
}

func TestForeignFileBackedUp(t *testing.T) {
	env, _ := newEnv(t)
	ctx := context.Background()
	put(t, env.Root, modprobe, "options nvidia NVreg_TemporaryFilePath=/var/tmp\n")
	mask := plan.Action{Kind: plan.WriteFile, Path: modprobe, Content: []byte("managed\n"), NeedsRoot: true, Class: inventory.Mask, Policy: "gpu-nvidia"}
	p := plan.ResolveAll([]plan.Action{mask}, env.Root, env.UserInv, env.SysInv)
	if p.Actions[0].State != plan.Foreign {
		t.Fatalf("state %q", p.Actions[0].State)
	}
	if err := plan.Apply(ctx, p, env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	backup := "/var/lib/cairn/host/backup/%etc%modprobe.d%nvidia-utils.conf"
	if got, err := env.Root.ReadFile(backup); err != nil || string(got) != "options nvidia NVreg_TemporaryFilePath=/var/tmp\n" {
		t.Fatalf("backup %q %v", got, err)
	}
	e, ok := env.SysInv.Get(modprobe)
	if !ok || e.Backup != backup || e.Class != inventory.Mask {
		t.Fatalf("entry %+v", e)
	}
	if got, _ := env.Root.ReadFile(modprobe); string(got) != "managed\n" {
		t.Fatalf("not overwritten: %q", got)
	}

	// Removing a managed file that has a backup restores the original.
	rm := plan.Action{Kind: plan.RemoveFile, Path: modprobe, NeedsRoot: true}
	p = plan.ResolveAll([]plan.Action{rm}, env.Root, env.UserInv, env.SysInv)
	if err := plan.Apply(ctx, p, env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.Root.ReadFile(modprobe); string(got) != "options nvidia NVreg_TemporaryFilePath=/var/tmp\n" {
		t.Fatalf("original not restored: %q", got)
	}
	if _, ok := env.SysInv.Get(modprobe); ok {
		t.Fatal("entry kept after remove")
	}
}

func TestGSettingRestoreDropsEntry(t *testing.T) {
	env, _ := newEnv(t)
	ctx := context.Background()
	set := []plan.Action{{Kind: plan.SetGSetting, Schema: gsSchema, Key: gsKey, Value: "'hibernate'"}}
	if err := plan.Apply(ctx, plan.ResolveAll(set, env.Root, env.UserInv, env.SysInv), env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	back := []plan.Action{{Kind: plan.SetGSetting, Schema: gsSchema, Key: gsKey, Value: "'suspend'"}}
	if err := plan.Apply(ctx, plan.ResolveAll(back, env.Root, env.UserInv, env.SysInv), env, plan.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(env.UserInv.Entries) != 0 {
		t.Fatalf("restoring the previous value must drop the entry: %+v", env.UserInv.Entries)
	}
}

func TestApplyValidatesBeforeExecuting(t *testing.T) {
	env, fr := newEnv(t)
	bad := []plan.Action{
		{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true},
		{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true},
		{Kind: plan.WriteFile, Path: "/etc/passwd", Content: []byte("x"), NeedsRoot: true},
	}
	err := plan.Apply(context.Background(), plan.ResolveAll(bad, env.Root, env.UserInv, env.SysInv), env, plan.Options{})
	if err == nil || !strings.Contains(err.Error(), "/etc/passwd") {
		t.Fatalf("err=%v", err)
	}
	if env.Root.Exists(sleepConf) || len(fr.Calls) != 0 {
		t.Fatal("actions executed before validation finished")
	}
	userBad := []plan.Action{
		{Kind: plan.WriteFile, Path: userUnit, Content: []byte("x")},
		{Kind: plan.RunCommand, Cmd: []string{"sh", "-c", "true"}},
	}
	if err := plan.Apply(context.Background(), plan.ResolveAll(userBad, env.Root, env.UserInv, env.SysInv), env, plan.Options{}); err == nil {
		t.Fatal("user-side command outside the allowlist accepted")
	}
	if env.Root.Exists(userUnit) {
		t.Fatal("user action executed before validation finished")
	}
}

func rootRequest(mode string, actions ...plan.Action) plan.RootRequest {
	host := config.Host{}
	host.Sleep.Lid = "suspend-then-hibernate"
	host.Sleep.LidAC = "suspend-then-hibernate"
	host.Sleep.CriticalBattery = "hibernate"
	host.Sleep.IdleBattery = "hibernate"
	host.Sleep.HibernateAfter = config.Duration{Duration: 15 * time.Minute}
	host.GPU.Nvidia.TemporaryFilePath = "/home/.nvidia-suspend"
	return plan.NewRootRequest(plan.Plan{Mode: mode, Actions: actions}, []string{"/home/.nvidia-suspend"}, host, false)
}

func TestApplyRootWritesConfigSnapshot(t *testing.T) {
	env, _ := newEnv(t)
	req := rootRequest(plan.ModeApply,
		plan.Action{Kind: plan.MakeDir, Path: "/home/.nvidia-suspend", Mode: 0o700, NeedsRoot: true, Class: inventory.ManagedDir},
		plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true},
		plan.Action{Kind: plan.SetGSetting, Schema: gsSchema, Key: gsKey, Value: "'x'"}, // user-level: dropped
	)
	if len(req.Actions) != 2 {
		t.Fatalf("request must carry root actions only: %+v", req.Actions)
	}
	if err := plan.ApplyRoot(context.Background(), req, env); err != nil {
		t.Fatal(err)
	}
	st, err := env.Root.Stat(plan.HostConfigPath)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("snapshot: %v %v", st, err)
	}
	data, _ := env.Root.ReadFile(plan.HostConfigPath)
	var h config.Host
	if err := json.Unmarshal(data, &h); err != nil || h.Sleep.HibernateAfter.Duration != 15*time.Minute || h.GPU.Nvidia.TemporaryFilePath != "/home/.nvidia-suspend" {
		t.Fatalf("snapshot content %s: %v", data, err)
	}
	if st, err := env.Root.Stat("/home/.nvidia-suspend"); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("temp dir: %v %v", st, err)
	}
	saved, err := inventory.Load(env.Root, plan.SysInventoryPath)
	if err != nil || len(saved.Entries) != 2 {
		t.Fatalf("saved inventory %+v %v", saved, err)
	}
}

func TestApplyRootRepairMode(t *testing.T) {
	const conflicting = "/etc/systemd/system/sleep-fallback.service"
	env, _ := newEnv(t)
	put(t, env.Root, conflicting, "[Service]\nExecStart=/usr/bin/systemctl suspend\n")
	rm := plan.Action{Kind: plan.RemoveFile, Path: conflicting, NeedsRoot: true}

	err := plan.ApplyRoot(context.Background(), rootRequest(plan.ModeApply, rm), env)
	if err == nil || !env.Root.Exists(conflicting) {
		t.Fatalf("apply mode must reject %s: err=%v", conflicting, err)
	}
	if env.Root.Exists(plan.HostConfigPath) {
		t.Fatal("rejected request must not write anything")
	}

	if err := plan.ApplyRoot(context.Background(), rootRequest(plan.ModeRepair, rm), env); err != nil {
		t.Fatal(err)
	}
	if env.Root.Exists(conflicting) {
		t.Fatal("repair did not remove the conflicting unit")
	}
	if got, _ := env.Root.ReadFile("/var/lib/cairn/host/backup/%etc%systemd%system%sleep-fallback.service"); !strings.Contains(string(got), "systemctl suspend") {
		t.Fatalf("backup missing: %q", got)
	}

	boot := plan.Action{Kind: plan.WriteFile, Path: "/boot/loader/entries/arch.conf", Content: []byte("options resume_offset=1\n"), NeedsRoot: true}
	if err := plan.ApplyRoot(context.Background(), rootRequest(plan.ModeApply, boot), env); err == nil {
		t.Fatal("boot entry write accepted in apply mode")
	}
	if err := plan.ApplyRoot(context.Background(), rootRequest(plan.ModeRepair, boot), env); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRootRejectsBadRequests(t *testing.T) {
	ok := plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true}
	cases := map[string]func(r *plan.RootRequest){
		"user action": func(r *plan.RootRequest) {
			r.Actions = append(r.Actions, plan.Action{Kind: plan.SetGSetting, Schema: "a", Key: "b", Value: "c"})
		},
		"bad version":     func(r *plan.RootRequest) { r.Version = 99 },
		"bad mode":        func(r *plan.RootRequest) { r.Mode = "yolo" },
		"extra /etc":      func(r *plan.RootRequest) { r.Extra = []string{"/etc"} },
		"extra root":      func(r *plan.RootRequest) { r.Extra = []string{"/"} },
		"extra relative":  func(r *plan.RootRequest) { r.Extra = []string{"home/x"} },
		"extra dotdot":    func(r *plan.RootRequest) { r.Extra = []string{"/home/../etc/x"} },
		"bad host config": func(r *plan.RootRequest) { r.Host.Sleep.Lid = "explode" },
		"bad command": func(r *plan.RootRequest) {
			r.Actions = append(r.Actions, plan.Action{Kind: plan.RunCommand, Cmd: []string{"rm", "-rf", "/"}, NeedsRoot: true})
		},
		"not root": func(r *plan.RootRequest) { r.Actions[0].NeedsRoot = false },
	}
	for name, mutate := range cases {
		env, fr := newEnv(t)
		req := rootRequest(plan.ModeApply, ok)
		mutate(&req)
		if err := plan.ApplyRoot(context.Background(), req, env); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if env.Root.Exists(sleepConf) || env.Root.Exists(plan.HostConfigPath) || len(fr.Calls) != 0 {
			t.Errorf("%s: executed something before rejecting", name)
		}
	}
}

func TestApplyRootReResolvesState(t *testing.T) {
	env, _ := newEnv(t)
	put(t, env.Root, sleepConf, "hand edit\n")
	env.SysInv.Upsert(inventory.Entry{Path: sleepConf, SHA256: fsx.SHA256([]byte("old managed"))})
	// The requester claims "create"; the root side must not trust it.
	req := rootRequest(plan.ModeApply, plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true, State: plan.Create})
	if err := plan.ApplyRoot(context.Background(), req, env); !errors.Is(err, plan.ErrModifiedByUser) {
		t.Fatalf("err=%v, want ErrModifiedByUser", err)
	}
	req.Force = true
	if err := plan.ApplyRoot(context.Background(), req, env); err != nil {
		t.Fatal(err)
	}
}

func TestRootRequestFile(t *testing.T) {
	root := newRoot(t)
	req := rootRequest(plan.ModeRepair, plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true})
	if err := plan.WriteRootRequest(root, "/run/cairn/host-plan.json", req); err != nil {
		t.Fatal(err)
	}
	st, _ := root.Stat("/run/cairn/host-plan.json")
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("plan file mode %v", st.Mode())
	}
	got, err := plan.ReadRootRequest(root, "/run/cairn/host-plan.json")
	if err != nil || got.Mode != plan.ModeRepair || len(got.Actions) != 1 || string(got.Actions[0].Content) != "x" {
		t.Fatalf("round trip %+v %v", got, err)
	}
	if err := os.Chmod(root.Path("/run/cairn/host-plan.json"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ReadRootRequest(root, "/run/cairn/host-plan.json"); err == nil {
		t.Fatal("world-writable plan file accepted")
	}
	put(t, root, "/run/cairn/junk.json", "not json")
	if err := os.Chmod(root.Path("/run/cairn/junk.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ReadRootRequest(root, "/run/cairn/junk.json"); err == nil || strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("junk plan file: %v (must fail without echoing content)", err)
	}
}
