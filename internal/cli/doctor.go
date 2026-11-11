package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/doctor"
)

// ExitError asks main to terminate the process with Code.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// hostChecks is the registration point for host checks; later tasks replace it.
var hostChecks = func(context.Context) ([]doctor.Check, error) { return nil, nil }

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
