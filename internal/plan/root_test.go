package plan_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const (
	selfBin    = "/usr/local/bin/cairn"
	nvidiaTmp  = "/home/.nvidia-suspend"
	conflicter = "/etc/systemd/system/sleep-fallback.service"
)

type builderFunc = func(string, config.Host, detect.System, string) ([]plan.Action, error)

func withBuilder(t *testing.T, f builderFunc) {
	t.Helper()
	old := plan.RootBuilder
	t.Cleanup(func() { plan.RootBuilder = old })
	plan.RootBuilder = f
}

func returns(actions ...plan.Action) builderFunc {
	return func(string, config.Host, detect.System, string) ([]plan.Action, error) { return actions, nil }
}

func validHost() config.Host {
	var h config.Host
	h.Sleep.Lid, h.Sleep.LidAC = "suspend-then-hibernate", "suspend-then-hibernate"
	h.Sleep.CriticalBattery, h.Sleep.IdleBattery = "hibernate", "hibernate"
	h.Sleep.HibernateAfter = config.Duration{Duration: 15 * time.Minute}
	h.GPU.Nvidia.Manage = true
	h.GPU.Nvidia.TemporaryFilePath = nvidiaTmp
	return h
}

func rootEnv(t *testing.T) plan.Env {
	t.Helper()
	env, _ := newEnv(t)
	if err := env.Root.MkdirAll("/home", 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

// expectFor computes what the user side would show for mode and host.
func expectFor(t *testing.T, env plan.Env, mode string, host config.Host) string {
	t.Helper()
	acts, err := plan.BuildRoot(mode, host, detect.System{}, selfBin)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Hash(plan.Resolve(acts, env.Root, env.SysInv).Actions)
}

func applyRoot(env plan.Env, mode string, host config.Host, expect string, force bool) error {
	return plan.ApplyRoot(context.Background(), plan.NewRootRequest(mode, host), env,
		plan.RootOptions{Expect: expect, Force: force, Self: selfBin})
}

func nothingWritten(t *testing.T, env plan.Env, paths ...string) {
	t.Helper()
	for _, p := range append(paths, plan.HostConfigPath, plan.SysInventoryPath) {
		if env.Root.Exists(p) {
			t.Errorf("%s written by a rejected run", p)
		}
	}
	if fr := env.Runner.(*execx.FakeRunner); len(fr.Calls) != 0 {
		t.Errorf("commands ran in a rejected run")
	}
}

func TestApplyRootRegeneratesAndWritesSnapshot(t *testing.T) {
	env := rootEnv(t)
	var gotMode, gotSelf string
	var gotHost config.Host
	withBuilder(t, func(mode string, h config.Host, _ detect.System, self string) ([]plan.Action, error) {
		gotMode, gotHost, gotSelf = mode, h, self
		return []plan.Action{
			{Kind: plan.MakeDir, Path: nvidiaTmp, Mode: 0o700, NeedsRoot: true, Class: inventory.ManagedDir},
			{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true},
			{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true},
		}, nil
	})
	host := validHost()
	if err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false); err != nil {
		t.Fatal(err)
	}
	if gotMode != plan.ModeApply || gotSelf != selfBin || gotHost.GPU.Nvidia.TemporaryFilePath != nvidiaTmp {
		t.Fatalf("builder got mode=%q self=%q host=%+v", gotMode, gotSelf, gotHost)
	}
	st, err := env.Root.Stat(plan.HostConfigPath)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("snapshot: %v %v", st, err)
	}
	data, _ := env.Root.ReadFile(plan.HostConfigPath)
	var h config.Host
	if err := json.Unmarshal(data, &h); err != nil || h.Sleep.HibernateAfter.Duration != 15*time.Minute || h.GPU.Nvidia.TemporaryFilePath != nvidiaTmp {
		t.Fatalf("snapshot content %s: %v", data, err)
	}
	if st, err := os.Lstat(env.Root.Path(nvidiaTmp)); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("temp dir: %v %v", st, err)
	}
	saved, err := inventory.Load(env.Root, plan.SysInventoryPath)
	if err != nil || len(saved.Entries) != 2 {
		t.Fatalf("saved inventory %+v %v", saved, err)
	}
	// Second run: nothing pending, the expected hash is the empty plan's.
	if err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRootExpectMismatchAborts(t *testing.T) {
	env := rootEnv(t)
	host := validHost()
	withBuilder(t, returns(plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("shown"), NeedsRoot: true}))
	shown := expectFor(t, env, plan.ModeApply, host)
	// What root regenerates now differs from what the user approved.
	withBuilder(t, returns(plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("swapped"), NeedsRoot: true}))
	for _, expect := range []string{shown, "", "deadbeef"} {
		err := applyRoot(env, plan.ModeApply, host, expect, false)
		if !errors.Is(err, plan.ErrPlanChanged) || !strings.Contains(err.Error(), "run cairn host apply again") {
			t.Fatalf("expect %q: err=%v", expect, err)
		}
		nothingWritten(t, env, sleepConf)
	}
}

