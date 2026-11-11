package sleep

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/plan"
)

// EventsPath is the root-owned system event log.
const EventsPath = plan.SysStateDir + "/events.jsonl"

// IDRecent is the doctor check over recent sleep events.
const IDRecent = "sleep.events.recent"

// Finding is a conclusion drawn from the system event log.
type Finding struct {
	Kind, SleepType string
	At              time.Time
}

// previousBootEvent returns the last event not written by currentBoot.
func previousBootEvent(evs []events.Event, currentBoot string) (events.Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Boot != currentBoot {
			return evs[i], true
		}
	}
	return events.Event{}, false
}

// PreviousBoot returns the id of the boot that wrote the last event before
// currentBoot.
func PreviousBoot(evs []events.Event, currentBoot string) (string, bool) {
	e, ok := previousBootEvent(evs, currentBoot)
	return e.Boot, ok
}

// AnalyzePreviousBoot reports whether the previous boot ended in the middle of
// a sleep (its last event is sleep.pre, never followed by sleep.post).
func AnalyzePreviousBoot(evs []events.Event, currentBoot string) (Finding, bool) {
	e, ok := previousBootEvent(evs, currentBoot)
	if !ok || e.Kind != events.KindSleepPre {
		return Finding{}, false
	}
	t, _ := e.Data["type"].(string)
	return Finding{Kind: events.KindBootLostDuringSleep, SleepType: t, At: e.TS}, true
}

// BatteryPercent reads the first (sorted) BAT*/capacity under root.
func BatteryPercent(root fsx.Root) (int, bool) {
	matches, _ := filepath.Glob(root.Path("/sys/class/power_supply/BAT*/capacity"))
	if len(matches) == 0 {
		return 0, false
	}
	rel, err := filepath.Rel(root.Path("/"), matches[0])
	if err != nil {
		return 0, false
	}
	b, err := root.ReadFile("/" + rel)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return v, true
}

// Notification is one desktop notification.
type Notification struct{ Title, Body string }

// NotificationFor maps an event to its notification, if it has one.
func NotificationFor(e events.Event) (Notification, bool) {
	switch e.Kind {
	case events.KindSleepCheckFailed:
		what := "unknown"
		switch f := e.Data["failed"].(type) {
		case []any:
			var ids []string
			for _, v := range f {
				if s, ok := v.(string); ok {
					ids = append(ids, s)
				}
			}
			if len(ids) > 0 {
				what = strings.Join(ids, ", ")
			}
		}
		if what == "unknown" {
			if s, ok := e.Data["error"].(string); ok && s != "" {
				what = s
			}
		}
		return Notification{"Hibernation unavailable", what + ". Suspended instead. Run: cairn doctor host"}, true
	case events.KindSleepFallback:
		return Notification{"Hibernation refused", "Suspended instead of hibernating. Run: cairn doctor host"}, true
	case events.KindRebootRequired:
		return Notification{"Reboot required", "Kernel or GPU driver was upgraded: reboot before relying on hibernation"}, true
	}
	return Notification{}, false
}

// LostNotification is the notification for a boot.lost-during-sleep finding.
func LostNotification(f Finding) Notification {
	t := f.SleepType
	if t == "" {
		t = "sleep"
	}
	return Notification{
		"Laptop lost power during sleep",
		"The previous session ended during " + t + " without resuming. Unsaved work may be lost. Check: cairn doctor host",
	}
}

