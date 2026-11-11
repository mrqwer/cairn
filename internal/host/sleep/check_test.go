package sleep

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/plan"
)

const gib = 1 << 30

func fixture() detect.System {
	return detect.System{
		Swaps:                []detect.Swap{{Path: "/swapfile", Type: "file", SizeKiB: 16 * 1024 * 1024, UsedKiB: 0, Priority: -2}},
		Resume:               detect.Resume{Device: "259:3", Offset: 61112320},
		KernelModulesPresent: true,
		InitramfsResume:      detect.Yes,
	}
}

func run(t *testing.T, sys detect.System, off uint64, image int64) map[string]doctor.Result {
	t.Helper()
	old := imageSizeBytes
	imageSizeBytes = func() (int64, error) { return image, nil }
	t.Cleanup(func() { imageSizeBytes = old })
	checks := Checks(config.Sleep{}, sys, testProbes(off))
	out := map[string]doctor.Result{}
	for _, r := range doctor.Run(context.Background(), checks) {
		out[r.ID] = r
	}
	return out
}

func testProbes(off uint64) Probes {
	return Probes{
		Offset: func(string) (uint64, error) { return off, nil },
		DevOf:  func(string) (string, error) { return "259:3", nil },
		RdevOf: func(p string) (string, error) {
			if p == "/dev/nvme0n1p4" {
				return "259:4", nil
			}
			return "", errors.New("not a device")
		},
		FSType: func(p string) (int64, error) {
			if strings.HasPrefix(p, "/btrfs/") {
				return BtrfsMagic, nil
			}
			return 0xEF53, nil
		},
	}
}

func TestChecksAllGreen(t *testing.T) {
	rs := run(t, fixture(), 61112320, 2*gib)
	if len(rs) != 5 {
		t.Fatalf("got %d results", len(rs))
	}
	for id, r := range rs {
		if r.Status != doctor.OK {
			t.Errorf("%s: %v %s", id, r.Status, r.Evidence)
		}
	}
}

func TestResumeOffsetMismatch(t *testing.T) {
	r := run(t, fixture(), 999, 2*gib)[IDResume]
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "61112320") || !strings.Contains(r.Evidence, "999") ||
		!strings.Contains(r.Fix, "kernel command line") {
		t.Fatalf("%+v", r)
	}
	p := testProbes(999)
	p.Repairable = func() bool { return true }
	rs := doctor.Run(context.Background(), Checks(config.Sleep{}, fixture(), p))
	if rs[1].ID != IDResume || rs[1].Fix != "sudo cairn host repair" {
		t.Fatalf("repairable: %+v", rs[1])
	}
}

func TestPartitionSwapSelectedAndMatched(t *testing.T) {
	sys := fixture()
	sys.Swaps = []detect.Swap{
		{Path: "/dev/zram0", Type: "partition", SizeKiB: 1 << 20, Priority: -100},
		{Path: "/dev/nvme0n1p4", Type: "partition", SizeKiB: 16 << 20, Priority: -3},
		{Path: "/swapfile", Type: "file", SizeKiB: 16 << 20, Priority: 10},
	}
	s, p, ok := selectSwap(config.Sleep{}, sys)
	if !ok || p != "/dev/nvme0n1p4" || s.Type != "partition" {
		t.Fatalf("%v %v %v", s, p, ok)
	}
	sys.Resume = detect.Resume{Device: "259:4", Offset: 0}
	rs := run(t, sys, 12345, gib)
	if r := rs[IDResume]; r.Status != doctor.OK || r.Evidence != "259:4/0" {
		t.Fatalf("partition match: %+v", r)
	}
	sys.Resume = detect.Resume{Device: "259:4", Offset: 77}
	if r := run(t, sys, 0, gib)[IDResume]; r.Status != doctor.Crit {
		t.Fatalf("stale offset on a partition: %+v", r)
	}
}

func TestBtrfsSwapFileUnsupported(t *testing.T) {
	sys := fixture()
	sys.Swaps = []detect.Swap{{Path: "/btrfs/swapfile", Type: "file", SizeKiB: 16 << 20}}
	r := run(t, sys, 61112320, gib)[IDResume]
	if r.Status != doctor.Crit || r.Evidence != "btrfs swap files are not supported yet" ||
		r.Fix != "use a swap partition or a swap file on ext4/xfs" {
		t.Fatalf("%+v", r)
	}
}

