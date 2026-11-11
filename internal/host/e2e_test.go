package host

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const (
	e2eSelf     = "/usr/local/bin/cairn"
	e2eUnitDir  = "/config/systemd/user"
	e2eUserInv  = "/state/cairn/host/managed.json"
	e2eNvTmp    = "/home/.nvidia-suspend"
	e2eGsSchema = "org.gnome.settings-daemon.plugins.power"
	e2eGsKey    = "sleep-inactive-battery-type"
	foreignMask = "options nvidia NVreg_DynamicPowerManagement=0x02\n"
	vendorConf  = "options nvidia NVreg_PreserveVideoMemoryAllocations=1 NVreg_TemporaryFilePath=/var/tmp\n"
)

var errExit1 = errors.New("exit status 1")

type roundTrip struct {
	t                *testing.T
	sysRoot, usrRoot fsx.Root
	runner           *execx.FakeRunner
	cfg              *config.Config
	sys              detect.System
	ttyOut           bytes.Buffer
}

func newRoundTrip(t *testing.T) *roundTrip {
	t.Helper()
	rt := &roundTrip{
		t:       t,
		sysRoot: fsx.Root{Dir: t.TempDir()},
		usrRoot: fsx.Root{Dir: t.TempDir()},
		runner: &execx.FakeRunner{Responses: map[string]execx.Response{
			"systemctl daemon-reload":                                        {},
			"systemctl --user daemon-reload":                                 {},
			"gsettings get " + e2eGsSchema + " " + e2eGsKey:                  {Out: []byte("'suspend'\n")},
			"gsettings set " + e2eGsSchema + " " + e2eGsKey + " 'hibernate'": {},
			"gsettings set " + e2eGsSchema + " " + e2eGsKey + " 'suspend'":   {},
		}},
		sys: detect.System{
			PkgManager: "pacman", GPU: "nvidia", Desktop: "gnome", BootLoader: "systemd-boot",
			MemSleepDeep: true, UPowerDropIns: true,
		},
	}
	for _, u := range []string{"cairn-host-notify.path", "cairn-host-boot-check.service"} {
		rt.runner.Responses["systemctl --user is-enabled "+u] = execx.Response{Out: []byte("disabled\n"), Err: errExit1}
		rt.runner.Responses["systemctl --user enable "+u] = execx.Response{}
		rt.runner.Responses["systemctl --user disable "+u] = execx.Response{}
	}
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host.GPU.Nvidia.TemporaryFilePath = e2eNvTmp
	rt.cfg = cfg
	for p, c := range map[string]string{
		"/usr/lib/modprobe.d/nvidia-utils.conf": vendorConf,
		"/etc/modprobe.d/nvidia-utils.conf":     foreignMask,
	} {
		if err := rt.sysRoot.WriteAtomic(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.sysRoot.MkdirAll("/home", 0o755); err != nil {
		t.Fatal(err)
	}
	old := plan.RootBuilder
	t.Cleanup(func() { plan.RootBuilder = old })
	plan.RootBuilder = Builder{Root: rt.sysRoot, Offset: func(string) (uint64, error) { return 0, errors.New("unused") }}.Build
	return rt
}

func (rt *roundTrip) envs() (userEnv, rootEnv plan.Env) {
	rt.t.Helper()
	userInv, err := inventory.Load(rt.usrRoot, e2eUserInv)
	if err != nil {
		rt.t.Fatal(err)
	}
	sysInv, err := inventory.Load(rt.sysRoot, plan.SysInventoryPath)
	if err != nil {
		rt.t.Fatal(err)
	}
	userEnv = plan.Env{Root: rt.usrRoot, Runner: rt.runner, UserInv: userInv, UserInvPath: e2eUserInv,
		UserBackupDir: "/state/cairn/host/backup"}
	rootEnv = plan.Env{Root: rt.sysRoot, Runner: rt.runner, SysInv: sysInv, SysInvPath: plan.SysInventoryPath,
		OwnerOf: func(fs.FileInfo) (uint32, bool) { return 0, true }}
	return userEnv, rootEnv
}

func (rt *roundTrip) userActs(mode string, userEnv plan.Env) []plan.Action {
	if mode == plan.ModeUninstall {
		return UninstallPlan(nil, userEnv.UserInv).Actions
	}
	return UserActions(rt.cfg, rt.sys, e2eSelf, e2eUnitDir)
}

// plan returns the pending actions of both sides, as `cairn host plan` sees
// them, plus whether the host config snapshot is stale.
func (rt *roundTrip) plan(mode string) (pending []plan.Action, stale bool) {
	rt.t.Helper()
	userEnv, rootEnv := rt.envs()
	u, r, err := PlanBoth(mode, rt.cfg.Host, rt.sys, e2eSelf, rt.userActs(mode, userEnv), userEnv, rootEnv)
	if err != nil {
		rt.t.Fatal(err)
	}
	return append(u.Pending(), r.Pending()...), plan.SnapshotStale(rt.sysRoot, rt.cfg.Host)
}

// run performs mode on both sides the way the CLI does, with the root step
// answering "y" on its terminal.
func (rt *roundTrip) run(mode string) {
	rt.t.Helper()
	if err := rt.runWith(mode, "y\n"); err != nil {
		rt.t.Fatalf("%s: %v\nroot output:\n%s", mode, err, rt.ttyOut.String())
	}
}

func key(a plan.Action) string { return string(a.State) + " " + describe(a) }

func TestRoundTrip(t *testing.T) {
	rt := newRoundTrip(t)
	before, stale := rt.plan(plan.ModeApply)
	if !stale || len(before) == 0 {
		t.Fatalf("fresh system: stale=%v pending=%d", stale, len(before))
	}

	rt.run(plan.ModeApply)
	if out := rt.ttyOut.String(); !strings.Contains(out, "Apply these root changes? [y/N]") ||
		!strings.Contains(out, PacmanHookPath) || !strings.Contains(out, "config snapshot") {
		t.Fatalf("root did not show and confirm its plan:\n%s", out)
	}

	rootFiles := []string{
		"/etc/systemd/logind.conf.d/50-cairn.conf",
		"/etc/systemd/sleep.conf.d/50-cairn.conf",
		"/etc/systemd/system/systemd-hibernate.service.d/50-cairn.conf",
		"/etc/systemd/system/systemd-suspend-then-hibernate.service.d/50-cairn.conf",
		"/etc/systemd/system/cairn-sleep-fallback.service",
		"/usr/lib/systemd/system-sleep/50-cairn",
		"/etc/UPower/UPower.conf.d/50-cairn.conf",
		PacmanHookPath,
	}
	userFiles := []string{
		e2eUnitDir + "/cairn-host-notify.path",
		e2eUnitDir + "/cairn-host-notify.service",
		e2eUnitDir + "/cairn-host-boot-check.service",
	}
	for _, p := range rootFiles {
		if !rt.sysRoot.Exists(p) {
			t.Errorf("%s not written", p)
		}
	}
	for _, p := range userFiles {
		if !rt.usrRoot.Exists(p) {
			t.Errorf("%s not written", p)
		}
	}
	if b, _ := rt.sysRoot.ReadFile("/etc/modprobe.d/nvidia-utils.conf"); !strings.Contains(string(b), "NVreg_TemporaryFilePath="+e2eNvTmp) {
		t.Fatalf("mask not written: %q", b)
	}
	if !rt.sysRoot.Exists(e2eNvTmp) || !rt.sysRoot.Exists(plan.HostConfigPath) {
		t.Fatal("nvidia dir or config snapshot missing")
	}
	userEnv, rootEnv := rt.envs()
	for _, p := range append(rootFiles, plan.SysStateDir, e2eNvTmp, "/etc/modprobe.d/nvidia-utils.conf") {
		if _, ok := rootEnv.SysInv.Get(p); !ok {
			t.Errorf("system inventory lacks %s", p)
		}
	}
	for _, p := range append(userFiles, "user-unit:cairn-host-notify.path", "user-unit:cairn-host-boot-check.service",
		"gsettings:"+e2eGsSchema+" "+e2eGsKey) {
		if _, ok := userEnv.UserInv.Get(p); !ok {
			t.Errorf("user inventory lacks %s", p)
		}
	}

	if again, stale := rt.plan(plan.ModeApply); len(again) != 0 || stale {
		var ks []string
		for _, a := range again {
			ks = append(ks, key(a))
		}
		t.Fatalf("second plan not empty (stale=%v):\n%s", stale, strings.Join(ks, "\n"))
	}

	rt.runner.Calls = nil
	rt.run(plan.ModeUninstall)

	for _, p := range append(rootFiles, e2eNvTmp) {
		if rt.sysRoot.Exists(p) {
			t.Errorf("%s left after uninstall", p)
		}
	}
	for _, p := range userFiles {
		if rt.usrRoot.Exists(p) {
			t.Errorf("%s left after uninstall", p)
		}
	}
	if b, _ := rt.sysRoot.ReadFile("/etc/modprobe.d/nvidia-utils.conf"); string(b) != foreignMask {
		t.Fatalf("foreign nvidia-utils.conf not restored: %q", b)
	}
	if !slices.ContainsFunc(rt.runner.Calls, func(c []string) bool {
		return strings.Join(c, " ") == "gsettings set "+e2eGsSchema+" "+e2eGsKey+" 'suspend'"
	}) {
		t.Fatalf("gsetting not restored: %q", rt.runner.Calls)
	}
	for _, u := range []string{"cairn-host-notify.path", "cairn-host-boot-check.service"} {
		if !slices.ContainsFunc(rt.runner.Calls, func(c []string) bool {
			return strings.Join(c, " ") == "systemctl --user disable "+u
		}) {
			t.Errorf("%s not disabled", u)
		}
	}

	// The plan is back to its pre-apply state: only creations, plus the
	// restored foreign file, which is foreign again.
	after, _ := rt.plan(plan.ModeApply)
	var want, got []string
	for _, a := range before {
		if a.Path != plan.SysStateDir { // kept: it holds the inventory and backups
			want = append(want, key(a))
		}
	}
	for _, a := range after {
		got = append(got, key(a))
		restored := a.State == plan.Foreign && a.Path == "/etc/modprobe.d/nvidia-utils.conf"
		if a.State != plan.Create && !restored && a.Kind != plan.RunCommand {
			t.Errorf("after uninstall: %s", key(a))
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan after uninstall:\n%s\nwant (as before apply):\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Uninstalling twice changes nothing.
	if again, _ := rt.plan(plan.ModeUninstall); len(again) != 0 {
		t.Fatalf("second uninstall pending: %v", again)
	}
}

func TestApplyRootRepairPrintsBuilderNotes(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	if err := root.WriteAtomic("/boot/loader/loader.conf", []byte("default arch.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real file outside root: its device has no node under root's /sys.
	swap := t.TempDir() + "/swapfile"
	if err := os.WriteFile(swap, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := Builder{Root: root, Offset: func(string) (uint64, error) { return 7, nil }}
	oldB, oldN := plan.RootBuilder, plan.RootNotes
	t.Cleanup(func() { plan.RootBuilder, plan.RootNotes = oldB, oldN })
	plan.RootBuilder, plan.RootNotes = b.Build, b.Notes
	sys := detect.System{BootLoader: "systemd-boot", KernelRunning: "6.1.0-test",
		Swaps: []detect.Swap{{Path: swap, Type: "file"}}}
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := plan.Env{Root: root, Runner: &execx.FakeRunner{}, OwnerOf: func(fs.FileInfo) (uint32, bool) { return 0, true }}
	if err := ApplyRootSide(context.Background(), plan.NewRootRequest(plan.ModeRepair, cfg.Host), env,
		plan.RootOptions{Yes: true, Sys: sys, Self: e2eSelf, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "note: no block device node for "+swap) {
		t.Fatalf("repair note not printed:\n%s", out.String())
	}
}

// runWith performs mode like run, with the root step answering tty.
func (rt *roundTrip) runWith(mode, tty string) error {
	rt.t.Helper()
	userEnv, rootEnv := rt.envs()
	u, r, err := PlanBoth(mode, rt.cfg.Host, rt.sys, e2eSelf, rt.userActs(mode, userEnv), userEnv, rootEnv)
	if err != nil {
		rt.t.Fatal(err)
	}
	priv := func(ctx context.Context, req plan.RootRequest) error {
		return ApplyRootSide(ctx, req, rootEnv, plan.RootOptions{
			Sys: rt.sys, Self: e2eSelf, Out: &rt.ttyOut, TTY: strings.NewReader(tty),
		})
	}
	return ApplyBoth(context.Background(), u, r, userEnv, plan.NewRootRequest(mode, rt.cfg.Host), false, priv)
}

func TestApplyRootDeclinedAppliesNoUserAction(t *testing.T) {
	rt := newRoundTrip(t)
	err := rt.runWith(plan.ModeApply, "n\n")
	if !errors.Is(err, plan.ErrRootDeclined) || !strings.Contains(err.Error(), "user-level changes were not applied") {
		t.Fatalf("err=%v", err)
	}
	for _, c := range rt.runner.Calls {
		if c[0] == "gsettings" || (len(c) > 1 && c[1] == "--user") {
			t.Errorf("user command ran: %q", c)
		}
	}
	for _, p := range []string{"cairn-host-notify.path", "cairn-host-notify.service", "cairn-host-boot-check.service"} {
		if rt.usrRoot.Exists(e2eUnitDir + "/" + p) {
			t.Errorf("user unit %s written", p)
		}
	}
	if rt.usrRoot.Exists(e2eUserInv) {
		t.Error("user inventory written")
	}
}

func TestApplyBothRejectsInvalidRootPreview(t *testing.T) {
	rt := newRoundTrip(t)
	userEnv, _ := rt.envs()
	u := plan.Resolve(rt.userActs(plan.ModeApply, userEnv), userEnv.Root, userEnv.UserInv)
	bad := plan.Plan{Mode: plan.ModeApply, Actions: []plan.Action{{Kind: plan.WriteFile, Path: "/etc/passwd", NeedsRoot: true, State: plan.Create}}}
	called := false
	priv := func(context.Context, plan.RootRequest) error { called = true; return nil }
	err := ApplyBoth(context.Background(), u, bad, userEnv, plan.NewRootRequest(plan.ModeApply, rt.cfg.Host), false, priv)
	if err == nil || !strings.Contains(err.Error(), "/etc/passwd") || called || len(rt.runner.Calls) != 0 {
		t.Fatalf("err=%v called=%v calls=%q", err, called, rt.runner.Calls)
	}
	req := plan.NewRootRequest(plan.ModeApply, rt.cfg.Host)
	req.Host.Sleep.Lid = "explode"
	if err := ApplyBoth(context.Background(), u, plan.Plan{}, userEnv, req, false, priv); err == nil || called {
		t.Fatalf("invalid request accepted: %v", err)
	}
}
