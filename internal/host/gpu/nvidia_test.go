package gpu

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const (
	cfgPath = "/home/.nvidia-suspend"
	gib     = 1 << 30
)

func vendor(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/gpu/nvidia-utils.conf")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cfg() config.Nvidia { return config.Nvidia{Manage: true, TemporaryFilePath: cfgPath} }

func nv() detect.System {
	return detect.System{GPU: "nvidia", NvidiaParams: map[string]string{"TemporaryFilePath": `"/var/tmp"`}}
}

func TestMaskReplacesTemporaryPath(t *testing.T) {
	acts := NvidiaActions(cfg(), nv(), vendor(t))
	if len(acts) != 2 {
		t.Fatalf("got %d actions: %+v", len(acts), acts)
	}
	m := acts[0]
	if m.Kind != plan.WriteFile || m.Path != VendorMask || !m.NeedsRoot || m.Class != inventory.Mask ||
		m.Policy != Policy || m.Mode != 0o644 || m.Upstream != VendorUpstream {
		t.Fatalf("mask action: %+v", m)
	}
	s := string(m.Content)
	for _, want := range []string{
		fsx.Header(Policy),
		"blacklist nouveau",
		"softdep nvidia post: nvidia-uvm nvidia-drm",
		"options nvidia NVreg_TemporaryFilePath=/home/.nvidia-suspend NVreg_PreserveVideoMemoryAllocations=1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "=/var/tmp") {
		t.Errorf("old path left:\n%s", s)
	}
	d := acts[1]
	if d.Kind != plan.MakeDir || d.Path != cfgPath || d.Mode != 0o700 || !d.NeedsRoot || d.Class != inventory.ManagedDir {
		t.Fatalf("dir action: %+v", d)
	}
	if string(NvidiaActions(cfg(), nv(), vendor(t))[0].Content) != s {
		t.Fatal("not deterministic")
	}
}

func TestMaskLineRewriting(t *testing.T) {
	c := config.Nvidia{Manage: true, TemporaryFilePath: "/var/tmp"}
	in := "# c\n  options nvidia NVreg_PreserveVideoMemoryAllocations=0 NVreg_TemporaryFilePath=/tmp foo=1\noptions nvidia_drm modeset=1\n"
	acts := NvidiaActions(c, nv(), []byte(in))
	if len(acts) != 1 {
		t.Fatalf("/var/tmp must not make a dir: %+v", acts)
	}
	want := fsx.Header(Policy) + "# c\n  options nvidia NVreg_PreserveVideoMemoryAllocations=1 NVreg_TemporaryFilePath=/var/tmp foo=1\noptions nvidia_drm modeset=1\n"
	if got := string(acts[0].Content); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	got := string(NvidiaActions(c, nv(), []byte("blacklist nouveau\n"))[0].Content)
	want = fsx.Header(Policy) + "blacklist nouveau\noptions nvidia NVreg_PreserveVideoMemoryAllocations=1 NVreg_TemporaryFilePath=/var/tmp\n"
	if got != want {
		t.Fatalf("append: got:\n%s", got)
	}
}

func TestNoActionsUnlessManagedNvidia(t *testing.T) {
	c := cfg()
	c.Manage = false
	if len(NvidiaActions(c, nv(), vendor(t))) != 0 {
		t.Fatal("manage=false")
	}
	if len(NvidiaActions(cfg(), detect.System{GPU: "intel"}, vendor(t))) != 0 {
		t.Fatal("non-nvidia")
	}
}

func TestForeignMaskBackedUpAndRestored(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	const foreign = "# user's own mask\n"
	if err := root.WriteAtomic(VendorMask, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteAtomic(VendorUpstream, vendor(t), 0o644); err != nil {
		t.Fatal(err)
	}
	env := plan.Env{
		Root: root, Runner: &execx.FakeRunner{},
		UserInv: &inventory.Inventory{}, SysInv: &inventory.Inventory{},
		UserInvPath: "/state/managed.json", SysInvPath: plan.SysInventoryPath,
		OwnerOf: func(os.FileInfo) (uint32, bool) { return 0, true },
	}
	acts := NvidiaActions(cfg(), nv(), vendor(t))
	p := plan.ResolveAll(acts, root, env.UserInv, env.SysInv)
	if p.Actions[0].State != plan.Foreign {
		t.Fatalf("state %v", p.Actions[0].State)
	}
	extra := plan.ExtraPaths(config.Host{GPU: config.GPU{Nvidia: cfg()}})
	if err := root.MkdirAll("/home", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(context.Background(), p, env, plan.Options{Extra: extra}); err != nil {
		t.Fatal(err)
	}
	e, ok := env.SysInv.Get(VendorMask)
	if !ok || e.Backup == "" || e.Class != inventory.Mask || e.Upstream != VendorUpstream ||
		e.UpstreamSHA256 != fsx.SHA256(vendor(t)) {
		t.Fatalf("entry: %+v %v", e, ok)
	}
	b, err := root.ReadFile(e.Backup)
	if err != nil || string(b) != foreign {
		t.Fatalf("backup: %q %v", b, err)
	}
}

func results(cs []doctor.Check) map[string]doctor.Result {
	out := map[string]doctor.Result{}
	for _, r := range doctor.Run(context.Background(), cs) {
		out[r.ID] = r
	}
	return out
}

func vr(n uint64) func() uint64 { return func() uint64 { return n } }

func TestVRAMQueriedOnlyBySpaceCheck(t *testing.T) {
	calls := 0
	vram := func() uint64 { calls++; return gib }
	cs := NvidiaChecks(cfg(), nv(), fsx.Root{Dir: t.TempDir()}, &inventory.Inventory{}, free(100*gib), vram)
	if calls != 0 {
		t.Fatalf("VRAM queried while building checks")
	}
	for _, c := range cs {
		if c.ID() != IDSpace {
			c.Run(context.Background())
		}
	}
	if calls != 0 {
		t.Fatalf("VRAM queried by a check other than %s", IDSpace)
	}
}

func free(n uint64) func(string) (uint64, error) {
	return func(string) (uint64, error) { return n, nil }
}

func TestParamsMismatchWarnsReboot(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	inv := &inventory.Inventory{}
	if err := root.WriteAtomic(VendorMask, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := results(NvidiaChecks(cfg(), nv(), root, inv, free(100*gib), vr(8*gib)))[IDParams]
	if r.Status != doctor.Warn || r.Fix != "reboot to load the new NVIDIA options" ||
		r.Evidence != "configured "+cfgPath+", driver uses /var/tmp" {
		t.Fatalf("%+v", r)
	}
	root2 := fsx.Root{Dir: t.TempDir()}
	r = results(NvidiaChecks(cfg(), nv(), root2, inv, free(100*gib), vr(8*gib)))[IDParams]
	if r.Status != doctor.Warn || r.Fix != "cairn host apply" {
		t.Fatalf("%+v", r)
	}
	sys := nv()
	sys.NvidiaParams["TemporaryFilePath"] = `"` + cfgPath + `"`
	if r = results(NvidiaChecks(cfg(), sys, root, inv, free(100*gib), vr(8*gib)))[IDParams]; r.Status != doctor.OK {
		t.Fatalf("%+v", r)
	}
}

func TestNotEnoughSpaceCrit(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	inv := &inventory.Inventory{}
	if r := results(NvidiaChecks(cfg(), nv(), root, inv, free(8*gib), vr(8*gib)))[IDSpace]; r.Status != doctor.Crit {
		t.Fatalf("%+v", r)
	}
	if r := results(NvidiaChecks(cfg(), nv(), root, inv, free(9*gib), vr(8*gib)))[IDSpace]; r.Status != doctor.OK {
		t.Fatalf("%+v", r)
	}
	if r := results(NvidiaChecks(cfg(), nv(), root, inv, free(9*gib), vr(0)))[IDSpace]; r.Status != doctor.Unknown {
		t.Fatalf("no vram: %+v", r)
	}
	bad := func(string) (uint64, error) { return 0, errors.New("boom") }
	if r := results(NvidiaChecks(cfg(), nv(), root, inv, bad, vr(8*gib)))[IDSpace]; r.Status != doctor.Unknown {
		t.Fatalf("statfs err: %+v", r)
	}
}

func TestUpstreamChangedWarns(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	if err := root.WriteAtomic(VendorUpstream, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := &inventory.Inventory{}
	if r := results(NvidiaChecks(cfg(), nv(), root, inv, free(100*gib), vr(gib)))[IDUpstream]; r.Status != doctor.OK {
		t.Fatalf("nothing managed: %+v", r)
	}
	inv.Upsert(inventory.Entry{Path: VendorMask, Class: inventory.Mask, Upstream: VendorUpstream, UpstreamSHA256: fsx.SHA256([]byte("v1"))})
	r := results(NvidiaChecks(cfg(), nv(), root, inv, free(100*gib), vr(gib)))[IDUpstream]
	if r.Status != doctor.Warn || r.Evidence != "vendor file changed: run cairn host apply" {
		t.Fatalf("%+v", r)
	}
	inv.Upsert(inventory.Entry{Path: VendorMask, Class: inventory.Mask, Upstream: VendorUpstream, UpstreamSHA256: fsx.SHA256([]byte("v2"))})
	if r = results(NvidiaChecks(cfg(), nv(), root, inv, free(100*gib), vr(gib)))[IDUpstream]; r.Status != doctor.OK {
		t.Fatalf("%+v", r)
	}
}

func TestVRAMBytes(t *testing.T) {
	fr := &execx.FakeRunner{Responses: map[string]execx.Response{
		"nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits": {Out: []byte("8192\n8192\n")},
	}}
	if got := VRAMBytes(context.Background(), fr); got != 8192<<20 {
		t.Fatalf("got %d", got)
	}
	if got := VRAMBytes(context.Background(), &execx.FakeRunner{}); got != 0 {
		t.Fatalf("unavailable: %d", got)
	}
}
