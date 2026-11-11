package sleep

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/plan"
)

// Check IDs.
const (
	IDSwapPresent = "sleep.swap.present"
	IDResume      = "sleep.resume.match"
	IDSwapFree    = "sleep.swap.free"
	IDKernel      = "sleep.kernel.match"
	IDInitramfs   = "sleep.initramfs.resume"
	IDSupported   = "sleep.hibernate.supported"
)

// RecordPath is where sleep-check --record stores its results.
const RecordPath = plan.SysStateDir + "/sleep-check.json"

// imageSizeBytes returns the estimated hibernation image size in bytes:
// /sys/power/image_size when it is readable and positive, else
// MemTotal-MemAvailable. A variable so tests can pin it.
var imageSizeBytes = func() (int64, error) {
	if b, err := os.ReadFile("/sys/power/image_size"); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var total, avail int64 = -1, -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total < 0 || avail < 0 {
		return 0, errors.New("MemTotal/MemAvailable not found in /proc/meminfo")
	}
	return (total - avail) * 1024, nil
}

type fnCheck struct {
	id, title string
	run       func() doctor.Result
}

func (c fnCheck) ID() string                        { return c.id }
func (c fnCheck) Title() string                     { return c.title }
func (c fnCheck) Run(context.Context) doctor.Result { return c.run() }

// BtrfsMagic is the statfs magic of btrfs. FIEMAP on btrfs reports logical
// addresses, so a swap file there has no usable resume_offset this way.
const BtrfsMagic = 0x9123683E

// ErrBtrfsSwap reports a hibernation swap file on btrfs.
var ErrBtrfsSwap = errors.New("btrfs swap files are not supported yet")

const fixBtrfs = "use a swap partition or a swap file on ext4/xfs"

// Probes are the filesystem queries behind the resume checks; tests replace
// them.
type Probes struct {
	Offset OffsetFunc                        // resume_offset of a swap file
	DevOf  func(path string) (string, error) // MAJ:MIN of the filesystem holding a file
	RdevOf func(path string) (string, error) // MAJ:MIN of a device node
	FSType func(path string) (int64, error)  // statfs magic of the filesystem holding a file
	// Repairable reports whether cairn host repair can fix the resume
	// settings (systemd-boot with an entry for the running kernel); nil
	// means it cannot.
	Repairable func() bool
}

// DefaultProbes queries the real filesystem.
func DefaultProbes() Probes {
	return Probes{Offset: FirstPhysicalOffset, DevOf: DeviceOf, RdevOf: RdevOf, FSType: FSType}
}

// Target returns the resume device ("MAJ:MIN") and offset the kernel needs
// for swap s: a partition's own device number and offset 0, or the device of
// a swap file's filesystem and its first physical extent. A file on btrfs is
// ErrBtrfsSwap.
func (p Probes) Target(s detect.Swap) (dev string, off uint64, err error) {
	if s.Type == "partition" {
		if p.RdevOf == nil {
			return "", 0, errors.New("no device probe")
		}
		dev, err = p.RdevOf(s.Path)
		return dev, 0, err
	}
	if p.FSType != nil {
		if t, err := p.FSType(s.Path); err == nil && t == BtrfsMagic {
			return "", 0, ErrBtrfsSwap
		}
	}
	if p.DevOf == nil || p.Offset == nil {
		return "", 0, errors.New("no file probes")
	}
	if dev, err = p.DevOf(s.Path); err != nil {
		return "", 0, err
	}
	if off, err = p.Offset(s.Path); err != nil {
		return "", 0, err
	}
	return dev, off, nil
}

// SelectSwap picks the hibernation swap: the configured path, else the
// active file or partition swap with the lowest priority, ignoring zram.
// ok is false when that swap is not active.
func SelectSwap(cfg config.Sleep, sys detect.System) (swap detect.Swap, ok bool) {
	s, _, ok := selectSwap(cfg, sys)
	return s, ok
}

func selectSwap(cfg config.Sleep, sys detect.System) (detect.Swap, string, bool) {
	if p := cfg.HibernationSwap.Path; p != "" {
		for _, s := range sys.Swaps {
			if s.Path == p {
				return s, p, true
			}
		}
		return detect.Swap{}, p, false
	}
	var best *detect.Swap
	for i := range sys.Swaps {
		s := &sys.Swaps[i]
		usable := (s.Type == "file" || s.Type == "partition") && !strings.HasPrefix(s.Path, "/dev/zram")
		if usable && (best == nil || s.Priority < best.Priority) {
			best = s
		}
	}
	if best == nil {
		return detect.Swap{}, "", false
	}
	return *best, best.Path, true
}