// Send shows n through notify-send.
func Send(ctx context.Context, r execx.Runner, n Notification) error {
	out, err := r.Run(ctx, "notify-send", "--urgency=critical", "--app-name=cairn", n.Title, n.Body)
	if err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NoCursorWindow bounds how far back a first Notify run looks.
const NoCursorWindow = 24 * time.Hour

// Notify sends a notification for new notifiable events in sys since the
// cursor stored in cursorPath (under user), then advances the cursor. A
// missing system log is not an error.
//
// With no cursor file only events of currentBoot that are at most
// NoCursorWindow old are considered, so installing never replays history. A
// kernel.reboot-required event from another boot is always skipped: the user
// has rebooted since. If the log is smaller than the cursor it was replaced:
// it is re-read from the start, skipping events older than the cursor file.
// Within one batch only the most recent event per notification title is sent.
//
// Delivery is at-least-once: when a notification fails the cursor is not
// advanced, so notifications of the batch already shown may repeat on retry.
func Notify(ctx context.Context, r execx.Runner, sys events.Log, user fsx.Root, cursorPath, currentBoot string, now time.Time) (int, error) {
	st, err := sys.Root.Stat(sys.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var cursor int64
	var since time.Time
	noCursor := false
	if b, err := user.ReadFile(cursorPath); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && v >= 0 {
			cursor = v
		}
		if cst, err := user.Stat(cursorPath); err == nil {
			since = cst.ModTime()
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		noCursor = true
	} else {
		return 0, err
	}
	replaced := cursor > st.Size()
	offset := cursor
	if replaced {
		offset = 0
	}
	evs, next, err := sys.ReadFrom(offset)
	if err != nil {
		return 0, err
	}
	last := map[string]int{}
	notes := make([]Notification, len(evs))
	for i, e := range evs {
		n, ok := NotificationFor(e)
		switch {
		case !ok,
			replaced && e.TS.Before(since),
			noCursor && (e.Boot != currentBoot || now.Sub(e.TS) > NoCursorWindow),
			e.Kind == events.KindRebootRequired && e.Boot != currentBoot:
			continue
		}
		notes[i] = n
		last[n.Title] = i
	}
	sent := 0
	for i, n := range notes {
		if n.Title == "" || last[n.Title] != i {
			continue
		}
		if err := Send(ctx, r, n); err != nil {
			return sent, err
		}
		sent++
	}
	if noCursor || next != cursor {
		if err := user.WriteAtomic(cursorPath, []byte(strconv.FormatInt(next, 10)+"\n"), 0o644); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// RecentEvents is the sleep.events.recent doctor check over the log at path
// under root. It is OK when the log is empty, missing or healthy and warns
// when a failed check or fallback is newer than the last successful resume.
func RecentEvents(root fsx.Root, path string) doctor.Check {
	return fnCheck{id: IDRecent, title: "Recent sleep events", run: func() doctor.Result {
		res := doctor.Result{ID: IDRecent, Title: "Recent sleep events"}
		evs, _, err := events.Log{Root: root, Path: path}.ReadFrom(0)
		if err != nil {
			res.Status, res.Evidence = doctor.Unknown, err.Error()
			return res
		}
		var sl []events.Event
		lastPost := -1
		for _, e := range evs {
			if !strings.HasPrefix(e.Kind, "sleep.") {
				continue
			}
			if e.Kind == events.KindSleepPost {
				lastPost = len(sl)
			}
			sl = append(sl, e)
		}
		if len(sl) == 0 {
			res.Evidence = "no sleep events recorded yet"
			return res
		}
		first := max(len(sl)-5, 0)
		var lines []string
		for i := len(sl) - 1; i >= first; i-- {
			e := sl[i]
			t, _ := e.Data["type"].(string)
			lines = append(lines, strings.TrimSpace(e.TS.UTC().Format(time.RFC3339)+" "+e.Kind+" "+t))
			if i > lastPost && (e.Kind == events.KindSleepCheckFailed || e.Kind == events.KindSleepFallback) {
				res.Status = doctor.Warn
			}
		}
		res.Evidence = strings.Join(lines, "; ")
		if res.Status == doctor.Warn {
			res.Fix = "Run: sudo cairn host sleep-check, then fix the failing checks"
		}
		return res
	}}
}

// IDFindings is the doctor check over silent-failure findings.
const IDFindings = "host.findings"

// FindingsWindow bounds how old a boot.lost-during-sleep finding may be to
// be shown by FindingsCheck.
const FindingsWindow = 14 * 24 * time.Hour

const titleFindings = "Silent sleep failures"

// FindingsCheck is the host.findings doctor check. It warns with the last
// (up to 3) boot.lost-during-sleep findings of the past FindingsWindow from
// findingsPath (the user's findings log under root) and when the system
// event log has a kernel.reboot-required event of the current boot.
func FindingsCheck(root fsx.Root, findingsPath string, now func() time.Time) doctor.Check {
	return fnCheck{id: IDFindings, title: titleFindings, run: func() doctor.Result {
		res := doctor.Result{ID: IDFindings, Title: titleFindings, Evidence: "none"}
		fs, _, err := events.Log{Root: root, Path: findingsPath}.ReadFrom(0)
		if err != nil {
			res.Status, res.Evidence = doctor.Unknown, err.Error()
			return res
		}
		evs, _, err := events.Log{Root: root, Path: EventsPath}.ReadFrom(0)
		if err != nil {
			res.Status, res.Evidence = doctor.Unknown, err.Error()
			return res
		}
		var lines []string
		for i := len(fs) - 1; i >= 0 && len(lines) < 3; i-- {
			e := fs[i]
			if e.Kind != events.KindBootLostDuringSleep || now().Sub(e.TS) > FindingsWindow {
				continue
			}
			t, _ := e.Data["type"].(string)
			if t == "" {
				t = "sleep"
			}
			lines = append(lines, e.TS.UTC().Format(time.RFC3339)+" power lost during "+t)
		}
		if len(lines) > 0 {
			res.Status, res.Fix = doctor.Warn, "check the battery and the hibernation checks above"
		}
		if cur, err := events.CurrentBoot(root); err == nil {
			for _, e := range evs {
				if e.Kind == events.KindRebootRequired && e.Boot == cur {
					lines = append(lines, "kernel or GPU driver upgraded: reboot required for safe hibernation")
					res.Status, res.Fix = doctor.Warn, "reboot"
					break
				}
			}
		}
		if len(lines) > 0 {
			res.Evidence = strings.Join(lines, "; ")
		}
		return res
	}}
}

// RepeatAfterFailure reports whether the sleep cycle that is ending followed
// a refused hibernation in currentBoot: walking back over this boot's sleep.*
// events and skipping sleep.pre (the cycle itself), the first one found is a
// sleep.fallback or an original (not repeated) sleep.check.failed.
func RepeatAfterFailure(evs []events.Event, currentBoot string) bool {
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Boot != currentBoot || !strings.HasPrefix(e.Kind, "sleep.") || e.Kind == events.KindSleepPre {
			continue
		}
		switch e.Kind {
		case events.KindSleepFallback:
			return true
		case events.KindSleepCheckFailed:
			return e.Data["repeat"] != true
		}
		return false
	}
	return false
}