func TestApplyRootRepairMode(t *testing.T) {
	env := rootEnv(t)
	host := validHost()
	put(t, env.Root, conflicter, "[Service]\nExecStart=/usr/bin/systemctl suspend\n")
	withBuilder(t, returns(plan.Action{Kind: plan.RemoveFile, Path: conflicter, NeedsRoot: true}))

	if err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false); err == nil {
		t.Fatalf("apply mode accepted removal of %s", conflicter)
	}
	if !env.Root.Exists(conflicter) {
		t.Fatal("rejected run removed the file")
	}
	nothingWritten(t, env)

	if err := applyRoot(env, plan.ModeRepair, host, expectFor(t, env, plan.ModeRepair, host), false); err != nil {
		t.Fatal(err)
	}
	if env.Root.Exists(conflicter) {
		t.Fatal("repair did not remove the conflicting unit")
	}
	if got, _ := env.Root.ReadFile("/var/lib/cairn/host/backup/%etc%systemd%system%sleep-fallback.service"); !strings.Contains(string(got), "systemctl suspend") {
		t.Fatalf("backup missing: %q", got)
	}

	boot := plan.Action{Kind: plan.WriteFile, Path: "/boot/loader/entries/arch.conf", Content: []byte("options resume_offset=1\n"), NeedsRoot: true}
	withBuilder(t, returns(boot))
	if err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false); err == nil {
		t.Fatal("boot entry write accepted in apply mode")
	}
	if err := applyRoot(env, plan.ModeRepair, host, expectFor(t, env, plan.ModeRepair, host), false); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRootRejectsBadRequests(t *testing.T) {
	withBuilder(t, returns(plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true}))
	cases := map[string]func(r *plan.RootRequest){
		"bad version":    func(r *plan.RootRequest) { r.Version = 99 },
		"bad mode":       func(r *plan.RootRequest) { r.Mode = "yolo" },
		"extra /etc":     func(r *plan.RootRequest) { r.Extra = []string{"/etc"} },
		"extra mismatch": func(r *plan.RootRequest) { r.Extra = []string{"/srv/other"} },
		"bad lid":        func(r *plan.RootRequest) { r.Host.Sleep.Lid = "explode" },
		"newline in path": func(r *plan.RootRequest) {
			r.Host.GPU.Nvidia.TemporaryFilePath = "/home/x\n/etc"
			r.Extra = []string{"/home/x\n/etc"}
		},
		"dotdot in path": func(r *plan.RootRequest) {
			r.Host.GPU.Nvidia.TemporaryFilePath = "/home/../etc"
			r.Extra = []string{"/home/../etc"}
		},
		"swap dotdot": func(r *plan.RootRequest) { r.Host.Sleep.HibernationSwap.Path = "/swap/../etc/shadow" },
		"system extra": func(r *plan.RootRequest) {
			r.Host.GPU.Nvidia.TemporaryFilePath = "/etc/x"
			r.Extra = []string{"/etc/x"}
		},
	}
	for name, mutate := range cases {
		env := rootEnv(t)
		req := plan.NewRootRequest(plan.ModeApply, validHost())
		expect := expectFor(t, env, plan.ModeApply, validHost())
		mutate(&req)
		if err := plan.ApplyRoot(context.Background(), req, env, plan.RootOptions{Expect: expect, Self: selfBin}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		nothingWritten(t, env, sleepConf)
	}
}

func TestApplyRootRejectsBadBuilderOutput(t *testing.T) {
	good := plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true}
	cases := map[string]builderFunc{
		"user action":    returns(good, plan.Action{Kind: plan.SetGSetting, Schema: "a", Key: "b", Value: "c"}),
		"not allowed":    returns(good, plan.Action{Kind: plan.WriteFile, Path: "/etc/passwd", NeedsRoot: true}),
		"sshd drop-in":   returns(good, plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/system/sshd.service.d/50-cairn.conf", NeedsRoot: true}),
		"bad command":    returns(good, plan.Action{Kind: plan.RunCommand, Cmd: []string{"rm", "-rf", "/"}, NeedsRoot: true}),
		"world writable": returns(good, plan.Action{Kind: plan.WriteFile, Path: "/etc/systemd/logind.conf.d/50-cairn.conf", Mode: 0o666, NeedsRoot: true}),
		"setuid":         returns(good, plan.Action{Kind: plan.WriteFile, Path: "/usr/lib/systemd/system-sleep/50-cairn", Mode: 0o755 | fs.ModeSetuid, NeedsRoot: true}),
		"builder error": func(string, config.Host, detect.System, string) ([]plan.Action, error) {
			return nil, errors.New("boom")
		},
	}
	for name, b := range cases {
		env := rootEnv(t)
		withBuilder(t, b)
		if err := applyRoot(env, plan.ModeApply, validHost(), "", false); err == nil || errors.Is(err, plan.ErrPlanChanged) {
			t.Errorf("%s: err=%v, want a validation error", name, err)
		}
		nothingWritten(t, env, sleepConf)
	}
}

