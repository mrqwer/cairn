// Package gpu manages GPU vendor settings that matter for suspend and
// hibernate (currently NVIDIA video memory preservation).
package gpu

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/ui"
)

// Policy is the policy name recorded for NVIDIA objects.
const Policy = "gpu-nvidia"

// Paths and check IDs.
const (
	// VendorUpstream is the vendor-shipped modprobe file; VendorMask shadows
	// it (same file name in /etc wins).
	VendorUpstream = "/usr/lib/modprobe.d/nvidia-utils.conf"
	VendorMask     = "/etc/modprobe.d/nvidia-utils.conf"

	IDParams   = "gpu.nvidia.params"
	IDSpace    = "gpu.nvidia.space"
	IDUpstream = "gpu.nvidia.upstream"

	defaultTempPath = "/var/tmp"
	tempToken       = "NVreg_TemporaryFilePath="
	preserveToken   = "NVreg_PreserveVideoMemoryAllocations=1"
	preservePrefix  = "NVreg_PreserveVideoMemoryAllocations="
)

var optionsNvidia = regexp.MustCompile(`^\s*options\s+nvidia\s`)

// NvidiaActions returns the actions that mask the vendor modprobe file with a
// copy that keeps video memory across suspend and points the temporary file
// path at cfg.TemporaryFilePath. It is empty unless the GPU is NVIDIA and
// management is enabled.
func NvidiaActions(cfg config.Nvidia, sys detect.System, vendor []byte) []plan.Action {
	if sys.GPU != "nvidia" || !cfg.Manage {
		return nil
	}
	acts := []plan.Action{{
		Kind: plan.WriteFile, Path: VendorMask, Content: []byte(maskContent(vendor, cfg.TemporaryFilePath)),
		Mode: 0o644, NeedsRoot: true, Class: inventory.Mask, Policy: Policy, Upstream: VendorUpstream,
		Reason: "keep NVIDIA video memory across suspend",
	}}
	if path.Clean(cfg.TemporaryFilePath) != defaultTempPath {
		acts = append(acts, plan.Action{
			Kind: plan.MakeDir, Path: cfg.TemporaryFilePath, Mode: 0o700, NeedsRoot: true,
			Class: inventory.ManagedDir, Policy: Policy, Reason: "NVIDIA video memory dump directory",
		})
	}
	return acts
}

func maskContent(vendor []byte, tmp string) string {
	var b strings.Builder
	b.WriteString(fsx.Header(Policy))
	found := false
	lines := strings.SplitAfter(string(vendor), "\n")
	for _, l := range lines {
		if optionsNvidia.MatchString(l) {
			found = true
			l = rewriteOptions(l, tmp)
		}
		b.WriteString(l)
	}
	if !found {
		if s := b.String(); !strings.HasSuffix(s, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("options nvidia " + preserveToken + " " + tempToken + tmp + "\n")
	}
	return b.String()
}

// rewriteOptions sets the temporary path and the preserve flag on one
// "options nvidia" line, keeping its indentation and other tokens.
func rewriteOptions(line, tmp string) string {
	body := strings.TrimRight(line, "\r\n")
	end := line[len(body):]
	indent := body[:len(body)-len(strings.TrimLeft(body, " \t"))]
	var toks []string
	hasTmp, hasPreserve := false, false
	for _, t := range strings.Fields(body) {
		switch {
		case strings.HasPrefix(t, tempToken):
			t, hasTmp = tempToken+tmp, true
		case strings.HasPrefix(t, preservePrefix):
			t, hasPreserve = preserveToken, true
		}
		toks = append(toks, t)
	}
	if !hasPreserve {
		toks = append(toks, preserveToken)
	}
	if !hasTmp {
		toks = append(toks, tempToken+tmp)
	}
	return indent + strings.Join(toks, " ") + end
}

type fnCheck struct {
	id, title string
	run       func() doctor.Result
}

func (c fnCheck) ID() string                        { return c.id }
func (c fnCheck) Title() string                     { return c.title }
func (c fnCheck) Run(context.Context) doctor.Result { return c.run() }

// NvidiaChecks returns the NVIDIA doctor checks. freeBytes reports free space
// at a path; vram returns the total GPU memory (0 when unknown) or an error
// such as a timeout. vram is called only when the space check runs, since
// querying it can wake the GPU.
func NvidiaChecks(cfg config.Nvidia, sys detect.System, root fsx.Root, inv *inventory.Inventory,
	freeBytes func(path string) (uint64, error), vram func() (uint64, error)) []doctor.Check {
	return []doctor.Check{
		fnCheck{IDParams, "NVIDIA temporary file path in effect", func() doctor.Result {
			return paramsResult(cfg, sys, root)
		}},
		fnCheck{IDSpace, "NVIDIA suspend space", func() doctor.Result {
			v, err := vram()
			if err != nil {
				return doctor.Result{Status: doctor.Unknown, Evidence: err.Error()}
			}
			return spaceResult(cfg, freeBytes, v)
		}},
		fnCheck{IDUpstream, "NVIDIA vendor modprobe file unchanged", func() doctor.Result {
			return upstreamResult(root, inv)
		}},
	}
}

func paramsResult(cfg config.Nvidia, sys detect.System, root fsx.Root) doctor.Result {
	cur, ok := sys.NvidiaParams["TemporaryFilePath"]
	if !ok {
		return doctor.Result{Status: doctor.Unknown, Evidence: "driver parameters not readable"}
	}
	cur = strings.Trim(cur, `"`)
	if path.Clean(cur) == path.Clean(cfg.TemporaryFilePath) {
		return doctor.Result{Status: doctor.OK, Evidence: cur}
	}
	r := doctor.Result{Status: doctor.Warn, Evidence: fmt.Sprintf("configured %s, driver uses %s", cfg.TemporaryFilePath, cur)}
	if root.Exists(VendorMask) {
		r.Fix = "reboot to load the new NVIDIA options"
	} else {
		r.Fix = "cairn host apply"
	}
	return r
}

func spaceResult(cfg config.Nvidia, freeBytes func(string) (uint64, error), vram uint64) doctor.Result {
	if vram == 0 {
		return doctor.Result{Status: doctor.Unknown, Evidence: "GPU memory size unavailable"}
	}
	free, err := freeBytes(cfg.TemporaryFilePath)
	if err != nil {
		return doctor.Result{Status: doctor.Unknown, Evidence: err.Error()}
	}
	need := vram + vram/20
	ev := fmt.Sprintf("%s: %s free, %s needed", cfg.TemporaryFilePath, ui.Bytes(free), ui.Bytes(need))
	if free >= need {
		return doctor.Result{Status: doctor.OK, Evidence: ev}
	}
	return doctor.Result{Status: doctor.Crit, Evidence: ev,
		Fix: "free space or set host.gpu.nvidia.temporary_file_path to a larger filesystem"}
}

func upstreamResult(root fsx.Root, inv *inventory.Inventory) doctor.Result {
	e, ok := inv.Get(VendorMask)
	if !ok || e.UpstreamSHA256 == "" {
		return doctor.Result{Status: doctor.OK, Evidence: "nothing managed yet"}
	}
	b, err := root.ReadFile(VendorUpstream)
	if err != nil {
		return doctor.Result{Status: doctor.Unknown, Evidence: err.Error()}
	}
	if fsx.SHA256(b) != e.UpstreamSHA256 {
		return doctor.Result{Status: doctor.Warn, Evidence: "vendor file changed: run cairn host apply", Fix: "cairn host apply"}
	}
	return doctor.Result{Status: doctor.OK, Evidence: "unchanged"}
}

// FreeBytes returns the bytes available to unprivileged writers on the
// filesystem holding p, or its nearest existing parent.
func FreeBytes(p string) (uint64, error) {
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(p, &st)
		if err == nil {
			return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // block size is positive
		}
		if p == "/" || p == "." || err != syscall.ENOENT {
			return 0, err
		}
		p = path.Dir(p)
	}
}

