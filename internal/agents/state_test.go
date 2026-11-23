package agents_test

import (
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

var t0 = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

// legal is the transition table of spec §9.3 as cairn applies it.
var legal = map[string][]string{
	conv.StateStarting: {conv.StateWorking, conv.StateWaiting, conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped},
	conv.StateWorking:  {conv.StateWaiting, conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped},
	conv.StateWaiting:  {conv.StateWorking, conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped},
	conv.StateIdle:     {conv.StateWorking, conv.StateWaiting, conv.StateDone, conv.StateFailed, conv.StateStopped},
}

func TestFoldTransitions(t *testing.T) {
	states := []string{conv.StateStarting, conv.StateWorking, conv.StateWaiting, conv.StateIdle,
		conv.StateDone, conv.StateFailed, conv.StateStopped, conv.StateArchived}
	kinds := []string{"working", "waiting", "idle", "done", "failed", "stopped"}
	for _, from := range states {
		for _, kind := range kinds {
			c := conv.Conversation{ID: "C", State: from, StateSource: conv.SourceObserved, LastActivityAt: t0}
			ev := agents.Event{TS: t0.Add(time.Second), Conversation: "C", Kind: kind, Reason: "r"}
			got := agents.Fold(c, ev)
			ok := from == kind && from != conv.StateArchived
			for _, to := range legal[from] {
				ok = ok || to == kind
			}
			if ok {
				if got.State != kind || got.StateSource != conv.SourceHook || !got.LastActivityAt.Equal(ev.TS) || got.IgnoredEvents != 0 {
					t.Errorf("%s -> %s must apply: %+v", from, kind, got)
				}
				continue
			}
			if got.State != from || got.StateSource != conv.SourceObserved || !got.LastActivityAt.Equal(t0) || got.IgnoredEvents != 1 {
				t.Errorf("%s -> %s must be ignored and counted: %+v", from, kind, got)
			}
		}
	}
}

func TestFoldEmptyStateIsStarting(t *testing.T) {
	got := agents.Fold(conv.Conversation{ID: "C"}, agents.Event{TS: t0, Kind: "working"})
	if got.State != conv.StateWorking {
		t.Fatalf("state %q", got.State)
	}
}

func TestFoldWaitingReason(t *testing.T) {
	c := conv.Conversation{ID: "C", State: conv.StateWorking, LastActivityAt: t0}
	c = agents.Fold(c, agents.Event{TS: t0.Add(time.Second), Kind: "waiting", Reason: "permission: Bash"})
	if c.State != conv.StateWaiting || c.WaitingReason != "permission: Bash" {
		t.Fatalf("%+v", c)
	}
	c = agents.Fold(c, agents.Event{TS: t0.Add(2 * time.Second), Kind: "working"})
	if c.State != conv.StateWorking || c.WaitingReason != "" {
		t.Fatalf("reason must clear on leaving waiting: %+v", c)
	}
}

func TestFoldActivityOnlyBumpsTime(t *testing.T) {
	c := conv.Conversation{ID: "C", State: conv.StateWaiting, StateSource: conv.SourceObserved, WaitingReason: "x", LastActivityAt: t0}
	got := agents.Fold(c, agents.Event{TS: t0.Add(time.Minute), Kind: "activity"})
	if got.State != conv.StateWaiting || got.StateSource != conv.SourceObserved || got.WaitingReason != "x" ||
		!got.LastActivityAt.Equal(t0.Add(time.Minute)) || got.IgnoredEvents != 0 {
		t.Fatalf("%+v", got)
	}
	arch := t0
	a := conv.Conversation{ID: "C", State: conv.StateIdle, ArchivedAt: &arch, LastActivityAt: t0}
	if got := agents.Fold(a, agents.Event{TS: t0.Add(time.Minute), Kind: "working"}); got.State != conv.StateIdle || got.IgnoredEvents != 1 {
		t.Fatalf("archived must only be left by resume: %+v", got)
	}
	if got := agents.Fold(c, agents.Event{TS: t0.Add(time.Minute), Kind: "bogus"}); got.State != conv.StateWaiting || got.IgnoredEvents != 1 {
		t.Fatalf("unknown kind: %+v", got)
	}
}

func TestFoldIgnoresStaleEvents(t *testing.T) {
	c := conv.Conversation{ID: "C", State: conv.StateWorking, StateSource: conv.SourceHook, LastActivityAt: t0}
	got := agents.Fold(c, agents.Event{TS: t0.Add(-time.Second), Kind: "waiting"})
	if got.State != conv.StateWorking || !got.LastActivityAt.Equal(t0) || got.IgnoredEvents != 1 {
		t.Fatalf("stale event applied: %+v", got)
	}
	got = agents.Fold(c, agents.Event{TS: t0.Add(-time.Second), Kind: "activity"})
	if !got.LastActivityAt.Equal(t0) {
		t.Fatalf("stale activity moved time back: %+v", got)
	}
}

func TestFoldNativeIDOnlyFillsUnknown(t *testing.T) {
	c := conv.Conversation{ID: "C", State: conv.StateWorking, LastActivityAt: t0}
	got := agents.Fold(c, agents.Event{TS: t0.Add(time.Second), Kind: "activity", NativeID: "abc-1"})
	if got.NativeID != "abc-1" {
		t.Fatalf("%+v", got)
	}
	got = agents.Fold(got, agents.Event{TS: t0.Add(2 * time.Second), Kind: "activity", NativeID: "other"})
	if got.NativeID != "abc-1" {
		t.Fatalf("known native id overwritten: %+v", got)
	}
	got = agents.Fold(c, agents.Event{TS: t0.Add(time.Second), Kind: "activity", NativeID: "../x"})
	if got.NativeID != "" {
		t.Fatalf("unsafe native id taken: %+v", got)
	}
}

func TestObserveFallbackMarksLowConfidence(t *testing.T) {
	now := t0.Add(time.Hour)
	base := conv.Conversation{ID: "C", State: conv.StateWorking, StateSource: conv.SourceObserved, LastActivityAt: t0}

	got := agents.Observe(base, true, 0, 0, now)
	if got.State != conv.StateStopped || got.StateSource != conv.SourceObserved || !got.LastActivityAt.Equal(now) {
		t.Fatalf("dead pane, status 0: %+v", got)
	}
	if got := agents.Observe(base, true, 3, 0, now); got.State != conv.StateFailed || got.StateSource != conv.SourceObserved {
		t.Fatalf("dead pane, status 3: %+v", got)
	}
	got = agents.Observe(base, false, 0, agents.ObserveIdleAfter, now)
	if got.State != conv.StateIdle || got.StateSource != conv.SourceObserved {
		t.Fatalf("long idle: %+v", got)
	}
	got = agents.Observe(got, false, 0, time.Second, now)
	if got.State != conv.StateWorking || got.StateSource != conv.SourceObserved {
		t.Fatalf("output again: %+v", got)
	}
	start := base
	start.State = conv.StateStarting
	if got := agents.Observe(start, false, 0, time.Second, now); got.State != conv.StateWorking {
		t.Fatalf("starting with output: %+v", got)
	}

	// A hook-sourced live state is not overridden by observation...
	hook := base
	hook.StateSource = conv.SourceHook
	hook.State = conv.StateWaiting
	if got := agents.Observe(hook, false, 0, agents.ObserveIdleAfter, now); got.State != conv.StateWaiting || got.StateSource != conv.SourceHook {
		t.Fatalf("observation overrode a hook state: %+v", got)
	}
	// ...but a dead pane is authoritative.
	if got := agents.Observe(hook, true, 0, 0, now); got.State != conv.StateStopped || got.StateSource != conv.SourceObserved {
		t.Fatalf("dead pane with hook state: %+v", got)
	}
	// Terminal and archived states stay.
	done := base
	done.State = conv.StateDone
	if got := agents.Observe(done, false, 0, time.Second, now); got.State != conv.StateDone {
		t.Fatalf("done revived: %+v", got)
	}
	arch := t0
	a := base
	a.ArchivedAt = &arch
	if got := agents.Observe(a, true, 1, 0, now); got.State != conv.StateWorking || got.ArchivedAt == nil {
		t.Fatalf("archived changed: %+v", got)
	}
}

func TestFoldReadyOnlyFromStarting(t *testing.T) {
	c := conv.Conversation{ID: "C", State: conv.StateStarting, LastActivityAt: t0}
	got := agents.Fold(c, agents.Event{TS: t0.Add(time.Second), Kind: "ready"})
	if got.State != conv.StateIdle || got.StateSource != conv.SourceHook {
		t.Fatalf("starting + ready: %+v", got)
	}
	for _, from := range []string{conv.StateWorking, conv.StateWaiting, conv.StateIdle, conv.StateStopped} {
		c := conv.Conversation{ID: "C", State: from, StateSource: conv.SourceObserved, LastActivityAt: t0}
		got := agents.Fold(c, agents.Event{TS: t0.Add(time.Second), Kind: "ready"})
		if got.State != from || got.StateSource != conv.SourceObserved || got.IgnoredEvents != 0 {
			t.Fatalf("%s + ready must change nothing: %+v", from, got)
		}
	}
}

// TestFoldClaudeTurn: the built-in claude hook mapping over one turn with a
// permission prompt and a compaction keeps the state right.
func TestFoldClaudeTurn(t *testing.T) {
	as, lerrs, err := adapter.Load(fsx.Root{Dir: t.TempDir()}, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 {
		t.Fatal(err, lerrs)
	}
	var events map[string]string
	for _, a := range as {
		if a.Name == "claude" {
			events = a.Hooks.Events
		}
	}
	c := conv.Conversation{ID: "C", State: conv.StateStarting, LastActivityAt: t0}
	steps := []struct{ hook, want string }{
		{"SessionStart", conv.StateIdle},
		{"UserPromptSubmit", conv.StateWorking},
		{"Notification", conv.StateWaiting},
		{"PostToolUse", conv.StateWorking},
		{"SessionStart", conv.StateWorking}, // compaction mid-turn
		{"PostToolUse", conv.StateWorking},
		{"Stop", conv.StateIdle},
		{"SessionEnd", conv.StateStopped},
	}
	for i, st := range steps {
		kind, ok := events[st.hook]
		if !ok {
			t.Fatalf("claude maps no %s", st.hook)
		}
		c = agents.Fold(c, agents.Event{TS: t0.Add(time.Duration(i+1) * time.Second), Kind: kind})
		if c.State != st.want {
			t.Fatalf("step %d %s (%s): state %q want %q", i, st.hook, kind, c.State, st.want)
		}
	}
}

// TestEventKindsSingleSource: every kind the adapter schema accepts is a
// kind Fold acts on from starting.
func TestEventKindsSingleSource(t *testing.T) {
	for _, k := range adapter.EventKinds {
		if !agents.ValidKind(k) {
			t.Errorf("%s not valid", k)
		}
		c := agents.Fold(conv.Conversation{ID: "C", State: conv.StateStarting}, agents.Event{TS: t0, Kind: k})
		if c.IgnoredEvents != 0 {
			t.Errorf("%s ignored from starting", k)
		}
	}
	if agents.ValidKind("bogus") {
		t.Error("bogus kind valid")
	}
}