const (
	titleSwapPresent = "Hibernation swap is active"
	titleResume      = "Resume device and offset match the hibernation swap"
	titleSwapFree    = "Swap has room for the hibernation image"
	titleKernel      = "Running kernel matches installed modules"
	titleInitramfs   = "Initramfs has the resume hook"
	titleSupported   = "Kernel allows hibernation"

	fixNoSwap     = "create and enable a swap file or partition at least as large as RAM, then set host.sleep.hibernation_swap.path"
	fixInitramfsA = `add the "resume" hook to HOOKS in /etc/mkinitcpio.conf (after "block") and run: sudo mkinitcpio -P`
	fixInitramfsG = "add resume support to the initramfs (the resume hook or module of your initramfs generator) and regenerate it"
)

// Titles maps the sleep check IDs to their titles.
var Titles = map[string]string{
	IDSwapPresent: titleSwapPresent,
	IDResume:      titleResume,
	IDSwapFree:    titleSwapFree,
	IDKernel:      titleKernel,
	IDInitramfs:   titleInitramfs,
	IDSupported:   titleSupported,
}

// Checks returns the sleep pre-flight checks.
func Checks(cfg config.Sleep, sys detect.System, probes Probes) []doctor.Check {
	swap, path, found := selectSwap(cfg, sys)
	noSwap := "no swap file or partition configured or active"
	if path != "" {
		noSwap = path + " is not an active swap"
	}
	missing := doctor.Result{Status: doctor.Crit, Evidence: noSwap, Fix: fixNoSwap}
	return []doctor.Check{
		fnCheck{IDSupported, titleSupported, func() doctor.Result {
			switch {
			case sys.HibernateSupported == detect.Yes:
				return doctor.Result{Status: doctor.OK}
			case sys.HibernateSupported == detect.No && (sys.Lockdown == "integrity" || sys.Lockdown == "confidentiality"):
				return doctor.Result{Status: doctor.Crit, Evidence: "hibernation is disabled by kernel lockdown (Secure Boot)",
					Fix: "disable Secure Boot (or kernel lockdown) in the firmware setup; until then the lid only suspends"}
			case sys.HibernateSupported == detect.No:
				return doctor.Result{Status: doctor.Crit, Evidence: "the kernel does not offer hibernation (no \"disk\" in /sys/power/state)",
					Fix: "use a kernel built with hibernation support (CONFIG_HIBERNATION)"}
			}
			return doctor.Result{Status: doctor.Unknown, Evidence: "could not read /sys/power/state"}
		}},
		fnCheck{IDSwapPresent, titleSwapPresent, func() doctor.Result {
			if !found {
				return missing
			}
			return doctor.Result{Status: doctor.OK, Evidence: path}
		}},
		fnCheck{IDResume, titleResume, func() doctor.Result {
			if !found {
				return missing
			}
			dev, off, err := probes.Target(swap)
			if errors.Is(err, ErrBtrfsSwap) {
				return doctor.Result{Status: doctor.Crit, Evidence: err.Error(), Fix: fixBtrfs}
			}
			if err != nil {
				return doctor.Result{Status: doctor.Unknown, Evidence: err.Error(), Fix: "sudo cairn host sleep-check"}
			}
			if dev != sys.Resume.Device || off != sys.Resume.Offset {
				fix := fmt.Sprintf("set resume= (the UUID of the device holding %s) and resume_offset=%d on the kernel "+
					"command line of your boot loader, then reboot; sudo cairn host repair prints the exact values", path, off)
				if probes.Repairable != nil && probes.Repairable() {
					fix = "sudo cairn host repair"
				}
				return doctor.Result{
					Status:   doctor.Crit,
					Evidence: fmt.Sprintf("expected %s/%d, kernel has %s/%d", dev, off, sys.Resume.Device, sys.Resume.Offset),
					Fix:      fix,
				}
			}
			return doctor.Result{Status: doctor.OK, Evidence: fmt.Sprintf("%s/%d", dev, off)}
		}},
		fnCheck{IDSwapFree, titleSwapFree, func() doctor.Result {
			if !found {
				return missing
			}
			img, err := imageSizeBytes()
			if err != nil {
				return doctor.Result{Status: doctor.Unknown, Evidence: err.Error()}
			}
			imgKiB := (img + 1023) / 1024
			free := swap.SizeKiB - swap.UsedKiB
			ev := fmt.Sprintf("image %d KiB, free %d KiB", imgKiB, free)
			if free < imgKiB {
				return doctor.Result{Status: doctor.Crit, Evidence: ev, Fix: "enlarge the hibernation swap or free memory"}
			}
			return doctor.Result{Status: doctor.OK, Evidence: ev}
		}},
		fnCheck{IDKernel, titleKernel, func() doctor.Result {
			if !sys.KernelModulesPresent {
				return doctor.Result{Status: doctor.Crit, Evidence: "kernel upgraded since boot — reboot before hibernating", Fix: "reboot"}
			}
			return doctor.Result{Status: doctor.OK, Evidence: sys.KernelRunning}
		}},
		fnCheck{IDInitramfs, titleInitramfs, func() doctor.Result {
			switch sys.InitramfsResume {
			case detect.Yes:
				return doctor.Result{Status: doctor.OK}
			case detect.No:
				fix := fixInitramfsG
				if sys.PkgManager == "pacman" {
					fix = fixInitramfsA
				}
				return doctor.Result{Status: doctor.Crit, Evidence: "no resume hook in the initramfs config", Fix: fix}
			default:
				return doctor.Result{Status: doctor.Unknown, Evidence: "could not determine"}
			}
		}},
	}
}

