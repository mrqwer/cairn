package sleep

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/doctor"
	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

var t0 = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

func evt(boot, kind string, min int, data map[string]any) events.Event {
	return events.Event{TS: t0.Add(time.Duration(min) * time.Minute), Boot: boot, Source: "t", Kind: kind, Data: data}
}

func typ(s string) map[string]any { return map[string]any{"type": s} }

func TestLostDuringSuspend(t *testing.T) {
	f, ok := AnalyzePreviousBoot([]events.Event{evt("A", events.KindSleepPre, 0, typ("suspend"))}, "B")
	if !ok || f.Kind != "boot.lost-during-sleep" || f.SleepType != "suspend" {
		t.Fatalf("%+v %v", f, ok)
	}
}

func TestCleanResumeNoFinding(t *testing.T) {
	evs := []events.Event{evt("A", events.KindSleepPre, 0, typ("hibernate")), evt("A", events.KindSleepPost, 1, typ("hibernate"))}
	if f, ok := AnalyzePreviousBoot(evs, "B"); ok {
		t.Fatalf("unexpected %+v", f)
	}
	if _, ok := AnalyzePreviousBoot(nil, "B"); ok {
		t.Fatal("empty log must not produce a finding")
	}
	// Events of the current boot are ignored.
	if _, ok := AnalyzePreviousBoot([]events.Event{evt("B", events.KindSleepPre, 0, typ("suspend"))}, "B"); ok {
		t.Fatal("current boot must not count")
	}
}

type notifyEnv struct {
	sys    events.Log
	user   fsx.Root
	cursor string
	run    *execx.FakeRunner
	boot   string
	now    time.Time
}

func newNotifyEnv(t *testing.T) *notifyEnv {
	t.Helper()
	root := fsx.Root{Dir: t.TempDir()}
	n := &notifyEnv{
		sys:    events.Log{Root: root, Path: EventsPath},
		user:   root,
		cursor: "/home/u/.local/state/cairn/host/notify.cursor",
		run:    &execx.FakeRunner{Responses: map[string]execx.Response{}},
		boot:   "A",
		now:    t0.Add(time.Hour),
	}
	return n
}

func (n *notifyEnv) want(title, body string) {
	n.run.Responses["notify-send --urgency=critical --app-name=cairn "+title+" "+body] = execx.Response{}
}

