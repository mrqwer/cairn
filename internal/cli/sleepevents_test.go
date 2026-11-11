package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/plan"
	"github.com/mrqwer/cairn/internal/xdg"
)

type seEnv struct {
	root fsx.Root
	run  *execx.FakeRunner
	st   string
}

func setupSE(t *testing.T) *seEnv {
	t.Helper()
	e := &seEnv{root: fsx.Root{Dir: t.TempDir()}, run: &execx.FakeRunner{Responses: map[string]execx.Response{
		"uname -r": {Out: []byte("6.1.0-test\n")},
	}}}
	e.st = "/home/u/.local/state/cairn"
	oRoot, oRun, oDirs, oOwn := sysRoot, eventRunner, userDirs, stateOwnerOf
	t.Cleanup(func() { sysRoot, eventRunner, userDirs, stateOwnerOf = oRoot, oRun, oDirs, oOwn })
	sysRoot, eventRunner = e.root, e.run
	userDirs = func() (xdg.Dirs, error) { return xdg.Dirs{State: e.st}, nil }
	stateOwnerOf = func(fs.FileInfo) (uint32, bool) { return 0, true }
	if err := e.root.MkdirAll(plan.SysStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.root.WriteAtomic("/proc/sys/kernel/random/boot_id", []byte("boot-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *seEnv) exec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var o, er bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&er)
	cmd.SetArgs(append([]string{"host"}, args...))
	err := cmd.Execute()
	return er.String(), err
}

func (e *seEnv) events(t *testing.T) []events.Event {
	evs, _, err := events.Log{Root: e.root, Path: sleep.EventsPath}.ReadFrom(0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestRecordSleepPre(t *testing.T) {
	e := setupSE(t)
	d := e.root.Path("/sys/class/power_supply/BAT0")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "capacity"), []byte("42\n"), 0o644)
	if _, err := e.exec(t, "record-sleep", "pre", "suspend"); err != nil {
		t.Fatal(err)
	}
	evs := e.events(t)
	if len(evs) != 1 || evs[0].Kind != events.KindSleepPre || evs[0].Source != "system-sleep" || evs[0].Boot != "boot-b" {
		t.Fatalf("%+v", evs)
	}
	dt := evs[0].Data
	if dt["type"] != "suspend" || dt["kernel"] != "6.1.0-test" || dt["battery_percent"] != float64(42) {
		t.Fatalf("data %v", dt)
	}
}

func TestRecordSleepSources(t *testing.T) {
	e := setupSE(t)
	for _, a := range [][]string{{"post", "hibernate"}, {"fallback"}, {"reboot-required"}} {
		if _, err := e.exec(t, append([]string{"record-sleep"}, a...)...); err != nil {
			t.Fatal(err)
		}
	}
	evs := e.events(t)
	if len(evs) != 3 || evs[0].Kind != "sleep.post" || evs[1].Source != "cairn-sleep-fallback" ||
		evs[2].Kind != "kernel.reboot-required" || evs[2].Source != "package-hook" {
		t.Fatalf("%+v", evs)
	}
}

func TestRecordSleepUsageAndUntrusted(t *testing.T) {
	e := setupSE(t)
	for _, a := range [][]string{{"record-sleep"}, {"record-sleep", "bogus"}} {
		errOut, err := e.exec(t, a...)
		if exitCode(err) != 2 || !strings.Contains(errOut, "usage:") {
			t.Fatalf("%v: %v %q", a, err, errOut)
		}
	}
	stateOwnerOf = func(fs.FileInfo) (uint32, bool) { return 1000, true }
	errOut, err := e.exec(t, "record-sleep", "pre", "suspend")
	if err != nil || !strings.Contains(errOut, "warning") || len(e.events(t)) != 0 {
		t.Fatalf("untrusted must warn and exit 0: %v %q", err, errOut)
	}
}

func TestBootCheckNotifiesOnce(t *testing.T) {
	e := setupSE(t)
	l := events.Log{Root: e.root, Path: sleep.EventsPath}
	if err := l.Append(events.Event{TS: time.Now(), Boot: "boot-a", Source: "t", Kind: events.KindSleepPre, Data: map[string]any{"type": "hibernate"}}); err != nil {
		t.Fatal(err)
	}
	body := "The previous session ended during hibernate without resuming. Unsaved work may be lost. Check: cairn doctor host"
	e.run.Responses["notify-send --urgency=critical --app-name=cairn Laptop lost power during sleep "+body] = execx.Response{}
	for range 2 {
		if _, err := e.exec(t, "boot-check"); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.run.Calls) != 1 {
		t.Fatalf("calls %v", e.run.Calls)
	}
	fs, _, _ := events.Log{Root: e.root, Path: e.st + "/host/findings.jsonl"}.ReadFrom(0)
	if len(fs) != 1 || fs[0].Kind != events.KindBootLostDuringSleep {
		t.Fatalf("findings %+v", fs)
	}
}

func TestBootCheckAndNotifyMissingLog(t *testing.T) {
	e := setupSE(t)
	for _, c := range []string{"boot-check", "notify"} {
		if _, err := e.exec(t, c); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
	if len(e.run.Calls) != 0 {
		t.Fatal("no notifications expected")
	}
}

func TestBootCheckRetriesAfterFailedSend(t *testing.T) {
	e := setupSE(t)
	l := events.Log{Root: e.root, Path: sleep.EventsPath}
	if err := l.Append(events.Event{TS: time.Now(), Boot: "boot-a", Source: "t", Kind: events.KindSleepPre, Data: map[string]any{"type": "suspend"}}); err != nil {
		t.Fatal(err)
	}
	key := "notify-send --urgency=critical --app-name=cairn Laptop lost power during sleep " +
		"The previous session ended during suspend without resuming. Unsaved work may be lost. Check: cairn doctor host"
	e.run.Responses[key] = execx.Response{Err: errors.New("no bus")}
	if _, err := e.exec(t, "boot-check"); err == nil {
		t.Fatal("want failure on first run")
	}
	e.run.Responses[key] = execx.Response{}
	if _, err := e.exec(t, "boot-check"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.exec(t, "boot-check"); err != nil {
		t.Fatal(err)
	}
	if len(e.run.Calls) != 2 {
		t.Fatalf("want one failed + one delivered send, got %v", e.run.Calls)
	}
	fs, _, _ := events.Log{Root: e.root, Path: e.st + "/host/findings.jsonl"}.ReadFrom(0)
	if len(fs) != 1 {
		t.Fatalf("want exactly one finding, got %+v", fs)
	}
}

func TestRecordSleepPostRepeatsFailedCheck(t *testing.T) {
	e := setupSE(t)
	oDet, oIs := detectSystem, isRoot
	t.Cleanup(func() { detectSystem, isRoot = oDet, oIs })
	isRoot = func() bool { return true }
	detectSystem = func(context.Context) (detect.System, error) {
		return detect.System{KernelModulesPresent: true, InitramfsResume: detect.Yes}, nil // no swap: Crit
	}
	l := events.Log{Root: e.root, Path: sleep.EventsPath}
	for _, k := range []string{events.KindSleepCheckFailed, events.KindSleepFallback, events.KindSleepPre} {
		if err := l.Append(events.Event{TS: time.Now(), Boot: "boot-b", Source: "t", Kind: k}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.exec(t, "record-sleep", "post", "suspend"); err != nil {
		t.Fatal(err)
	}
	evs := e.events(t)
	last := evs[len(evs)-1]
	if len(evs) != 5 || evs[3].Kind != events.KindSleepPost || last.Kind != events.KindSleepCheckFailed ||
		last.Data["repeat"] != true || !strings.Contains(fmt.Sprint(last.Data["failed"]), "sleep.swap.present") {
		t.Fatalf("%+v", evs)
	}
	// The repeat itself does not trigger another one.
	if _, err := e.exec(t, "record-sleep", "pre", "suspend"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.exec(t, "record-sleep", "post", "suspend"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.exec(t, "record-sleep", "pre", "suspend"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.exec(t, "record-sleep", "post", "suspend"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(t)); n != 9 {
		t.Fatalf("want 9 events (no further repeat), got %d: %+v", n, e.events(t))
	}
}
