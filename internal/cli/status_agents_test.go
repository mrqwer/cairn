package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// putLive stores c and, when live, holds its conversation lock the way a
// running pane does.
func putLive(t testing.TB, s conv.Store, c conv.Conversation, live bool) {
	t.Helper()
	if c.Tool == "" {
		c.Tool = "fake"
	}
	if err := s.Put(c); err != nil {
		t.Fatal(err)
	}
	if live {
		rel, err := s.AcquireLock(c.Tool, "cairn-"+c.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(rel)
	}
}

func noTmux(t *testing.T) {
	t.Helper()
	old := newDriver
	t.Cleanup(func() { newDriver = old })
	newDriver = func(string) (wsDriver, error) {
		t.Fatal("status --tmux must not talk to tmux")
		return nil, errNoTmux
	}
}

func TestStatusTmuxFormat(t *testing.T) {
	dir := hostTestEnv(t)
	noTmux(t)
	out, _, err := runSplit(t, "status", "--tmux")
	if err != nil || out != "" {
		t.Fatalf("no conversations: %q %v", out, err)
	}
	s := testStore(dir)
	arch := time.Now()
	putLive(t, s, conv.Conversation{ID: "01A", State: conv.StateIdle}, true)
	out, _, _ = runSplit(t, "status", "--tmux")
	if out != "" {
		t.Fatalf("idle only must print nothing: %q", out)
	}
	putLive(t, s, conv.Conversation{ID: "01B", State: conv.StateWaiting}, true)
	putLive(t, s, conv.Conversation{ID: "01C", State: conv.StateWaiting}, true)
	for _, id := range []string{"01D", "01E", "01F"} {
		putLive(t, s, conv.Conversation{ID: id, State: conv.StateWorking}, true)
	}
	// not counted: no running tool (stale after a crash or reboot), archived, stopped
	putLive(t, s, conv.Conversation{ID: "01G", State: conv.StateWaiting}, false)
	putLive(t, s, conv.Conversation{ID: "01H", State: conv.StateWorking, ArchivedAt: &arch}, true)
	putLive(t, s, conv.Conversation{ID: "01I", State: conv.StateStopped}, false)
	out, _, err = runSplit(t, "status", "--tmux")
	if err != nil || out != "2 waiting · 3 working\n" {
		t.Fatalf("got %q %v", out, err)
	}
	putLive(t, s, conv.Conversation{ID: "01J", State: conv.StateWorking}, true)
	if err := s.Put(conv.Conversation{ID: "01B", Tool: "fake", State: conv.StateIdle}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conv.Conversation{ID: "01C", Tool: "fake", State: conv.StateIdle}); err != nil {
		t.Fatal(err)
	}
	if out, _, _ = runSplit(t, "status", "--tmux"); out != "4 working\n" {
		t.Fatalf("got %q", out)
	}
}

func TestStatusTmuxQuietOnBrokenStore(t *testing.T) {
	t.Setenv("HOME", "relative")
	noTmux(t)
	out, _, err := runSplit(t, "status", "--tmux")
	if err != nil || out != "" {
		t.Fatalf("a status line must stay empty on errors: %q %v", out, err)
	}
}

func TestStatusJSONAgents(t *testing.T) {
	dir, _ := wsTestEnv(t)
	s := testStore(dir)
	putLive(t, s, conv.Conversation{ID: "01A", Name: "fix login", Project: "demo", Task: "t1",
		State: conv.StateWaiting, WaitingReason: "permission: Bash"}, true)
	putLive(t, s, conv.Conversation{ID: "01B", Name: "docs", Project: "api", State: conv.StateWorking}, true)
	putLive(t, s, conv.Conversation{ID: "01C", Name: "old", State: conv.StateWaiting}, false)
	putLive(t, s, conv.Conversation{ID: "01D", Name: "idle", State: conv.StateIdle}, true)

	out := mustRun(t, "", "status", "--json")
	var got struct {
		Projects []map[string]any `json:"projects"`
		Agents   struct {
			Counts  map[string]int `json:"counts"`
			Waiting []struct {
				ID, Name, Project, Task, Reason string
			} `json:"waiting"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.Projects == nil {
		t.Fatalf("workspace status lost:\n%s", out)
	}
	want := map[string]int{"waiting": 1, "working": 1, "idle": 1, "stopped": 1}
	if !reflect.DeepEqual(got.Agents.Counts, want) {
		t.Fatalf("counts %v want %v", got.Agents.Counts, want)
	}
	w := got.Agents.Waiting
	if len(w) != 1 || w[0].ID != "01A" || w[0].Name != "fix login" || w[0].Project != "demo" || w[0].Task != "t1" ||
		w[0].Reason != "permission: Bash" {
		t.Fatalf("waiting %+v\n%s", w, out)
	}

	plain := mustRun(t, "", "status")
	if !strings.Contains(plain, "no active projects") || !strings.Contains(plain, "agents: 1 waiting · 1 working") ||
		!strings.Contains(plain, "fix login") || !strings.Contains(plain, "permission: Bash") {
		t.Fatalf("plain status:\n%s", plain)
	}
}

func TestStatusJSONAgentsEmpty(t *testing.T) {
	wsTestEnv(t)
	out := mustRun(t, "", "status", "--json")
	if !strings.Contains(out, `"counts": {}`) || !strings.Contains(out, `"waiting": []`) {
		t.Fatalf("empty agents must still be objects and arrays:\n%s", out)
	}
	if plain := mustRun(t, "", "status"); strings.Contains(plain, "agents:") {
		t.Fatalf("no conversations, no agents line:\n%s", plain)
	}
}

func seedStatusRecords(tb testing.TB, s conv.Store) {
	tb.Helper()
	for i := 0; i < 200; i++ {
		state := []string{conv.StateWorking, conv.StateWaiting, conv.StateIdle, conv.StateStopped}[i%4]
		c := conv.Conversation{ID: fmt.Sprintf("01S%03d", i), Name: fmt.Sprintf("c%d", i), Project: "demo",
			State: state, WaitingReason: "r", LaunchArgv: []string{"fake", "start", "x"}}
		putLive(tb, s, c, state != conv.StateStopped)
		if i%2 == 0 {
			if err := s.WriteCurated(c.ID, conv.Curated{Name: "n", Tags: []string{"a"}}); err != nil {
				tb.Fatal(err)
			}
		}
	}
}

// TestStatusTmuxFast: the tmux status line reads records only; with 200
// conversations it stays under 10 ms (asserted at 50 ms for slow CI).
func TestStatusTmuxFast(t *testing.T) {
	dir := hostTestEnv(t)
	noTmux(t)
	seedStatusRecords(t, testStore(dir))
	best := time.Hour
	for i := 0; i < 10; i++ {
		start := time.Now()
		out, _, err := runSplit(t, "status", "--tmux")
		d := time.Since(start)
		if err != nil || out != "50 waiting · 50 working\n" {
			t.Fatalf("got %q %v", out, err)
		}
		best = min(best, d)
	}
	t.Logf("status --tmux with 200 records: %v", best)
	if best > 50*time.Millisecond {
		t.Fatalf("status --tmux took %v", best)
	}
}

func BenchmarkStatusTmux(b *testing.B) {
	dir := b.TempDir()
	b.Setenv("HOME", dir)
	b.Setenv("XDG_STATE_HOME", dir+"/state")
	b.Setenv("XDG_CONFIG_HOME", dir+"/config")
	b.Setenv("XDG_RUNTIME_DIR", dir+"/run")
	seedStatusRecords(b, testStore(dir))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cmd := NewRootCmd()
		var o strings.Builder
		cmd.SetOut(&o)
		cmd.SetArgs([]string{"status", "--tmux"})
		if err := cmd.Execute(); err != nil || o.String() == "" {
			b.Fatalf("%q %v", o.String(), err)
		}
	}
}
