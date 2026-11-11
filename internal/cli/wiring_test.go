package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/host/gpu"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

func TestInitWritesOnlyDetectedSwap(t *testing.T) {
	dir := hostTestEnv(t)
	old := detectSystem
	t.Cleanup(func() { detectSystem = old })
	detectSystem = func(context.Context) (detect.System, error) {
		return detect.System{Swaps: []detect.Swap{
			{Path: "/swap/other", Type: "file", Priority: 20},
			{Path: "/dev/zram0", Type: "partition", Priority: -50},
			{Path: "/hibernate-swap/swapfile", Type: "file", Priority: -10},
		}}, nil
	}
	out, err := runHost(t, "", "init", "--host")
	if err != nil || !strings.Contains(out, "cairn config explain "+swapKey) {
		t.Fatalf("init: %v\n%s", err, out)
	}
	path := filepath.Join(dir, "config/cairn/cairn.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			settings = append(settings, l)
		}
	}
	if strings.Join(settings, "\n") != "[host.sleep.hibernation_swap]\npath = \"/hibernate-swap/swapfile\"" {
		t.Fatalf("config:\n%s", data)
	}
	if out, err := runHost(t, "", "config", "explain", swapKey); err != nil || !strings.Contains(out, "/hibernate-swap/swapfile") {
		t.Fatalf("written config does not load: %v\n%s", err, out)
	}

	mine := append(data, []byte("# edited by hand\n")...)
	if err := os.WriteFile(path, mine, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runHost(t, "", "init", "--host"); err != nil || !strings.Contains(out, "leaving it unchanged") {
		t.Fatalf("second init: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(path); string(got) != string(mine) {
		t.Fatalf("second run changed the file:\n%s", got)
	}
}

func TestCheckSelfPath(t *testing.T) {
	for _, ok := range []string{"/usr/local/bin/cairn", "/opt/cairn-1.0/bin/cairn_x"} {
		if err := checkSelfPath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"/opt/my apps/cairn", "/opt/x\tcairn", "/opt/100%/cairn", `/opt/"q"/cairn`,
		"/opt/'q'/cairn", `/opt/a\b/cairn`, "/opt/$HOME/cairn", "/opt/`x`/cairn", "/opt/a;b/cairn", "/opt/a\nb/cairn"} {
		if err := checkSelfPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	old := exePath
	t.Cleanup(func() { exePath = old })
	dir := t.TempDir()
	bin := filepath.Join(dir, "my cairn")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	exePath = func() (string, error) { return bin, nil }
	if _, err := selfPath(); err == nil || !strings.Contains(err.Error(), "cannot be used in unit files") {
		t.Fatalf("selfPath: %v", err)
	}
}

type sudoRec struct {
	mode string
	yes  bool
	req  plan.RootRequest
}

// wireEnv isolates the host commands: temp XDG dirs, a temp system root,
// a fixed detection and a recording sudo.
func wireEnv(t *testing.T) (fsx.Root, *[]sudoRec) {
	t.Helper()
	hostTestEnv(t)
	oldRoot, oldDet, oldCheck, oldSudo, oldDesired := sysRoot, detectSystem, checkSelf, runSudo, hostDesired
	t.Cleanup(func() {
		sysRoot, detectSystem, checkSelf, runSudo, hostDesired = oldRoot, oldDet, oldCheck, oldSudo, oldDesired
	})
	sysRoot = fsx.Root{Dir: t.TempDir()}
	detectSystem = func(context.Context) (detect.System, error) { return detect.System{}, nil }
	checkSelf = func(fsx.Root, string) error { return nil }
	var calls []sudoRec
	runSudo = func(_ context.Context, _ *cobra.Command, _, mode string, yes, _ bool, request []byte) error {
		req, err := plan.ReadRootRequest(strings.NewReader(string(request)))
		calls = append(calls, sudoRec{mode, yes, req})
		return err
	}
	return sysRoot, &calls
}

func TestHostApplyAlwaysRunsPrivilegedStep(t *testing.T) {
	root, calls := wireEnv(t)
	hostDesired = func(context.Context, *config.Config, detect.System, string) ([]plan.Action, error) { return nil, nil }
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := plan.HostSnapshot(cfg.Host)
	if err := root.WriteAtomic(plan.HostConfigPath, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	// The user-side preview sees nothing pending, but root may: still run it.
	withRootBuilder(t, plan.Action{Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true})
	out, err := runHost(t, "", "host", "apply")
	if err != nil || len(*calls) != 1 || (*calls)[0].mode != plan.ModeApply {
		t.Fatalf("calls=%+v err=%v\n%s", *calls, err, out)
	}
	if !strings.Contains(out, "needs root: nothing to do") && !strings.Contains(out, "user-level: nothing to do") {
		t.Fatalf("out:\n%s", out)
	}

	// No root policy at all (and a fresh snapshot): no sudo.
	*calls = nil
	withRootBuilder(t)
	if _, err := runHost(t, "", "host", "apply"); err != nil || len(*calls) != 0 {
		t.Fatalf("calls=%+v err=%v", *calls, err)
	}
}

func withRootBuilder(t *testing.T, acts ...plan.Action) {
	t.Helper()
	old := plan.RootBuilder
	t.Cleanup(func() { plan.RootBuilder = old })
	plan.RootBuilder = func(string, config.Host, detect.System, string) ([]plan.Action, error) { return acts, nil }
}

func TestHostUninstallCmd(t *testing.T) {
	root, calls := wireEnv(t)
	// The registered builder derives root's removals from the system inventory.
	sysInv := &inventory.Inventory{}
	sysInv.Upsert(inventory.Entry{Path: "/etc/systemd/sleep.conf.d/50-cairn.conf", Class: inventory.DropIn})
	if err := sysInv.Save(root, plan.SysInventoryPath); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd/user/cairn-host-notify.service")
	if err := root.WriteAtomic(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userInv := &inventory.Inventory{}
	userInv.Upsert(inventory.Entry{Path: unit, Class: inventory.DropIn, SHA256: fsx.SHA256([]byte("[Unit]\n"))})
	if err := userInv.Save(root, filepath.Join(os.Getenv("XDG_STATE_HOME"), "cairn/host/managed.json")); err != nil {
		t.Fatal(err)
	}
	oldRunner := hostRunner
	t.Cleanup(func() { hostRunner = oldRunner })
	hostRunner = fakeOK{}

	out, err := runHost(t, "", "host", "uninstall", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "RemoveFile") || !strings.Contains(out, unit) {
		t.Fatalf("user plan not shown:\n%s", out)
	}
	if root.Exists(unit) {
		t.Fatal("user unit not removed")
	}
	if len(*calls) != 1 || (*calls)[0].mode != plan.ModeUninstall || !(*calls)[0].yes {
		t.Fatalf("calls=%+v", *calls)
	}
}

type fakeOK struct{}

func (fakeOK) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

func TestHostRepairGoesThroughSudo(t *testing.T) {
	_, calls := wireEnv(t)
	built := false
	old := plan.RootBuilder
	t.Cleanup(func() { plan.RootBuilder = old })
	plan.RootBuilder = func(string, config.Host, detect.System, string) ([]plan.Action, error) {
		built = true
		return nil, nil
	}
	out, err := runHost(t, "", "host", "repair")
	if err != nil || len(*calls) != 1 || (*calls)[0].mode != plan.ModeRepair || (*calls)[0].req.Mode != plan.ModeRepair {
		t.Fatalf("calls=%+v err=%v\n%s", *calls, err, out)
	}
	if built {
		t.Fatal("repair plan computed on the user side")
	}
}

func TestDoctorDetectsOnceAndQueriesVRAMLazily(t *testing.T) {
	hostTestEnv(t)
	oldRoot, oldDet, oldFree, oldVram := sysRoot, detectSystem, freeBytesOf, vramOf
	t.Cleanup(func() { sysRoot, detectSystem, freeBytesOf, vramOf = oldRoot, oldDet, oldFree, oldVram })
	sysRoot = fsx.Root{Dir: t.TempDir()}
	detects := 0
	detectSystem = func(context.Context) (detect.System, error) {
		detects++
		return detect.System{GPU: "nvidia"}, nil
	}
	vram := 0
	vramOf = func(context.Context) (uint64, error) { vram++; return 1 << 30, nil }
	freeBytesOf = func(string) (uint64, error) { return 1 << 40, nil }
	checks, err := hostChecks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if detects != 1 || vram != 0 {
		t.Fatalf("detects=%d vram queries=%d before running", detects, vram)
	}
	found := false
	for _, c := range checks {
		if c.ID() == gpu.IDSpace {
			found = true
			c.Run(context.Background())
		}
	}
	if !found || vram != 1 {
		t.Fatalf("space check found=%v vram=%d", found, vram)
	}
}

func TestSleepCheckBlocksOnNvidiaSpace(t *testing.T) {
	e := setupSC(t)
	e.sys.GPU = "nvidia"
	h, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := plan.HostSnapshot(h.Host)
	if err := e.root.WriteAtomic(plan.HostConfigPath, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	oldFree, oldVram := freeBytesOf, vramOf
	t.Cleanup(func() { freeBytesOf, vramOf = oldFree, oldVram })
	freeBytesOf = func(string) (uint64, error) { return 1 << 30, nil }
	vramOf = func(context.Context) (uint64, error) { return 8 << 30, nil }
	_, _, err = runSC(t, "--record")
	if exitCode(err) != 1 || !strings.Contains(eventLog(t, e), gpu.IDSpace) {
		t.Fatalf("err=%v log=%s", err, eventLog(t, e))
	}
}

func TestSleepCheckIgnoresInventoryWhenNvidiaUnmanaged(t *testing.T) {
	e := setupSC(t)
	e.sys.GPU = "nvidia"
	h, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	h.Host.GPU.Nvidia.Manage = false
	snap, _ := plan.HostSnapshot(h.Host)
	if err := e.root.WriteAtomic(plan.HostConfigPath, snap, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.root.WriteAtomic(plan.SysInventoryPath, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, errOut, err := runSC(t, "--record"); exitCode(err) != 0 {
		t.Fatalf("err=%v\n%s\n%s", err, out, errOut)
	}
}
