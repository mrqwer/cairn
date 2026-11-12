package events_test

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/events"
	"github.com/mrqwer/cairn/internal/fsx"
)

func newLog(t *testing.T) events.Log {
	t.Helper()
	return events.Log{Root: fsx.Root{Dir: t.TempDir()}, Path: "/var/lib/cairn/events.jsonl"}
}

func ev(kind string) events.Event {
	return events.Event{
		TS:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Boot:   "boot-a",
		Source: "test",
		Kind:   kind,
		Data:   map[string]any{"k": "v"},
	}
}

func TestAppendAndRead(t *testing.T) {
	l := newLog(t)
	if err := l.Append(ev(events.KindSleepPre)); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ev(events.KindSleepPost)); err != nil {
		t.Fatal(err)
	}
	evs, next, err := l.ReadFrom(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != "sleep.pre" || evs[1].Kind != "sleep.post" {
		t.Fatalf("got %+v", evs)
	}
	if !evs[0].TS.Equal(ev("").TS) || evs[0].Data["k"] != "v" {
		t.Fatalf("roundtrip mismatch: %+v", evs[0])
	}
	st, err := l.Root.Stat(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if next != st.Size() {
		t.Fatalf("next %d != size %d", next, st.Size())
	}
}

func TestReadFromOffset(t *testing.T) {
	l := newLog(t)
	if err := l.Append(ev(events.KindSleepPre)); err != nil {
		t.Fatal(err)
	}
	_, next, err := l.ReadFrom(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ev(events.KindSleepPost)); err != nil {
		t.Fatal(err)
	}
	evs, _, err := l.ReadFrom(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "sleep.post" {
		t.Fatalf("got %+v", evs)
	}
}

func TestTruncatedLastLine(t *testing.T) {
	l := newLog(t)
	if err := l.Append(ev(events.KindSleepPre)); err != nil {
		t.Fatal(err)
	}
	st, err := l.Root.Stat(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	good := st.Size()
	f, err := os.OpenFile(l.Root.Path(l.Path), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"2026-`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	evs, next, err := l.ReadFrom(0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %+v, %v", evs, err)
	}
	if next != good {
		t.Fatalf("next %d, want %d", next, good)
	}
	if err := l.Append(ev(events.KindSleepPost)); err != nil {
		t.Fatal(err)
	}
	evs, _, err = l.ReadFrom(0)
	if err != nil || len(evs) != 2 || evs[1].Kind != events.KindSleepPost {
		t.Fatalf("after repair: %+v, %v", evs, err)
	}
}

func TestSkipsGarbageAndMissingFile(t *testing.T) {
	l := newLog(t)
	evs, next, err := l.ReadFrom(0)
	if err != nil || len(evs) != 0 || next != 0 {
		t.Fatalf("missing: %v %d %v", evs, next, err)
	}
	if err := l.Root.WriteAtomic(l.Path, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ev(events.KindRebootRequired)); err != nil {
		t.Fatal(err)
	}
	evs, _, err = l.ReadFrom(0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("got %+v, %v", evs, err)
	}
}

func TestCurrentBoot(t *testing.T) {
	r := fsx.Root{Dir: t.TempDir()}
	if err := r.WriteAtomic("/proc/sys/kernel/random/boot_id", []byte("boot-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := events.CurrentBoot(r)
	if err != nil || got != "boot-a" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestAppendModeIgnoresUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	l := newLog(t)
	if err := l.Append(ev(events.KindSleepPre)); err != nil {
		t.Fatal(err)
	}
	st, err := l.Root.Stat(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", st.Mode().Perm())
	}
}
