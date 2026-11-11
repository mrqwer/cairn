package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/host/gpu"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

// ExitError asks main to terminate the process with Code.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// hostChecks builds the host checks. The config is loaded and the system
// detected once per run and shared by every check.
var hostChecks = func(ctx context.Context) ([]doctor.Check, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, err
	}
	sys, err := detectSystem(ctx)
	if err != nil {
		return nil, err
	}
	checks := []doctor.Check{policyCheck(cfg, sys)}
	checks = append(checks, userSleepChecks(cfg.Host.Sleep, sys)...)
	checks = append(checks, sleep.RecentEvents(sysRoot, sleep.EventsPath))
	if dirs, err := userDirs(); err == nil {
		checks = append(checks, sleep.FindingsCheck(sysRoot, filepath.Join(dirs.State, "host", "findings.jsonl"), time.Now))
	}
	if !cfg.Host.GPU.Nvidia.Manage || sys.GPU != "nvidia" {
		return checks, nil
	}
	inv, err := inventory.Load(sysRoot, plan.SysInventoryPath)
	if err != nil {
		return nil, err
	}
	return append(checks, nvidiaChecks(ctx, cfg.Host.GPU.Nvidia, sys, inv)...), nil
}

// IDPolicyApplied is the doctor check comparing the host plan with the system.
const IDPolicyApplied = "host.policy.applied"

const titlePolicyApplied = "Host policy applied"

// policyCheck resolves the apply plan as `cairn host plan` does (user side,
// with root's preview): OK when nothing is pending, Warn with the number of
// pending changes, Crit listing managed files changed by hand.
func policyCheck(cfg *config.Config, sys detect.System) doctor.Check {
	return check{IDPolicyApplied, titlePolicyApplied, func(ctx context.Context) doctor.Result {
		if errs := cfg.Validate(); len(errs) > 0 {
			return doctor.Result{Status: doctor.Unknown, Evidence: "configuration is invalid: " + errs[0].Error(),
				Fix: "cairn config validate"}
		}
		hs, err := resolveHost(ctx, cfg, sys, plan.ModeApply)
		if err != nil {
			return doctor.Result{Status: doctor.Unknown, Evidence: err.Error()}
		}
		if m := append(hs.user.ModifiedPaths(), hs.root.ModifiedPaths()...); len(m) > 0 {
			return doctor.Result{Status: doctor.Crit, Evidence: "changed by hand: " + strings.Join(m, ", "),
				Fix: "review the changes, then cairn host apply --force (a backup is kept)"}
		}
		n := len(hs.user.Pending()) + len(hs.root.Pending())
		if hs.stale {
			n++
		}
		if n > 0 {
			return doctor.Result{Status: doctor.Warn, Evidence: fmt.Sprintf("%d changes pending — run cairn host apply", n),
				Fix: "cairn host apply"}
		}
		return doctor.Result{Status: doctor.OK, Evidence: "nothing pending"}
	}}
}

// check adapts a function to doctor.Check.
type check struct {
	id, title string
	run       func(context.Context) doctor.Result
}

func (c check) ID() string    { return c.id }
func (c check) Title() string { return c.title }
func (c check) Run(ctx context.Context) doctor.Result {
	r := c.run(ctx)
	r.ID, r.Title = c.id, c.title
	return r
}

// Seams for the NVIDIA space check.
var (
	freeBytesOf = gpu.FreeBytes
	vramOf      = func(ctx context.Context) (uint64, error) { return gpu.VRAMBytes(ctx, execx.OSRunner{}) }
)

// nvidiaChecks returns the NVIDIA checks when the GPU is NVIDIA and managed.
// The VRAM query (nvidia-smi, which can wake the GPU) runs only inside the
// space check.
func nvidiaChecks(ctx context.Context, n config.Nvidia, sys detect.System, inv *inventory.Inventory) []doctor.Check {
	if !n.Manage || sys.GPU != "nvidia" {
		return nil
	}
	return gpu.NvidiaChecks(n, sys, sysRoot, inv, freeBytesOf, func() (uint64, error) { return vramOf(ctx) })
}

type jsonResult struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
	Fix      string `json:"fix"`
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:       "doctor [host]",
		Short:     "Run health checks",
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"host"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks, err := hostChecks(cmd.Context())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			rs := doctor.Run(ctx, checks)
			if asJSON {
				out := make([]jsonResult, 0, len(rs))
				for _, r := range rs {
					out = append(out, jsonResult{r.ID, r.Title, r.Status.String(), r.Evidence, r.Fix})
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(out); err != nil {
					return err
				}
			} else if len(rs) == 0 {
				cmd.Println("no checks registered")
			} else {
				doctor.Render(cmd.OutOrStdout(), rs)
			}
			if code := doctor.ExitCode(rs); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print results as JSON")
	return cmd
}
