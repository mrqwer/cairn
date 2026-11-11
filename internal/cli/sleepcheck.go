package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

// userSleepChecks builds the sleep checks for `cairn doctor host`. Unless
// running as root it cannot call FIEMAP, so the resume check reads the last
// sleep-check result instead.
func userSleepChecks(sc config.Sleep, sys detect.System) []doctor.Check {
	checks := sleep.Checks(sc, sys, offsetOf, deviceOf)
	if !isRoot() {
		for i, c := range checks {
			if c.ID() == sleep.IDResume {
				checks[i] = sleep.RecordedResume(sysRoot, time.Now)
			}
		}
	}
	return checks
}

// Seams for tests.
var (
	isRoot       = func() bool { return os.Geteuid() == 0 }
	stateOwnerOf func(fs.FileInfo) (uint32, bool) // nil: the real owner
	offsetOf     sleep.OffsetFunc                 = sleep.FirstPhysicalOffset
	deviceOf                                      = sleep.DeviceOf
)

const defaultsNote = "no " + plan.HostConfigPath + ", using built-in defaults"

// hostSnapshot reads the root-side host snapshot; sleep-check runs from
// systemd without XDG state, so it must not use the user config loader.
// usedDefaults reports that the snapshot was missing.
func hostSnapshot() (h config.Host, usedDefaults bool, err error) {
	data, err := sysRoot.ReadFile(plan.HostConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err := config.Defaults()
		if err != nil {
			return config.Host{}, false, err
		}
		return cfg.Host, true, nil
	}
	if err != nil {
		return config.Host{}, false, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return config.Host{}, false, fmt.Errorf("parse %s: %w", plan.HostConfigPath, err)
	}
	return h, false, nil
}

// failClosed turns Unknown into Crit for the checks that guard the resume
// offset, so a root --record run never lets hibernate proceed unverified.
func failClosed(rs []doctor.Result) {
	for i, r := range rs {
		if r.Status == doctor.Unknown && (r.ID == sleep.IDResume || r.ID == sleep.IDSwapFree) {
			rs[i].Status = doctor.Crit
			rs[i].Evidence = "could not verify: " + r.Evidence
		}
	}
}

func newSleepCheckCmd() *cobra.Command {
	var record bool
	cmd := &cobra.Command{
		Use:   "sleep-check",
		Short: "Run the hibernation pre-flight checks (run as root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSleepCheck(cmd, record)
		},
	}
	cmd.Flags().BoolVar(&record, "record", false, "store the result and log failures (for ExecStartPre)")
	return cmd
}

// errTimedOut is returned by bounded when fn did not finish in time.
var errTimedOut = errors.New("timed out")

// bounded runs fn with a context that expires after d and returns
// errTimedOut when fn has not returned by then (fn keeps running in the
// background; the callers are short-lived processes).
func bounded[T any](ctx context.Context, d time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn(ctx)
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, errTimedOut
	}
}

// sleepRun is one root-side run of the sleep checks.
type sleepRun struct {
	results      []doctor.Result
	usedDefaults bool
}

// rootSleepResults runs the sleep and NVIDIA checks the way the root side
// sees the system: host config from the snapshot (built-in defaults when it
// is missing or the state dir is not trusted) and its own detection.
func rootSleepResults(ctx context.Context, trusted bool) (sleepRun, error) {
	var run sleepRun
	var h config.Host
	var err error
	inv := &inventory.Inventory{}
	if trusted {
		h, run.usedDefaults, err = hostSnapshot()
	} else {
		var cfg *config.Config
		if cfg, err = config.Defaults(); err == nil {
			h, run.usedDefaults = cfg.Host, true
		}
	}
	if err != nil {
		return run, err
	}
	sys, err := detectSystem(ctx)
	if err != nil {
		return run, err
	}
	// The inventory only feeds the NVIDIA checks; a damaged one must not
	// block hibernation on systems where they do not run.
	if trusted && h.GPU.Nvidia.Manage && sys.GPU == "nvidia" {
		if inv, err = inventory.Load(sysRoot, plan.SysInventoryPath); err != nil {
			return run, err
		}
	}
	// NVIDIA checks too: a dump target without room for the VRAM must block
	// hibernation at entry.
	checks := append(sleep.Checks(h.Sleep, sys, offsetOf, deviceOf), nvidiaChecks(ctx, h.GPU.Nvidia, sys, inv)...)
	run.results = doctor.Run(ctx, checks)
	if run.usedDefaults {
		for i := range run.results {
			if run.results[i].ID == sleep.IDSwapPresent {
				run.results[i].Evidence += " (" + defaultsNote + ")"
			}
		}
	}
	return run, nil
}

func runSleepCheck(cmd *cobra.Command, record bool) error {
	ctx := ctxOf(cmd)
	trusted := true
	if err := plan.CheckTrustedDirs(sysRoot, stateOwnerOf, "/var/lib/cairn", plan.SysStateDir); err != nil {
		if record {
			cmd.PrintErrf("state directory not trusted: %v\n", err)
			return &ExitError{Code: 1}
		}
		trusted = false
		cmd.PrintErrf("note: state directory not trusted or missing (%v); not reading or writing it\n", err)
	}
	fail := func(err error) error {
		if record {
			logEvent(cmd, map[string]any{"error": err.Error()})
		}
		return err
	}
	run, err := rootSleepResults(ctx, trusted)
	if err != nil {
		return fail(err)
	}
	if run.usedDefaults {
		cmd.PrintErrf("note: %s\n", defaultsNote)
	}
	rs := run.results
	if record && isRoot() {
		failClosed(rs)
	}
	doctor.Render(cmd.OutOrStdout(), rs)
	if !record {
		// As root the result is stored for doctor (which cannot run FIEMAP
		// as a user); no event is logged and the exit code is unchanged.
		if trusted && isRoot() {
			if err := sleep.WriteRecord(sysRoot, sleep.NewRecord(time.Now(), rs)); err != nil {
				cmd.PrintErrf("warning: not recording result: %v\n", err)
			}
		}
		if code := doctor.ExitCode(rs); code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	if err := sleep.WriteRecord(sysRoot, sleep.NewRecord(time.Now(), rs)); err != nil {
		cmd.PrintErrf("warning: not recording result: %v\n", err)
	}
	if failed := sleep.FailedIDs(rs); len(failed) > 0 {
		logEvent(cmd, map[string]any{"failed": anyList(failed)})
		return &ExitError{Code: 1}
	}
	return nil
}

func anyList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// logEvent appends a sleep.check.failed event; the state dir was verified
// trusted by the caller.
func logEvent(cmd *cobra.Command, data map[string]any) {
	boot, _ := events.CurrentBoot(sysRoot)
	l := events.Log{Root: sysRoot, Path: plan.SysStateDir + "/events.jsonl"}
	err := l.Append(events.Event{
		TS: time.Now().UTC(), Boot: boot, Source: "sleep-check",
		Kind: events.KindSleepCheckFailed, Data: data,
	})
	if err != nil {
		cmd.PrintErrf("warning: not logging event: %v\n", err)
	}
}
