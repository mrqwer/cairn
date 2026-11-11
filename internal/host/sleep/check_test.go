package sleep

import (
	"context"
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
	checks := Checks(config.Sleep{}, sys,
		func(string) (uint64, error) { return off, nil },
		func(string) (string, error) { return "259:3", nil })
	out := map[string]doctor.Result{}
	for _, r := range doctor.Run(context.Background(), checks) {
		out[r.ID] = r
	}
	return out
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
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "61112320") || !strings.Contains(r.Evidence, "999") || r.Fix != "cairn host repair" {
		t.Fatalf("%+v", r)
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
	if r.Status != doctor.Crit || !strings.Contains(r.Evidence, "no swap file configured or active") {
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
