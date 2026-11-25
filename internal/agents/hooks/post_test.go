package hooks_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/hooks"
	"github.com/mrqwer/cairn/internal/fsx"
)

func postStore(t *testing.T) (conv.Store, string) {
	t.Helper()
	root := t.TempDir()
	return conv.Store{Dir: "/state/agents", Root: fsx.Root{Dir: root}}, root
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		n++
	}
	return n
}

func ev(kind string) agents.Event {
	return agents.Event{TS: time.Now().UTC(), Conversation: "C1", Kind: kind}
}

func TestPostUnknownConversationWritesNothing(t *testing.T) {
	s, root := postStore(t)
	if _, err := hooks.Post(s, ev("working"), time.Second); !errors.Is(err, conv.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state/agents/events")); err == nil {
		t.Fatal("events written for an unknown conversation")
	}
}

// TestPostIgnoredCountSaturates: a stream of ignored events stops
// rewriting the record once IgnoredEvents reaches its cap.
func TestPostIgnoredCountSaturates(t *testing.T) {
	s, root := postStore(t)
	if err := s.Put(conv.Conversation{ID: "C1", State: conv.StateDone}); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "state/agents/conversations.jsonl")
	for i := 0; i < agents.MaxIgnoredEvents; i++ {
		if _, err := hooks.Post(s, ev("working"), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(data)
	for i := 0; i < 20; i++ {
		c, err := hooks.Post(s, ev("working"), time.Second)
		if err != nil || c.IgnoredEvents != agents.MaxIgnoredEvents || c.State != conv.StateDone {
			t.Fatalf("%+v %v", c, err)
		}
	}
	if after, _ := os.ReadFile(data); string(after) != string(before) {
		t.Fatal("saturated ignored events still rewrite the record")
	}
	if n := countLines(t, root+"/state/agents/events/C1.jsonl"); n != agents.MaxIgnoredEvents+20 {
		t.Fatalf("event log has %d lines", n)
	}
}

func TestPostArchivedByOverlayIgnored(t *testing.T) {
	s, _ := postStore(t)
	if err := s.Put(conv.Conversation{ID: "C1", State: conv.StateIdle}); err != nil {
		t.Fatal(err)
	}
	arch := time.Now().UTC()
	if err := s.WriteCurated("C1", conv.Curated{ArchivedAt: &arch}); err != nil {
		t.Fatal(err)
	}
	c, err := hooks.Post(s, ev("working"), time.Second)
	if err != nil || c.State != conv.StateIdle || c.IgnoredEvents != 1 {
		t.Fatalf("%+v %v", c, err)
	}
}

// TestPostBusyIsAppliedByTheNextEvent: an event that could not take the
// store lock is kept and folded, in time order, by the next event.
func TestPostBusyIsAppliedByTheNextEvent(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock binary")
	}
	s, root := postStore(t)
	if err := s.Put(conv.Conversation{ID: "C1", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")
	cmd := exec.Command("flock", filepath.Join(root, "state/agents/.store.lock"), "sh", "-c",
		"touch "+ready+"; while [ ! -e "+release+" ]; do sleep 0.01; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flock never held")
		}
	}
	busy := ev("waiting")
	busy.Reason = "permission"
	if _, err := hooks.Post(s, busy, 50*time.Millisecond); !errors.Is(err, conv.ErrBusy) {
		t.Fatalf("err %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	c, err := hooks.Post(s, ev("activity"), time.Second)
	if err != nil || c.State != conv.StateWaiting || c.WaitingReason != "permission" {
		t.Fatalf("busy event not applied: %+v %v", c, err)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "state/agents/events"))
	if len(ents) != 1 || ents[0].Name() != "C1.jsonl" {
		t.Fatalf("pending events left behind: %v", ents)
	}
	if n := countLines(t, root+"/state/agents/events/C1.jsonl"); n != 2 {
		t.Fatalf("event log has %d lines", n)
	}
}
