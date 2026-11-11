package repair

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

const (
	hibD     = "/etc/systemd/system/systemd-hibernate.service.d"
	entry    = "/boot/loader/entries/arch.conf"
	fallback = "/boot/loader/entries/arch-fallback.conf"
	swapPath = "/hibernate-swap/swapfile"
	kernel   = "6.1.0-test"
)

func put(t *testing.T, root fsx.Root, p, content string) {
	t.Helper()
	if err := root.WriteAtomic(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a systemd-boot system whose hibernation swap lives on 259:2.
func fixture(t *testing.T) (fsx.Root, detect.System, *execx.FakeRunner) {
	t.Helper()
	root := fsx.Root{Dir: t.TempDir()}
	put(t, root, "/sys/dev/block/259:2/uevent", "MAJOR=259\nMINOR=2\nDEVNAME=nvme0n1p2\nDEVTYPE=partition\n")
	put(t, root, "/usr/lib/modules/"+kernel+"/pkgbase", "linux\n")
	put(t, root, "/boot/loader/loader.conf", "default arch.conf\ntimeout 3\n")
	put(t, root, "/boot/initramfs-linux.img", "img")
	fr := &execx.FakeRunner{Responses: map[string]execx.Response{
		"blkid -s UUID -o value /dev/nvme0n1p2": {Out: []byte("x\n")},
	}}
	oldRun := runner
	t.Cleanup(func() { runner, testDev = oldRun, "259:2" })
	runner = fr
	sys := detect.System{
		BootLoader:    "systemd-boot",
		KernelRunning: kernel,
		Swaps: []detect.Swap{
			{Path: "/swap/other", Type: "file", Priority: 20},
			{Path: swapPath, Type: "file", Priority: -10},
		},
	}
	return root, sys, fr
}

// testDev is the device the test probes report for the swap file.
var testDev = "259:2"

func offsetFn(v uint64) sleep.Probes {
	return sleep.Probes{
		Offset: func(string) (uint64, error) { return v, nil },
		DevOf: func(p string) (string, error) {
			if p != swapPath {
				return "", errors.New("unexpected path " + p)
			}
			return testDev, nil
		},
		RdevOf: func(p string) (string, error) {
			if p == "/dev/nvme0n1p3" {
				return "259:3", nil
			}
			return "", errors.New("not a device")
		},
		FSType: func(p string) (int64, error) {
			if strings.HasPrefix(p, "/btrfs/") {
				return sleep.BtrfsMagic, nil
			}
			return 0xEF53, nil
		},
	}
}

// apply resolves and applies p as root against root, like apply-root does.
func apply(t *testing.T, root fsx.Root, p plan.Plan) *inventory.Inventory {
	t.Helper()
	inv := &inventory.Inventory{}
	env := plan.Env{
		Root: root, Runner: &execx.FakeRunner{Responses: map[string]execx.Response{"systemctl daemon-reload": {}}},
		SysInv: inv, SysInvPath: plan.SysInventoryPath,
		OwnerOf: func(fs.FileInfo) (uint32, bool) { return 0, true },
	}
	for _, a := range p.Actions {
		if err := plan.CheckRootAction(a, nil, plan.ModeRepair); err != nil {
			t.Fatalf("repair action rejected by the allowlist: %v", err)
		}
	}
	r := plan.Resolve(p.Actions, root, inv)
	r.Mode = plan.ModeRepair
	if err := plan.Apply(context.Background(), r, env, plan.Options{RootOnly: true}); err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestRepairRemovesConflictingFallback(t *testing.T) {
	root, sys, _ := fixture(t)
	sys.BootLoader = "grub"
	conflict := hibD + "/fallback.conf"
	ours := hibD + "/50-cairn.conf"
	harmless := hibD + "/10-timeout.conf"
	put(t, root, conflict, "[Unit]\nOnFailure=sleep-fallback.service\n")
	put(t, root, ours, "[Unit]\nOnFailure=cairn-sleep-fallback.service\n")
	put(t, root, harmless, "[Service]\nTimeoutSec=60\n")
	put(t, root, "/etc/systemd/system/sleep-fallback.service", "[Service]\nExecStart=/usr/bin/systemctl suspend\n")

	p := Plan(root, sys, config.Sleep{}, offsetFn(1))
	apply(t, root, p)

	for _, gone := range []string{conflict, "/etc/systemd/system/sleep-fallback.service"} {
		if root.Exists(gone) {
			t.Errorf("%s not removed", gone)
		}
		if !root.Exists(plan.BackupDir + "/" + strings.ReplaceAll(gone, "/", "%")) {
			t.Errorf("%s not backed up", gone)
		}
	}
	if got, _ := root.ReadFile(ours); string(got) != "[Unit]\nOnFailure=cairn-sleep-fallback.service\n" {
		t.Fatalf("50-cairn.conf touched: %q", got)
	}
	if !root.Exists(harmless) {
		t.Fatal("drop-in without OnFailure= removed")
	}
}

func TestRepairUpdatesResumeOffset(t *testing.T) {
	root, sys, _ := fixture(t)
	orig := "title Arch Linux\nlinux /vmlinuz-linux\ninitrd /initramfs-linux.img\noptions root=PARTUUID=p rw quiet resume=UUID=x resume_offset=1\n"
	put(t, root, entry, orig)
	put(t, root, "/boot/loader/entries/other.conf", "title Other\nlinux /vmlinuz-linux-lts\noptions rw resume_offset=5\n")

	p := Plan(root, sys, config.Sleep{}, offsetFn(61112320))
	if len(p.Actions) != 1 || p.Actions[0].Path != entry {
		t.Fatalf("actions %+v", p.Actions)
	}
	inv := apply(t, root, p)

	got, _ := root.ReadFile(entry)
	want := strings.Replace(orig, "resume_offset=1", "resume_offset=61112320", 1)
	if string(got) != want {
		t.Fatalf("entry:\n%s\nwant:\n%s", got, want)
	}
	if b, _ := root.ReadFile(plan.BackupDir + "/%boot%loader%entries%arch.conf"); string(b) != orig {
		t.Fatalf("backup %q", b)
	}
	if e, ok := inv.Get(entry); !ok || e.Class != inventory.Repaired {
		t.Fatalf("entry record %+v", e)
	}
	if b, _ := root.ReadFile("/boot/loader/entries/other.conf"); !strings.Contains(string(b), "resume_offset=5") {
		t.Fatal("entry for another kernel changed")
	}
	// Second run: nothing to change.
	if p := Plan(root, sys, config.Sleep{}, offsetFn(61112320)); len(p.Actions) != 0 {
		t.Fatalf("second plan %+v", p.Actions)
	}
}

func TestRepairAddsMissingResumeOptions(t *testing.T) {
	root, sys, _ := fixture(t)
	put(t, root, entry, "linux /vmlinuz-linux\noptions rw\n")
	p := Plan(root, sys, config.Sleep{HibernationSwap: config.Swap{Path: swapPath}}, offsetFn(42))
	if len(p.Actions) != 1 {
		t.Fatalf("actions %+v", p.Actions)
	}
	if got := string(p.Actions[0].Content); got != "linux /vmlinuz-linux\noptions rw resume=UUID=x resume_offset=42\n" {
		t.Fatalf("content %q", got)
	}
}

func TestRepairReportsWithoutActions(t *testing.T) {
	root, sys, _ := fixture(t)
	put(t, root, "/boot/loader/loader.conf", "timeout 3\n")
	put(t, root, fallback, "linux /vmlinuz-linux\ninitrd /initramfs-linux-fallback.img\noptions rw resume=UUID=x resume_offset=7\n")
	p := Plan(root, sys, config.Sleep{}, offsetFn(7))
	if len(p.Actions) != 0 {
		t.Fatalf("report-only items became actions: %+v", p.Actions)
	}
	notes := strings.Join(p.Notes, "\n")
	if !strings.Contains(notes, "loader.conf") || !strings.Contains(notes, "/initramfs-linux-fallback.img") {
		t.Fatalf("notes %q", notes)
	}
	if got := strings.Join(Report(root, sys), "\n"); got != notes {
		t.Fatalf("Report %q differs from plan notes %q", got, notes)
	}
}

func TestRepairNoDeviceIsReported(t *testing.T) {
	root, sys, _ := fixture(t)
	put(t, root, entry, "linux /vmlinuz-linux\noptions rw resume_offset=1\n")
	testDev = "0:45" // no block node
	p := Plan(root, sys, config.Sleep{}, offsetFn(9))
	if len(p.Actions) != 0 || !strings.Contains(strings.Join(p.Notes, "\n"), "0:45") {
		t.Fatalf("actions %+v notes %q", p.Actions, p.Notes)
	}
}

func TestRepairOtherBootLoaderNote(t *testing.T) {
	root, sys, _ := fixture(t)
	sys.BootLoader = "grub"
	p := Plan(root, sys, config.Sleep{}, offsetFn(61112320))
	want := "boot loader grub: set resume=UUID=x resume_offset=61112320 on the kernel command line manually"
	if len(p.Actions) != 0 || !strings.Contains(strings.Join(p.Notes, "\n"), want) {
		t.Fatalf("actions %+v notes %q", p.Actions, p.Notes)
	}
}

func TestRepairRefusesAmbiguousEntries(t *testing.T) {
	for name, content := range map[string]string{
		"two linux lines":  "linux /vmlinuz-linux\nlinux /vmlinuz-linux\noptions rw resume_offset=1\n",
		"duplicate offset": "linux /vmlinuz-linux\noptions rw resume_offset=1 resume_offset=2\n",
		"duplicate resume": "linux /vmlinuz-linux\noptions rw resume=UUID=x\noptions resume=UUID=y resume_offset=1\n",
	} {
		root, sys, _ := fixture(t)
		put(t, root, entry, content)
		p := Plan(root, sys, config.Sleep{}, offsetFn(9))
		if len(p.Actions) != 0 || !strings.Contains(strings.Join(p.Notes, "\n"), "arch.conf") {
			t.Errorf("%s: actions %+v notes %q", name, p.Actions, p.Notes)
		}
	}
}

func TestRepairLeavesCorrectEntryAlone(t *testing.T) {
	root, sys, _ := fixture(t)
	put(t, root, entry, "linux /vmlinuz-linux\noptions  rw   resume=UUID=x\tresume_offset=7  \n")
	if p := Plan(root, sys, config.Sleep{}, offsetFn(7)); len(p.Actions) != 0 {
		t.Fatalf("rewrote an entry that already has the right values: %+v", p.Actions)
	}
}

func TestRepairPartitionSwap(t *testing.T) {
	root, sys, fr := fixture(t)
	put(t, root, "/sys/dev/block/259:3/uevent", "MAJOR=259\nMINOR=3\nDEVNAME=nvme0n1p3\nDEVTYPE=partition\n")
	fr.Responses["blkid -s UUID -o value /dev/nvme0n1p3"] = execx.Response{Out: []byte("part\n")}
	sys.Swaps = []detect.Swap{{Path: "/dev/zram0", Type: "partition", Priority: -100}, {Path: "/dev/nvme0n1p3", Type: "partition", Priority: -2}}
	put(t, root, entry, "linux /vmlinuz-linux\noptions rw resume_offset=61112320\n")
	p := Plan(root, sys, config.Sleep{}, offsetFn(5))
	if len(p.Actions) != 1 || string(p.Actions[0].Content) != "linux /vmlinuz-linux\noptions rw resume_offset=0 resume=UUID=part\n" {
		t.Fatalf("actions %+v notes %q", p.Actions, p.Notes)
	}
}

func TestRepairBtrfsSwapNote(t *testing.T) {
	root, sys, _ := fixture(t)
	sys.Swaps = []detect.Swap{{Path: "/btrfs/swapfile", Type: "file"}}
	put(t, root, entry, "linux /vmlinuz-linux\noptions rw\n")
	p := Plan(root, sys, config.Sleep{}, offsetFn(5))
	notes := strings.Join(p.Notes, "\n")
	if len(p.Actions) != 0 || !strings.Contains(notes, "btrfs swap files are not supported yet") ||
		!strings.Contains(notes, "use a swap partition or a swap file on ext4/xfs") {
		t.Fatalf("actions %+v notes %q", p.Actions, notes)
	}
}

func TestRepairNoEntryForKernelNote(t *testing.T) {
	root, sys, _ := fixture(t)
	put(t, root, "/boot/loader/entries/other.conf", "linux /vmlinuz-linux-lts\noptions rw\n")
	p := Plan(root, sys, config.Sleep{}, offsetFn(42))
	want := "no boot entry for kernel " + kernel + ": set resume=UUID=x resume_offset=42 in /etc/kernel/cmdline and rebuild the UKI"
	if len(p.Actions) != 0 || !strings.Contains(strings.Join(p.Notes, "\n"), want) {
		t.Fatalf("actions %+v notes %q", p.Actions, p.Notes)
	}
	if HasBootEntry(root, sys) {
		t.Fatal("HasBootEntry without a matching entry")
	}
	put(t, root, entry, "linux /vmlinuz-linux\noptions rw\n")
	if !HasBootEntry(root, sys) {
		t.Fatal("HasBootEntry with a matching entry")
	}
}

func TestRepairRemovesOnlyUnitsNamedByConflicts(t *testing.T) {
	root, sys, _ := fixture(t)
	sys.BootLoader = "grub"
	put(t, root, hibD+"/hook.conf", "[Unit]\nOnFailure=my-fallback.service cairn-sleep-fallback.service\n")
	for _, u := range []string{"my-fallback.service", "sleep-fallback.service", "cairn-sleep-fallback.service"} {
		put(t, root, "/etc/systemd/system/"+u, "[Service]\n")
	}
	p := Plan(root, sys, config.Sleep{}, offsetFn(1))
	var removed []string
	for _, a := range p.Actions {
		if a.Kind == plan.RemoveFile {
			removed = append(removed, a.Path)
		}
	}
	if strings.Join(removed, " ") != hibD+"/hook.conf /etc/systemd/system/my-fallback.service" {
		t.Fatalf("removed %q", removed)
	}
}
