// Package gpu manages GPU vendor settings that matter for suspend and
// hibernate (currently NVIDIA video memory preservation).
package gpu

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
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
// at a path; vramBytes is the total GPU memory (0 when unknown).
func NvidiaChecks(cfg config.Nvidia, sys detect.System, root fsx.Root, inv *inventory.Inventory,
	freeBytes func(path string) (uint64, error), vramBytes uint64) []doctor.Check {
	return []doctor.Check{
		fnCheck{IDParams, "NVIDIA temporary file path in effect", func() doctor.Result {
			return paramsResult(cfg, sys, root)
		}},
		fnCheck{IDSpace, "NVIDIA suspend space", func() doctor.Result {
			return spaceResult(cfg, freeBytes, vramBytes)
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
	ev := fmt.Sprintf("%s: %d MiB free, %d MiB needed", cfg.TemporaryFilePath, free>>20, need>>20)
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

// VRAMBytes returns the total memory of the first GPU via nvidia-smi, or 0
// when it cannot be determined.
func VRAMBytes(ctx context.Context, r execx.Runner) uint64 {
	out, err := r.Run(ctx, "nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	mib, err := strconv.ParseUint(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0
	}
	return mib << 20
}
