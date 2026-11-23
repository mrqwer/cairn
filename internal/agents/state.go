package agents

import (
	"slices"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// Event is one line of <State>/agents/events/<id>.jsonl, posted by a tool
// hook through cairn event.
type Event struct {
	TS           time.Time `json:"ts"`
	Conversation string    `json:"conversation"`
	Kind         string    `json:"kind"`
	Reason       string    `json:"reason,omitempty"`
	NativeID     string    `json:"native_id,omitempty"`
}

// MaxIgnoredEvents caps IgnoredEvents ("at least this many"), so a tool
// that keeps posting events cairn ignores stops rewriting the record.
const MaxIgnoredEvents = 100

// KindActivity only records that the tool did something; KindReady says
// the tool is up and awaits input.
const (
	KindActivity = "activity"
	KindReady    = "ready"
)

// transitions is the table of spec §9.3: starting → working ⇄ waiting →
// idle → done | failed | stopped. working, waiting and idle reach each
// other (a new prompt, a permission request, a finished turn) and every live
// state may end. done, failed and stopped are left only by a new launch
// (back to starting), archived only by resume; neither comes from a hook.
var transitions = map[string]map[string]bool{
	conv.StateStarting: set(conv.StateWorking, conv.StateWaiting, conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped),
	conv.StateWorking:  set(conv.StateWaiting, conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped),
	conv.StateWaiting:  set(conv.StateWorking, conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped),
	conv.StateIdle:     set(conv.StateWorking, conv.StateWaiting, conv.StateDone, conv.StateFailed, conv.StateStopped),
}

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// ValidKind reports whether k is an event kind cairn event accepts
// (adapter.EventKinds).
func ValidKind(k string) bool { return slices.Contains(adapter.EventKinds, k) }

func archived(c conv.Conversation) bool {
	return c.ArchivedAt != nil || c.State == conv.StateArchived
}

// Fold applies one hook event to c. An accepted state event sets the state,
// state_source = hook and last_activity_at = the event time; waiting keeps
// the reason, other states clear it. A repeated state is accepted (it
// refreshes the time and reason). activity only moves last_activity_at.
// An event older than last_activity_at, of an unknown kind, on an archived
// conversation or not in the table is ignored and counted in IgnoredEvents.
// A native id from the event fills an unknown one only.
func Fold(c conv.Conversation, ev Event) conv.Conversation {
	ignore := func() conv.Conversation {
		if c.IgnoredEvents < MaxIgnoredEvents {
			c.IgnoredEvents++
		}
		return c
	}
	if archived(c) || ev.TS.Before(c.LastActivityAt) || !ValidKind(ev.Kind) {
		return ignore()
	}
	if c.NativeID == "" && ev.NativeID != "" && ValidNativeID(ev.NativeID) {
		c.NativeID = ev.NativeID
	}
	if ev.Kind == KindActivity {
		c.LastActivityAt = ev.TS
		return c
	}
	from := c.State
	if from == "" {
		from = conv.StateStarting
	}
	if ev.Kind == KindReady {
		if from != conv.StateStarting {
			return c // expected outside a start (resume, compaction): not counted
		}
		ev.Kind = conv.StateIdle
	}
	if from != ev.Kind && !transitions[from][ev.Kind] {
		return ignore()
	}
	c.State, c.StateSource, c.LastActivityAt = ev.Kind, conv.SourceHook, ev.TS
	c.WaitingReason = ""
	if ev.Kind == conv.StateWaiting {
		c.WaitingReason = ev.Reason
	}
	return c
}

// ObserveIdleAfter is how long a pane without output counts as idle when
// the state is observed rather than reported by hooks.
const ObserveIdleAfter = 2 * time.Minute

// Observe is the fallback for tools without (working) hooks, from what the
// pane shows: a dead pane is stopped (exit status 0) or failed, whatever
// the state source; for a live pane, idleFor ≥ ObserveIdleAfter makes
// starting or working idle, and fresh output makes starting or idle
// working. A live state reported by hooks is not overridden, and terminal
// or archived conversations are left alone. Changes set state_source =
// observed (lower confidence). The exit status is a deviation from the
// planned signature: a dead pane needs it to tell stopped from failed.
func Observe(c conv.Conversation, paneDead bool, exitStatus int, idleFor time.Duration, now time.Time) conv.Conversation {
	if archived(c) {
		return c
	}
	if paneDead {
		c.State, c.StateSource, c.WaitingReason = conv.StateStopped, conv.SourceObserved, ""
		if exitStatus != 0 {
			c.State = conv.StateFailed
		}
		c.LastActivityAt = now
		return c
	}
	if c.StateSource == conv.SourceHook {
		return c
	}
	state := c.State
	switch {
	case idleFor >= ObserveIdleAfter && (state == conv.StateStarting || state == conv.StateWorking || state == ""):
		state = conv.StateIdle
	case idleFor < ObserveIdleAfter && (state == conv.StateStarting || state == conv.StateIdle || state == ""):
		// last_activity_at is not moved: it orders hook events, and an
		// observed time must not make a just-posted hook event look stale.
		state = conv.StateWorking
	default:
		return c
	}
	c.State, c.StateSource = state, conv.SourceObserved
	return c
}
