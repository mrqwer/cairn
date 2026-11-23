package agents_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
)

func get(t *testing.T, s conv.Store, id string) conv.Conversation {
	t.Helper()
	c, ok, err := s.Get(id)
	if err != nil || !ok {
		t.Fatalf("get %s: %v %v", id, ok, err)
	}
	return c
}

// holdRunning makes c a running conversation: state working and its
// conversation lock held (as a pane running the tool holds it).
func holdRunning(t *testing.T, s conv.Store, id string) func() {
	t.Helper()
	if err := s.Put(conv.Conversation{ID: id, Tool: "fake", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	rel, err := s.AcquireLock("fake", "cairn-"+id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rel)
	return rel
}

func TestAdmitNewAndLaunch(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	s := f.store
	q := agents.NewQueue(s, 1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Nothing running: a new conversation is admitted and stays starting.
	f.put(t, conv.Conversation{ID: "N1", State: conv.StateStarting})
	if queued, _, err := agents.AdmitNew(s, q, "N1", now); err != nil || queued {
		t.Fatalf("N1: %v %v", queued, err)
	}
	// Records that do not run a tool (no live lock) take no slot.
	if queued, _, _ := agents.AdmitNew(s, q, "N1", now); queued {
		t.Fatal("unlaunched record counted")
	}
	holdRunning(t, s, "R1")
	f.put(t, conv.Conversation{ID: "N2", State: conv.StateStarting})
	f.put(t, conv.Conversation{ID: "N3", State: conv.StateStarting})
	if queued, pos, err := agents.AdmitNew(s, q, "N2", now); err != nil || !queued || pos != 1 {
		t.Fatalf("N2: %v %d %v", queued, pos, err)
	}
	if queued, pos, _ := agents.AdmitNew(s, q, "N3", now.Add(time.Second)); !queued || pos != 2 {
		t.Fatalf("N3: %v %d", queued, pos)
	}
	if c := get(t, s, "N2"); c.State != conv.StateQueued {
		t.Fatalf("N2 state %q", c.State)
	}
	// At launch (lock held), N1 must wait too: R1 runs.
	if ok, pos, _ := agents.AdmitLaunch(s, q, "N1", now.Add(time.Minute), false); ok || pos != 3 {
		t.Fatalf("launch N1: %v %d", ok, pos)
	}
}

func TestPromoteOldestQueued(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	s := f.store
	q := agents.NewQueue(s, 1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rel := holdRunning(t, s, "R1")
	f.put(t, conv.Conversation{ID: "Q2", State: conv.StateQueued, LastActivityAt: now.Add(time.Minute)})
	f.put(t, conv.Conversation{ID: "Q1", State: conv.StateQueued, LastActivityAt: now})
	if _, ok, err := agents.Promote(s, q, now, -1); err != nil || ok {
		t.Fatalf("promoted while full: %v %v", ok, err)
	}
	rel()
	if _, err := s.Update("R1", func(c *conv.Conversation) error { c.State = conv.StateDone; return nil }); err != nil {
		t.Fatal(err)
	}
	p, ok, err := agents.Promote(s, q, now, -1)
	if err != nil || !ok || p.ID != "Q1" {
		t.Fatalf("promote %+v %v %v", p.ID, ok, err)
	}
	if get(t, s, "Q1").State != conv.StateStarting || get(t, s, "Q2").State != conv.StateQueued {
		t.Fatal("wrong states after promotion")
	}
}

func TestPaneWaitsWhileQueued(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	s := f.store
	holdRunning(t, s, "R1")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "waiter", State: conv.StateStarting})
	q := agents.NewQueue(s, 1)
	f.env.Queue = &q
	f.env.Now = true
	done := make(chan error, 1)
	go func() { done <- agents.RunPane(context.Background(), f.env, "C1") }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.out.String(), "queued") {
		if time.Now().After(deadline) {
			t.Fatalf("no queue notice:\n%s", f.out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if len(f.argvs(t)) != 0 {
		t.Fatal("tool started while the queue was full")
	}
	if c := get(t, s, "C1"); c.State != conv.StateQueued {
		t.Fatalf("state %q", c.State)
	}
	// The running agent finishes: the waiting pane starts its tool.
	if _, err := s.Update("R1", func(c *conv.Conversation) error { c.State = conv.StateDone; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("pane did not start after the slot freed:\n%s", f.out.String())
	}
	if len(f.argvs(t)) != 1 {
		t.Fatalf("tool runs %v", f.argvs(t))
	}
}

func TestQueueUnlimited(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	q := agents.NewQueue(f.store, 0)
	holdRunning(t, f.store, "R1")
	f.put(t, conv.Conversation{ID: "N1", State: conv.StateStarting})
	if queued, _, _ := agents.AdmitNew(f.store, q, "N1", time.Now()); queued {
		t.Fatal("queued without a limit")
	}
	if ok, _ := (task.Queue{Max: 1}).Admit([]conv.Conversation{{ID: "x", State: conv.StateWorking}}); ok {
		t.Fatal("a queue without Live counts every running state")
	}
}

func TestDequeueStopsWaitingPane(t *testing.T) {
	f := newPaneFixture(t, 0, "x")
	s := f.store
	holdRunning(t, s, "R1")
	f.put(t, conv.Conversation{ID: "C1", NativeID: uuid, Name: "waiter", State: conv.StateStarting})
	q := agents.NewQueue(s, 1)
	f.env.Queue = &q
	f.env.Now = true
	done := make(chan error, 1)
	go func() { done <- agents.RunPane(context.Background(), f.env, "C1") }()
	deadline := time.Now().Add(5 * time.Second)
	for get(t, s, "C1").State != conv.StateQueued {
		if time.Now().After(deadline) {
			t.Fatalf("not queued:\n%s", f.out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	ok, err := agents.Dequeue(s, "C1")
	if err != nil || !ok {
		t.Fatalf("dequeue %v %v", ok, err)
	}
	// The slot frees afterwards: the pane must not start the tool anyway.
	if _, err := s.Update("R1", func(c *conv.Conversation) error { c.State = conv.StateDone; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("pane kept waiting:\n%s", f.out.String())
	}
	if len(f.argvs(t)) != 0 {
		t.Fatalf("tool started after leaving the queue: %v", f.argvs(t))
	}
	if !strings.Contains(f.out.String(), "left the queue") {
		t.Fatalf("out:\n%s", f.out.String())
	}
	// Not queued: nothing to take out.
	if ok, _ := agents.Dequeue(s, "R1"); ok {
		t.Fatal("dequeued a conversation that was not queued")
	}
}

func TestPromotedPaneKeepsItsTurn(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	s := f.store
	q := agents.NewQueue(s, 1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rel := holdRunning(t, s, "R1")
	// Two panes wait in the queue, each holding its lock: X first.
	f.put(t, conv.Conversation{ID: "X", State: conv.StateQueued, LastActivityAt: now})
	f.put(t, conv.Conversation{ID: "W", State: conv.StateQueued, LastActivityAt: now.Add(time.Second)})
	for _, id := range []string{"X", "W"} {
		r, err := s.AcquireLock("fake", "cairn-"+id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r)
	}
	rel()
	if _, err := s.Update("R1", func(c *conv.Conversation) error { c.State = conv.StateDone; return nil }); err != nil {
		t.Fatal(err)
	}
	if p, ok, _ := agents.Promote(s, q, now.Add(time.Minute), -1); !ok || p.ID != "X" {
		t.Fatalf("promoted %q %v", p.ID, ok)
	}
	if ok, pos, err := agents.AdmitLaunch(s, q, "X", now.Add(2*time.Minute), true); err != nil || !ok {
		t.Fatalf("promoted X not admitted: %v %d %v (state %q)", ok, pos, err, get(t, s, "X").State)
	}
	if ok, _, _ := agents.AdmitLaunch(s, q, "W", now.Add(2*time.Minute), true); ok {
		t.Fatal("W overtook X")
	}
}

func TestAdmitNewLeavesLivePaneAlone(t *testing.T) {
	f := newPaneFixture(t, 0, "")
	s := f.store
	q := agents.NewQueue(s, 1)
	holdRunning(t, s, "R1")
	// The pane of S1 was admitted and runs its tool (lock held, starting).
	f.put(t, conv.Conversation{ID: "S1", State: conv.StateStarting})
	r, err := s.AcquireLock("fake", "cairn-S1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r)
	if queued, _, _ := agents.AdmitNew(s, q, "S1", time.Now()); queued || get(t, s, "S1").State != conv.StateStarting {
		t.Fatalf("a running conversation was queued: %q", get(t, s, "S1").State)
	}
}
