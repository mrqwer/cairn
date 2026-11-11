package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Seams for tests.
var (
	eventRunner execx.Runner     = execx.OSRunner{}
	userDirs                     = func() (xdg.Dirs, error) { return xdg.Resolve(os.Getenv) }
	nowFunc     func() time.Time = time.Now
)

const recordUsage = "usage: cairn host record-sleep <pre|post|fallback|reboot-required> [type]"

func newRecordSleepCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "record-sleep <pre|post|fallback|reboot-required> [type]",
		Short:  "Append a sleep event to the system log (run as root by hooks)",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var kind, source string
			if len(args) >= 1 && len(args) <= 2 {
				switch args[0] {
				case "pre":
					kind, source = events.KindSleepPre, "system-sleep"
				case "post":
					kind, source = events.KindSleepPost, "system-sleep"
				case "fallback":
					kind, source = events.KindSleepFallback, "cairn-sleep-fallback"
				case "reboot-required":
					kind, source = events.KindRebootRequired, "package-hook"
				}
			}
			if kind == "" {
				cmd.PrintErrln(recordUsage)
				return &ExitError{Code: 2}
			}
			var typ string
			if len(args) == 2 {
				typ = args[1]
			}
			// A logging hook must never block sleep: every failure below
			// warns and exits 0.
			if err := plan.TrustedDir(sysRoot, plan.SysStateDir, stateOwnerOf); err != nil {
				cmd.PrintErrf("warning: state directory not trusted or missing (%v); not recording\n", err)
				return nil
			}
			data := map[string]any{}
			if typ != "" {
				data["type"] = typ
			}
			uctx, cancel := context.WithTimeout(ctxOf(cmd), 2*time.Second)
			defer cancel()
			if out, err := eventRunner.Run(uctx, "uname", "-r"); err == nil {
				if k := strings.TrimSpace(string(out)); k != "" {
					data["kernel"] = k
				}
			}
			if pct, ok := sleep.BatteryPercent(sysRoot); ok {
				data["battery_percent"] = pct
			}
			boot, _ := events.CurrentBoot(sysRoot)
			l := events.Log{Root: sysRoot, Path: sleep.EventsPath}
			var repeat bool
			if kind == events.KindSleepPost {
				prior, _, _ := l.ReadFrom(0)
				repeat = sleep.RepeatAfterFailure(prior, boot)
			}
			err := l.Append(events.Event{TS: nowFunc().UTC(), Boot: boot, Source: source, Kind: kind, Data: data})
			if err != nil {
				cmd.PrintErrf("warning: not recording event: %v\n", err)
			}
			if repeat {
				repeatFailedCheck(cmd, l, boot)
			}
			return nil
		},
	}
}

// repeatTimeout bounds the re-run of the sleep checks on resume.
var repeatTimeout = 5 * time.Second

// repeatFailedCheck re-runs the sleep checks after resuming from a sleep that
// replaced a refused hibernation and, when one is still critical, logs a new
// sleep.check.failed event (data repeat=true) so the user is warned again
// (spec 5.2 item 3). Best effort: errors and timeouts only warn.
func repeatFailedCheck(cmd *cobra.Command, l events.Log, boot string) {
	run, err := bounded(ctxOf(cmd), repeatTimeout, func(ctx context.Context) (sleepRun, error) {
		return rootSleepResults(ctx, true)
	})
	if err != nil {
		cmd.PrintErrf("warning: not repeating the sleep checks: %v\n", err)
		return
	}
	if len(sleep.FailedIDs(run.results)) == 0 {
		return
	}
	data := sleep.FailedEventData(run.results)
	data["repeat"] = true
	err = l.Append(events.Event{
		TS: nowFunc().UTC(), Boot: boot, Source: "record-sleep", Kind: events.KindSleepCheckFailed, Data: data,
	})
	if err != nil {
		cmd.PrintErrf("warning: not recording event: %v\n", err)
	}
}

func newBootCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "boot-check",
		Short:  "Detect a previous boot lost during sleep and notify (user unit at login)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := userDirs()
			if err != nil {
				return err
			}
			evs, _, err := events.Log{Root: sysRoot, Path: sleep.EventsPath}.ReadFrom(0)
			if err != nil {
				return err
			}
			cur, err := events.CurrentBoot(sysRoot)
			if err != nil {
				return err
			}
			prev, ok := sleep.PreviousBoot(evs, cur)
			if !ok {
				return nil
			}
			lastPath := filepath.Join(dirs.State, "host", "boot-check.last")
			if b, err := sysRoot.ReadFile(lastPath); err == nil && strings.TrimSpace(string(b)) == prev {
				return nil
			}
			f, found := sleep.AnalyzePreviousBoot(evs, cur)
			if found {
				fl := events.Log{Root: sysRoot, Path: filepath.Join(dirs.State, "host", "findings.jsonl")}
				if err := recordFinding(fl, f, prev, cur); err != nil {
					return err
				}
				// The alert must survive a not-yet-ready notification
				// daemon at login: only a delivered one marks the boot
				// handled, so a retry sends it again.
				if err := sleep.Send(ctxOf(cmd), eventRunner, sleep.LostNotification(f)); err != nil {
					return err
				}
			}
			return sysRoot.WriteAtomic(lastPath, []byte(prev+"\n"), 0o644)
		},
	}
}

// recordFinding appends f unless a finding for the same previous boot exists.
func recordFinding(fl events.Log, f sleep.Finding, prev, cur string) error {
	old, _, err := fl.ReadFrom(0)
	if err != nil {
		return err
	}
	for _, e := range old {
		if e.Kind == f.Kind && e.Data["previous_boot"] == prev {
			return nil
		}
	}
	return fl.Append(events.Event{
		TS: nowFunc().UTC(), Boot: cur, Source: "boot-check", Kind: f.Kind,
		Data: map[string]any{"type": f.SleepType, "previous_boot": prev},
	})
}

func newNotifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "notify",
		Short:  "Send desktop notifications for new system sleep events (user path unit)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := userDirs()
			if err != nil {
				return err
			}
			cur, err := events.CurrentBoot(sysRoot)
			if err != nil {
				return err
			}
			sys := events.Log{Root: sysRoot, Path: sleep.EventsPath}
			_, err = sleep.Notify(ctxOf(cmd), eventRunner, sys, sysRoot, filepath.Join(dirs.State, "host", "notify.cursor"), cur, nowFunc())
			return err
		},
	}
}