// vramTimeout bounds nvidia-smi: sleep-check runs it on every hibernate entry.
var vramTimeout = 5 * time.Second

// VRAMBytes returns the total memory of the first GPU via nvidia-smi, or 0
// when it cannot be determined. A query that does not finish within
// vramTimeout is an error ("nvidia-smi timed out").
func VRAMBytes(ctx context.Context, r execx.Runner) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, vramTimeout)
	defer cancel()
	out, err := r.Run(ctx, "nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits")
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 0, errors.New("nvidia-smi timed out")
	}
	if err != nil {
		return 0, nil
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	mib, err := strconv.ParseUint(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0, nil
	}
	return mib << 20, nil
}

// IDUnits is the doctor check over the vendor sleep units.
const IDUnits = "gpu.nvidia.units"

// VendorUnitDir is where the driver package installs its units.
const VendorUnitDir = "/usr/lib/systemd/system"

// VendorUnits are the NVIDIA sleep units the driver package ships; they save
// and restore video memory around suspend and hibernate.
var VendorUnits = []string{
	"nvidia-suspend.service",
	"nvidia-hibernate.service",
	"nvidia-resume.service",
	"nvidia-suspend-then-hibernate.service",
}

// PresentUnits returns the VendorUnits whose unit file exists under root.
func PresentUnits(root fsx.Root) []string {
	var out []string
	for _, u := range VendorUnits {
		if root.Exists(VendorUnitDir + "/" + u) {
			out = append(out, u)
		}
	}
	return out
}

// VendorUnitActions enables the present vendor sleep units when the GPU is
// NVIDIA and management is enabled.
func VendorUnitActions(cfg config.Nvidia, sys detect.System, present []string) []plan.Action {
	if sys.GPU != "nvidia" || !cfg.Manage {
		return nil
	}
	var out []plan.Action
	for _, u := range present {
		out = append(out, plan.Action{
			Kind: plan.EnableUnit, Unit: u, NeedsRoot: true, Class: inventory.UnitEnable, Policy: Policy,
			Reason: "NVIDIA video memory save/restore around sleep",
		})
	}
	return out
}

// enabledStates are `systemctl is-enabled` answers under which a unit runs
// when its target is reached.
var enabledStates = map[string]bool{
	"enabled": true, "enabled-runtime": true, "static": true, "alias": true,
	"indirect": true, "generated": true, "linked": true, "linked-runtime": true,
}

// UnitsCheck warns when a present vendor sleep unit is not enabled.
// isEnabled returns the `systemctl is-enabled` answer for a unit.
func UnitsCheck(root fsx.Root, isEnabled func(unit string) string) doctor.Check {
	return fnCheck{IDUnits, "NVIDIA sleep units enabled", func() doctor.Result {
		present := PresentUnits(root)
		if len(present) == 0 {
			return doctor.Result{ID: IDUnits, Status: doctor.OK, Evidence: "no vendor sleep units installed"}
		}
		var off []string
		for _, u := range present {
			if st := isEnabled(u); !enabledStates[st] {
				off = append(off, u+" ("+st+")")
			}
		}
		if len(off) > 0 {
			return doctor.Result{ID: IDUnits, Status: doctor.Warn, Evidence: "not enabled: " + strings.Join(off, ", "), Fix: "cairn host apply"}
		}
		return doctor.Result{ID: IDUnits, Status: doctor.OK, Evidence: strings.Join(present, ", ")}
	}}
}