func TestApplyRootModifiedNeedsForce(t *testing.T) {
	env := rootEnv(t)
	host := validHost()
	put(t, env.Root, sleepConf, "hand edit\n")
	env.SysInv.Upsert(inventory.Entry{Path: sleepConf, SHA256: fsx.SHA256([]byte("old managed"))})
	withBuilder(t, returns(plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("x"), NeedsRoot: true}))
	expect := expectFor(t, env, plan.ModeApply, host)
	if err := applyRoot(env, plan.ModeApply, host, expect, false); !errors.Is(err, plan.ErrModifiedByUser) {
		t.Fatalf("err=%v, want ErrModifiedByUser", err)
	}
	if err := applyRoot(env, plan.ModeApply, host, expect, true); err != nil {
		t.Fatal(err)
	}
}

func TestReadRootRequest(t *testing.T) {
	req := plan.NewRootRequest(plan.ModeRepair, validHost())
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.ReadRootRequest(bytes.NewReader(data))
	if err != nil || got.Mode != plan.ModeRepair || len(got.Extra) != 1 || got.Extra[0] != nvidiaTmp {
		t.Fatalf("round trip %+v %v", got, err)
	}
	big := append(bytes.Repeat([]byte(" "), plan.MaxRootRequest), data...)
	if _, err := plan.ReadRootRequest(bytes.NewReader(big)); err == nil {
		t.Fatal("oversized request accepted")
	}
	if _, err := plan.ReadRootRequest(strings.NewReader("root:secret:1:2")); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("junk: %v (must fail without echoing input)", err)
	}
	if _, err := plan.ReadRootRequest(strings.NewReader(`{"version":1,"mode":"apply","actions":[]}`)); err == nil {
		t.Fatal("unknown field (actions) accepted")
	}
	if _, err := plan.ReadRootRequest(strings.NewReader(string(data) + `{}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestMakeDirRefusesSymlink(t *testing.T) {
	env := rootEnv(t)
	if err := env.Root.MkdirAll("/etc/target", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(env.Root.Path("/etc/target"), env.Root.Path(nvidiaTmp)); err != nil {
		t.Fatal(err)
	}
	withBuilder(t, returns(plan.Action{Kind: plan.MakeDir, Path: nvidiaTmp, Mode: 0o700, NeedsRoot: true}))
	host := validHost()
	err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v, want symlink refusal", err)
	}
	if st, _ := os.Stat(env.Root.Path("/etc/target")); st.Mode().Perm() != 0o755 {
		t.Fatalf("symlink target chmodded to %v", st.Mode().Perm())
	}
}

func TestMakeDirRefusesUntrustedParent(t *testing.T) {
	env := rootEnv(t)
	env.OwnerOf = func(fi fs.FileInfo) (uint32, bool) {
		if fi.Name() == "home" {
			return 1000, true
		}
		return 0, true
	}
	withBuilder(t, returns(plan.Action{Kind: plan.MakeDir, Path: nvidiaTmp, Mode: 0o700, NeedsRoot: true}))
	host := validHost()
	err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false)
	if err == nil || !strings.Contains(err.Error(), "uid 1000") {
		t.Fatalf("err=%v, want untrusted parent refusal", err)
	}
	if env.Root.Exists(nvidiaTmp) {
		t.Fatal("directory created under an untrusted parent")
	}
}

func TestMakeDirExtraParentMustExist(t *testing.T) {
	env, _ := newEnv(t) // no /home
	withBuilder(t, returns(plan.Action{Kind: plan.MakeDir, Path: nvidiaTmp, Mode: 0o700, NeedsRoot: true}))
	host := validHost()
	if err := applyRoot(env, plan.ModeApply, host, expectFor(t, env, plan.ModeApply, host), false); err == nil {
		t.Fatal("extra dir created with a missing parent")
	}
	if env.Root.Exists("/home") {
		t.Fatal("parents of an extra path were created")
	}
}

func TestHash(t *testing.T) {
	a := plan.Action{Kind: plan.WriteFile, Path: sleepConf, Content: []byte("a"), NeedsRoot: true, State: plan.Create}
	b := a
	b.Content = []byte("b")
	user := plan.Action{Kind: plan.WriteFile, Path: "/x", Content: []byte("u"), State: plan.Create}
	same := a
	same.State = plan.Unchanged
	h := plan.Hash([]plan.Action{a})
	if h == plan.Hash([]plan.Action{b}) {
		t.Error("content not covered")
	}
	if h != plan.Hash([]plan.Action{a, user, same}) {
		t.Error("user-level and unchanged actions must not affect the hash")
	}
	m := a
	m.Mode = 0o600
	if h == plan.Hash([]plan.Action{m}) {
		t.Error("mode not covered")
	}
	c := plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true, State: plan.Update}
	if plan.Hash([]plan.Action{a, c}) == plan.Hash([]plan.Action{c, a}) {
		t.Error("order not covered")
	}
	c2 := c
	c2.Cmd = []string{"systemctl daemon-reload"}
	if plan.Hash([]plan.Action{c}) == plan.Hash([]plan.Action{c2}) {
		t.Error("argv boundaries not covered")
	}
}