// Record is the content of RecordPath.
type Record struct {
	Time    time.Time    `json:"time"`
	Results []RecordItem `json:"results"`
}

// RecordItem is one stored doctor result.
type RecordItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
	Fix      string `json:"fix"`
}

// NewRecord converts results to their stored form.
func NewRecord(now time.Time, rs []doctor.Result) Record {
	rec := Record{Time: now.UTC(), Results: make([]RecordItem, 0, len(rs))}
	for _, r := range rs {
		rec.Results = append(rec.Results, RecordItem{r.ID, r.Title, r.Status.String(), r.Evidence, r.Fix})
	}
	return rec
}

// WriteRecord stores rec atomically (mode 0644). It writes only when the
// state directory already exists; apply creates it with the secure-chain
// checks, and root must not create it here.
func WriteRecord(root fsx.Root, rec Record) error {
	if _, err := root.Stat(plan.SysStateDir); err != nil {
		return fmt.Errorf("state dir %s: %w", plan.SysStateDir, err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return root.WriteAtomic(RecordPath, append(data, '\n'), 0o644)
}

func parseStatus(s string) doctor.Status {
	switch s {
	case "ok":
		return doctor.OK
	case "warn":
		return doctor.Warn
	case "CRIT":
		return doctor.Crit
	}
	return doctor.Unknown
}

// RecordedResume returns a resume check that reads the last stored
// sleep-check result, for callers that cannot run FIEMAP (not root).
func RecordedResume(root fsx.Root, now func() time.Time) doctor.Check {
	return fnCheck{IDResume, titleResume, func() doctor.Result {
		unknown := doctor.Result{Status: doctor.Unknown, Evidence: "no recorded sleep-check result", Fix: "sudo cairn host sleep-check"}
		data, err := root.ReadFile(RecordPath)
		if errors.Is(err, fs.ErrNotExist) {
			return unknown
		}
		if err != nil {
			unknown.Evidence = err.Error()
			return unknown
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			unknown.Evidence = "unreadable " + RecordPath + ": " + err.Error()
			return unknown
		}
		for _, it := range rec.Results {
			if it.ID != IDResume {
				continue
			}
			age := now().Sub(rec.Time).Round(time.Second)
			return doctor.Result{
				Status:   parseStatus(it.Status),
				Evidence: fmt.Sprintf("%s (recorded %s ago)", it.Evidence, age),
				Fix:      it.Fix,
			}
		}
		return unknown
	}}
}

// FailedEventData is the data of a sleep.check.failed event for rs: the
// sorted IDs of the Crit results ("failed") and their titles ("titles"), in
// the same order.
func FailedEventData(rs []doctor.Result) map[string]any {
	crit := map[string]string{}
	for _, r := range rs {
		if r.Status == doctor.Crit {
			crit[r.ID] = r.Title
		}
	}
	ids := FailedIDs(rs)
	failed, titles := make([]any, len(ids)), make([]any, len(ids))
	for i, id := range ids {
		failed[i], titles[i] = id, crit[id]
	}
	return map[string]any{"failed": failed, "titles": titles}
}

// FailedIDs returns the IDs of Crit results, sorted.
func FailedIDs(rs []doctor.Result) []string {
	var ids []string
	for _, r := range rs {
		if r.Status == doctor.Crit {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
