package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

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

// hostChecks is the registration point for host checks; later tasks append
// their checks to its result.
var hostChecks = func(ctx context.Context) ([]doctor.Check, error) {
	var checks []doctor.Check
	sc, err := userSleepChecks(ctx)
	if err != nil {
		return nil, err
	}
	checks = append(checks, sc...)
	checks = append(checks, sleep.RecentEvents(sysRoot, sleep.EventsPath))
	gc, err := nvidiaChecks(ctx)
	if err != nil {
		return nil, err
	}
	return append(checks, gc...), nil
}

// nvidiaChecks returns the NVIDIA checks when the GPU is NVIDIA and managed.
func nvidiaChecks(ctx context.Context) ([]doctor.Check, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, err
	}
	n := cfg.Host.GPU.Nvidia
	if !n.Manage {
		return nil, nil
	}
	sys, err := detectSystem(ctx)
	if err != nil {
		return nil, err
	}
	if sys.GPU != "nvidia" {
		return nil, nil
	}
	inv, err := inventory.Load(sysRoot, plan.SysInventoryPath)
	if err != nil {
		return nil, err
	}
	return gpu.NvidiaChecks(n, sys, sysRoot, inv, gpu.FreeBytes, gpu.VRAMBytes(ctx, execx.OSRunner{})), nil
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
