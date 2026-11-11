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

// SelectSwap picks the hibernation swap: the configured path, else the
// active file swap with the lowest priority. ok is false when that swap is
// not active.
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
		if s.Type == "file" && (best == nil || s.Priority < best.Priority) {
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
	titleResume      = "Resume device and offset match the swap file"
	titleSwapFree    = "Swap has room for the hibernation image"
	titleKernel      = "Running kernel matches installed modules"
	titleInitramfs   = "Initramfs has the resume hook"
)

// Checks returns the sleep pre-flight checks.
func Checks(cfg config.Sleep, sys detect.System, offset OffsetFunc, devOf func(path string) (string, error)) []doctor.Check {
	swap, path, found := selectSwap(cfg, sys)
	noSwap := "no swap file configured or active"
	if path != "" {
		noSwap = path + " is not an active swap"
	}
	return []doctor.Check{
		fnCheck{IDSwapPresent, titleSwapPresent, func() doctor.Result {
			if !found {
				return doctor.Result{Status: doctor.Crit, Evidence: noSwap, Fix: "cairn host repair"}
			}
			return doctor.Result{Status: doctor.OK, Evidence: path}
		}},
		fnCheck{IDResume, titleResume, func() doctor.Result {
			if !found {
				return doctor.Result{Status: doctor.Crit, Evidence: noSwap, Fix: "cairn host repair"}
			}
			dev, err := devOf(path)
			if err != nil {
				return doctor.Result{Status: doctor.Unknown, Evidence: err.Error(), Fix: "sudo cairn host sleep-check"}
			}
			off, err := offset(path)
			if err != nil {
				return doctor.Result{Status: doctor.Unknown, Evidence: err.Error(), Fix: "sudo cairn host sleep-check"}
			}
			if dev != sys.Resume.Device || off != sys.Resume.Offset {
				return doctor.Result{
					Status:   doctor.Crit,
					Evidence: fmt.Sprintf("expected %s/%d, kernel has %s/%d", dev, off, sys.Resume.Device, sys.Resume.Offset),
					Fix:      "cairn host repair",
				}
			}
			return doctor.Result{Status: doctor.OK, Evidence: fmt.Sprintf("%s/%d", dev, off)}
		}},
		fnCheck{IDSwapFree, titleSwapFree, func() doctor.Result {
			if !found {
				return doctor.Result{Status: doctor.Crit, Evidence: noSwap, Fix: "cairn host repair"}
			}
			img, err := imageSizeBytes()
			if err != nil {
				return doctor.Result{Status: doctor.Unknown, Evidence: err.Error()}
			}
			imgKiB := (img + 1023) / 1024
			free := swap.SizeKiB - swap.UsedKiB
			ev := fmt.Sprintf("image %d KiB, free %d KiB", imgKiB, free)
			if free < imgKiB {
				return doctor.Result{Status: doctor.Crit, Evidence: ev, Fix: "enlarge the swap file or free memory"}
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
				return doctor.Result{Status: doctor.Crit, Evidence: "no resume hook in the initramfs config", Fix: "cairn host repair"}
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