func (n *notifyEnv) notify(t *testing.T) int {
	t.Helper()
	c, err := Notify(context.Background(), n.run, n.sys, n.user, n.cursor, n.boot, n.now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNotifySendsOncePerEvent(t *testing.T) {
	n := newNotifyEnv(t)
	n.want("Hibernation refused", "Suspended instead of hibernating. Run: cairn doctor host")
	for _, e := range []events.Event{evt("A", events.KindSleepPre, 0, typ("suspend")), evt("A", events.KindSleepFallback, 1, nil)} {
		if err := n.sys.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	n.notify(t)
	n.notify(t)
	if len(n.run.Calls) != 1 {
		t.Fatalf("calls %v", n.run.Calls)
	}
}

func TestNotifyMessages(t *testing.T) {
	n := newNotifyEnv(t)
	n.want("Hibernation unavailable", "boom. Suspended instead. Run: cairn doctor host")
	n.want("Reboot required", "Kernel or GPU driver was upgraded: reboot before relying on hibernation")
	for _, e := range []events.Event{
		evt("A", events.KindSleepCheckFailed, 1, map[string]any{"error": "boom"}),
		evt("A", events.KindRebootRequired, 2, nil),
	} {
		if err := n.sys.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if c := n.notify(t); c != 2 {
		t.Fatalf("sent %d", c)
	}
	want := []string{"notify-send", "--urgency=critical", "--app-name=cairn", "Hibernation unavailable"}
	if got := n.run.Calls[0][:4]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("argv %v", n.run.Calls[0])
	}
}

func TestNotifyMissingLog(t *testing.T) {
	n := newNotifyEnv(t)
	if c := n.notify(t); c != 0 || len(n.run.Calls) != 0 {
		t.Fatal("must be a no-op")
	}
}

func TestNotifyReplacedLogSkipsOldEvents(t *testing.T) {
	n := newNotifyEnv(t)
	n.now = time.Now()
	n.want("Hibernation refused", "Suspended instead of hibernating. Run: cairn doctor host")
	// Cursor far beyond the log size, written "now".
	if err := n.user.WriteAtomic(n.cursor, []byte("99999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := evt("A", events.KindSleepFallback, 0, nil)
	old.TS = time.Now().Add(-time.Hour)
	fresh := evt("A", events.KindSleepFallback, 0, nil)
	fresh.TS = time.Now().Add(time.Hour)
	for _, e := range []events.Event{old, fresh} {
		if err := n.sys.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if c := n.notify(t); c != 1 {
		t.Fatalf("sent %d, want only the fresh event", c)
	}
	n.notify(t)
	if len(n.run.Calls) != 1 {
		t.Fatalf("calls %v", n.run.Calls)
	}
}

func TestNotifyFailureKeepsCursor(t *testing.T) {
	n := newNotifyEnv(t)
	if err := n.sys.Append(evt("A", events.KindRebootRequired, 0, nil)); err != nil {
		t.Fatal(err)
	}
	n.run.Responses["notify-send --urgency=critical --app-name=cairn Reboot required Kernel or GPU driver was upgraded: reboot before relying on hibernation"] = execx.Response{Err: errors.New("no bus")}
	if _, err := Notify(context.Background(), n.run, n.sys, n.user, n.cursor, n.boot, n.now); err == nil {
		t.Fatal("want error")
	}
	if n.user.Exists(n.cursor) {
		t.Fatal("cursor must not advance after a failed notification")
	}
}

func TestBatteryPercent(t *testing.T) {
	root := fsx.Root{Dir: t.TempDir()}
	if _, ok := BatteryPercent(root); ok {
		t.Fatal("no battery")
	}
	for name, v := range map[string]string{"BAT1": "55\n", "BAT0": "81\n"} {
		d := root.Path("/sys/class/power_supply/" + name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "capacity"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p, ok := BatteryPercent(root); !ok || p != 81 {
		t.Fatalf("%d %v", p, ok)
	}
}

func TestRecentEventsCheck(t *testing.T) {
	run := func(evs ...events.Event) doctor.Result {
		root := fsx.Root{Dir: t.TempDir()}
		l := events.Log{Root: root, Path: EventsPath}
		for _, e := range evs {
			if err := l.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		return RecentEvents(root, EventsPath).Run(context.Background())
	}
	if r := run(); r.Status != doctor.OK || r.ID != IDRecent {
		t.Fatalf("empty: %+v", r)
	}
	r := run(evt("A", events.KindSleepPre, 0, typ("hibernate")), evt("A", events.KindSleepPost, 1, typ("hibernate")))
	if r.Status != doctor.OK || !strings.Contains(r.Evidence, "2026-01-02T03:01:00Z sleep.post hibernate") {
		t.Fatalf("healthy: %+v", r)
	}
	r = run(evt("A", events.KindSleepPost, 0, typ("suspend")), evt("A", events.KindSleepFallback, 1, typ("hibernate")))
	if r.Status != doctor.Warn || r.Fix == "" {
		t.Fatalf("fallback after post: %+v", r)
	}
	r = run(evt("A", events.KindSleepCheckFailed, 0, nil), evt("A", events.KindSleepPost, 1, typ("suspend")))
	if r.Status != doctor.OK {
		t.Fatalf("failure older than post: %+v", r)
	}
	var many []events.Event
	for i := range 8 {
		many = append(many, evt("A", events.KindSleepPre, i, typ("suspend")))
	}
	if r := run(many...); strings.Count(r.Evidence, "sleep.pre") != 5 {
		t.Fatalf("want 5 entries: %s", r.Evidence)
	}
}

func TestNotifyFailedIDsListed(t *testing.T) {
	n := newNotifyEnv(t)
	n.want("Hibernation unavailable", "sleep.swap.free, sleep.resume.match. Suspended instead. Run: cairn doctor host")
	if err := n.sys.Append(evt("A", events.KindSleepCheckFailed, 0, map[string]any{"failed": []any{"sleep.swap.free", "sleep.resume.match"}})); err != nil {
		t.Fatal(err)
	}
	if c := n.notify(t); c != 1 {
		t.Fatalf("sent %d", c)
	}
}

func cursorOf(t *testing.T, n *notifyEnv) string {
	t.Helper()
	b, err := n.user.ReadFile(n.cursor)
	if err != nil {
		t.Fatalf("cursor missing: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestNotifyNoCursorIgnoresOldBootEvents(t *testing.T) {
	n := newNotifyEnv(t)
	n.boot = "B"
	for _, e := range []events.Event{evt("A", events.KindSleepFallback, 0, nil), evt("A", events.KindRebootRequired, 1, nil)} {
		if err := n.sys.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if c := n.notify(t); c != 0 || len(n.run.Calls) != 0 {
		t.Fatalf("sent %d, calls %v", c, n.run.Calls)
	}
	st, _ := n.sys.Root.Stat(n.sys.Path)
	if got := cursorOf(t, n); got != strconv.FormatInt(st.Size(), 10) {
		t.Fatalf("cursor %s, want end of log %d", got, st.Size())
	}
}

func TestNotifyNoCursorCurrentBootFailureAndAge(t *testing.T) {
	n := newNotifyEnv(t)
	n.want("Hibernation refused", "Suspended instead of hibernating. Run: cairn doctor host")
	if err := n.sys.Append(evt("A", events.KindSleepFallback, 0, nil)); err != nil {
		t.Fatal(err)
	}
	if c := n.notify(t); c != 1 {
		t.Fatalf("sent %d", c)
	}
	// Same boot but older than 24h: not replayed.
	m := newNotifyEnv(t)
	m.now = t0.Add(25 * time.Hour)
	if err := m.sys.Append(evt("A", events.KindSleepFallback, 0, nil)); err != nil {
		t.Fatal(err)
	}
	if c := m.notify(t); c != 0 {
		t.Fatalf("stale event notified: %d", c)
	}
}

func TestNotifyRebootRequiredFromPreviousBootSkipped(t *testing.T) {
	n := newNotifyEnv(t)
	if err := n.user.WriteAtomic(n.cursor, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n.boot = "B"
	if err := n.sys.Append(evt("A", events.KindRebootRequired, 0, nil)); err != nil {
		t.Fatal(err)
	}
	if c := n.notify(t); c != 0 || len(n.run.Calls) != 0 {
		t.Fatalf("sent %d", c)
	}
}

func TestNotifyCoalescesPerTitle(t *testing.T) {
	n := newNotifyEnv(t)
	n.want("Hibernation unavailable", "third. Suspended instead. Run: cairn doctor host")
	for i, msg := range []string{"first", "second", "third"} {
		if err := n.sys.Append(evt("A", events.KindSleepCheckFailed, i, map[string]any{"error": msg})); err != nil {
			t.Fatal(err)
		}
	}
	if c := n.notify(t); c != 1 || len(n.run.Calls) != 1 {
		t.Fatalf("sent %d calls %v", c, n.run.Calls)
	}
}
