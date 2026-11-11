package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"context"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/plan"
)

// userSleepChecks builds the sleep checks for `cairn doctor host`. Unless
// running as root it cannot call FIEMAP, so the resume check reads the last
// sleep-check result instead.
func userSleepChecks(ctx context.Context) ([]doctor.Check, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, err
	}
	sys, err := detectSystem(ctx)
	if err != nil {
		return nil, err
	}
	checks := sleep.Checks(cfg.Host.Sleep, sys, sleep.FirstPhysicalOffset, sleep.DeviceOf)
	if os.Geteuid() != 0 {
		for i, c := range checks {
			if c.ID() == sleep.IDResume {
				checks[i] = sleep.RecordedResume(sysRoot, time.Now)
			}
		}
	}
	return checks, nil
}

// hostSleepConfig reads the root-side host snapshot; sleep-check runs from
// systemd without XDG state, so it must not use the user config loader.
func hostSleepConfig() (config.Sleep, error) {
	data, err := sysRoot.ReadFile(plan.HostConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err := config.Defaults()
		if err != nil {
			return config.Sleep{}, err
		}
		return cfg.Host.Sleep, nil
	}
	if err != nil {
		return config.Sleep{}, err
	}
	var h config.Host
	if err := json.Unmarshal(data, &h); err != nil {
		return config.Sleep{}, fmt.Errorf("parse %s: %w", plan.HostConfigPath, err)
	}
	return h.Sleep, nil
}

func newSleepCheckCmd() *cobra.Command {
	var record bool
	cmd := &cobra.Command{
		Use:   "sleep-check",
		Short: "Run the hibernation pre-flight checks (run as root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctxOf(cmd)
			sc, err := hostSleepConfig()
			if err != nil {
				return err
			}
			sys, err := detectSystem(ctx)
			if err != nil {
				return err
			}
			rs := doctor.Run(ctx, sleep.Checks(sc, sys, sleep.FirstPhysicalOffset, sleep.DeviceOf))
			doctor.Render(cmd.OutOrStdout(), rs)
			code := doctor.ExitCode(rs)
			if !record {
				if code != 0 {
					return &ExitError{Code: code}
				}
				return nil
			}
			// The state dir is created by apply with its ownership checks;
			// never create it here.
			if err := sleep.WriteRecord(sysRoot, sleep.NewRecord(time.Now(), rs)); err != nil {
				cmd.PrintErrf("warning: not recording result: %v\n", err)
			}
			if failed := sleep.FailedIDs(rs); len(failed) > 0 {
				logEvent(cmd, failed)
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&record, "record", false, "store the result and log failures (for ExecStartPre)")
	return cmd
}

func logEvent(cmd *cobra.Command, failed []string) {
	if _, err := sysRoot.Stat(plan.SysStateDir); err != nil {
		cmd.PrintErrf("warning: not logging event: %v\n", err)
		return
	}
	boot, _ := events.CurrentBoot(sysRoot)
	ids := make([]any, len(failed))
	for i, f := range failed {
		ids[i] = f
	}
	l := events.Log{Root: sysRoot, Path: plan.SysStateDir + "/events.jsonl"}
	err := l.Append(events.Event{
		TS: time.Now().UTC(), Boot: boot, Source: "sleep-check",
		Kind: events.KindSleepCheckFailed, Data: map[string]any{"failed": ids},
	})
	if err != nil {
		cmd.PrintErrf("warning: not logging event: %v\n", err)
	}
}
