package task

import (
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

func at(min int) time.Time { return time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC) }

func TestQueueAdmitPositions(t *testing.T) {
	live := map[string]bool{"a": true, "b": true, "c": true, "q1": true}
	q := Queue{Max: 2, Live: func(c conv.Conversation) bool { return live[c.ID] }}
	cs := []conv.Conversation{
		{ID: "a", State: conv.StateWorking},
		{ID: "d", State: conv.StateWorking}, // no live pane: not counted
		{ID: "e", State: conv.StateIdle},    // idle does not count
	}
	if ok, pos := q.Admit(cs); !ok || pos != 0 {
		t.Fatalf("one running: %v %d", ok, pos)
	}
	cs = append(cs, conv.Conversation{ID: "b", State: conv.StateWaiting})
	if ok, pos := q.Admit(cs); ok || pos != 1 {
		t.Fatalf("full: %v %d", ok, pos)
	}
	cs = append(cs,
		conv.Conversation{ID: "q2", State: conv.StateQueued, LastActivityAt: at(2)},
		conv.Conversation{ID: "q1", State: conv.StateQueued, LastActivityAt: at(1)})
	if ok, pos := q.Admit(cs); ok || pos != 3 {
		t.Fatalf("full with two queued: %v %d", ok, pos)
	}
	if p := q.Position(cs, "q1"); p != 1 {
		t.Fatalf("q1 position %d", p)
	}
	if p := q.Position(cs, "q2"); p != 2 {
		t.Fatalf("q2 position %d", p)
	}
	if _, ok := q.Next(cs); ok {
		t.Fatal("next while full")
	}
	// A slot frees: the oldest queued one with a live pane goes first.
	cs[0].State = conv.StateDone
	if n, ok := q.Next(cs); !ok || n.ID != "q1" {
		t.Fatalf("next %v %v", n.ID, ok)
	}
	// Starting counts as running.
	cs = append(cs, conv.Conversation{ID: "c", State: conv.StateStarting})
	if ok, _ := q.Admit(cs); ok {
		t.Fatal("starting not counted")
	}
	// No limit.
	if ok, _ := (Queue{}).Admit(cs); !ok {
		t.Fatal("unlimited queue refused")
	}
}