func TestFixStrings(t *testing.T) {
	sys := fixture()
	sys.Swaps = nil
	missing := "create and enable a swap file or partition at least as large as RAM, then set host.sleep.hibernation_swap.path"
	for _, id := range []string{IDSwapPresent, IDResume, IDSwapFree} {
		if r := run(t, sys, 1, gib)[id]; r.Fix != missing {
			t.Errorf("%s fix %q", id, r.Fix)
		}
	}
	sys = fixture()
	sys.InitramfsResume = detect.No
	sys.PkgManager = "pacman"
	if r := run(t, sys, 61112320, gib)[IDInitramfs]; r.Fix != `add the "resume" hook to HOOKS in /etc/mkinitcpio.conf (after "block") and run: sudo mkinitcpio -P` {
		t.Errorf("arch initramfs fix %q", r.Fix)
	}
	sys.PkgManager = "apt-get"
	if r := run(t, sys, 61112320, gib)[IDInitramfs]; r.Fix == "" || strings.Contains(r.Fix, "cairn host repair") {
		t.Errorf("generic initramfs fix %q", r.Fix)
	}
}

func TestKernelMismatchCrit(t *testing.T) {
	sys := fixture()
	sys.KernelModulesPresent = false
	if r := run(t, sys, 61112320, gib)[IDKernel]; r.Status != doctor.Crit {
		t.Fatalf("%+v", r)
	}
}

func TestSwapTooSmall(t *testing.T) {
	sys := fixture()
	sys.Swaps[0].SizeKiB = 4 * 1024 * 1024
	if r := run(t, sys, 61112320, 8*gib)[IDSwapFree]; r.Status != doctor.Crit {
		t.Fatalf("%+v", r)
	}
}

func TestNoSwapCrit(t *testing.T) {
	sys := fixture()
	sys.Swaps = []detect.Swap{{Path: "/dev/zram0", Type: "partition", SizeKiB: 100}}
	r := run(t, sys, 1, gib)[IDSwapPresent]
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "no swap file or partition configured or active") {
		t.Fatalf("%+v", r)
	}
}

func TestLowestPriorityFileSwapSelected(t *testing.T) {
	sys := fixture()
	sys.Swaps = append(sys.Swaps, detect.Swap{Path: "/other", Type: "file", SizeKiB: 1, Priority: -5})
	s, p, ok := selectSwap(config.Sleep{}, sys)
	if !ok || p != "/other" || s.Priority != -5 {
		t.Fatalf("%v %v %v", s, p, ok)
	}
}

func TestInitramfsTri(t *testing.T) {
	for tri, want := range map[detect.Tri]doctor.Status{detect.Yes: doctor.OK, detect.No: doctor.Crit, detect.Unknown: doctor.Unknown} {
		sys := fixture()
		sys.InitramfsResume = tri
		if r := run(t, sys, 61112320, gib)[IDInitramfs]; r.Status != want {
			t.Errorf("%v: got %v", tri, r.Status)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rs := []doctor.Result{{ID: IDResume, Title: "t", Status: doctor.Crit, Evidence: "expected 1:1/2, kernel has 3:3/4", Fix: "cairn host repair"}}
	if err := WriteRecord(root, NewRecord(now, rs)); err == nil {
		t.Fatal("must not create the state dir")
	}
	if err := root.MkdirAll(plan.SysStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(root, NewRecord(now, rs)); err != nil {
		t.Fatal(err)
	}
	r := RecordedResume(root, func() time.Time { return now.Add(90 * time.Second) }).Run(context.Background())
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "1m30s") || !strings.Contains(r.Evidence, "expected 1:1/2") {
		t.Fatalf("%+v", r)
	}
	empty := RecordedResume(fsx.Root{Dir: t.TempDir()}, time.Now).Run(context.Background())
	if empty.Status != doctor.Unknown || empty.Fix != "sudo cairn host sleep-check" {
		t.Fatalf("%+v", empty)
	}
}
