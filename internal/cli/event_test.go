package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// runEvent runs cairn with stdin and separate stdout and stderr; it is
// safe to call from several goroutines.
func runEvent(stdin string, args ...string) (stdout, stderr string, err error) {
	cmd := NewRootCmd()
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.Execute()
	return o.String(), e.String(), err
}

func eventsFile(dir, id string) string {
	return filepath.Join(dir, "state/cairn/agents/events", id+".jsonl")
}

type eventLine struct {
	TS           time.Time `json:"ts"`
	Conversation string    `json:"conversation"`
	Kind         string    `json:"kind"`
	Reason       string    `json:"reason"`
}

func readEvents(t *testing.T, path string) []eventLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []eventLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev eventLine
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("broken line %q: %v", sc.Text(), err)
		}
		out = append(out, ev)
	}
	return out
}

func TestEventFlipsStateAndAppends(t *testing.T) {
	dir := hostTestEnv(t)
	s := testStore(dir)
	if err := s.Put(conv.Conversation{ID: "01EV", Tool: "fake", State: conv.StateWorking, StateSource: conv.SourceObserved}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runEvent("", "event", "--conversation", "01EV", "--kind", "waiting", "--reason", "permission: Bash")
	if err != nil || out != "" {
		t.Fatalf("err %v stdout %q", err, out)
	}
	c, _, _ := s.Get("01EV")
	if c.State != conv.StateWaiting || c.StateSource != conv.SourceHook || c.WaitingReason != "permission: Bash" {
		t.Fatalf("record %+v", c)
	}
	evs := readEvents(t, eventsFile(dir, "01EV"))
	if len(evs) != 1 || evs[0].Conversation != "01EV" || evs[0].Kind != "waiting" || evs[0].Reason != "permission: Bash" || evs[0].TS.IsZero() {
		t.Fatalf("events %+v", evs)
	}
	if fi, _ := os.Stat(eventsFile(dir, "01EV")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("events file mode %v", fi.Mode())
	}
}

func TestEventReadsHookJSONFromStdin(t *testing.T) {
	dir := hostTestEnv(t)
	s := testStore(dir)
	if err := s.Put(conv.Conversation{ID: "01EV", Tool: "fake", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	in := `{"session_id":"abc","hook_event_name":"Notification","message":"needs your permission to use Bash\n\u001b[31m"}`
	if _, _, err := runEvent(in, "event", "--conversation", "01EV", "--kind", "waiting", "--stdin-json"); err != nil {
		t.Fatal(err)
	}
	c, _, _ := s.Get("01EV")
	if c.WaitingReason != "needs your permission to use Bash [31m" || c.NativeID != "abc" {
		t.Fatalf("record %+v", c)
	}
}

func TestEventUnknownConversationExitsZero(t *testing.T) {
	dir := hostTestEnv(t)
	cases := [][]string{
		{"event", "--conversation", "01NOPE", "--kind", "waiting"},
		{"event", "--conversation", "../../etc", "--kind", "waiting"},
		{"event", "--conversation", "-x", "--kind", "waiting"},
		{"event", "--kind", "waiting"},
		{"event", "--conversation", "01NOPE", "--kind", "sleeping"},
		{"event", "--conversation", "01NOPE", "--kind", "idle", "--bogus-flag", "{\"type\":\"agent-turn-complete\"}"},
	}
	for _, args := range cases {
		out, errOut, err := runEvent("", args...)
		if err != nil || out != "" {
			t.Fatalf("%q: err %v stdout %q", args, err, out)
		}
		if errOut == "" {
			t.Errorf("%q: no note on stderr", args)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "state/cairn/agents/events")); len(ents) != 0 {
		t.Fatalf("events written for unknown conversations: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(dir, "etc.jsonl")); err == nil {
		t.Fatal("path traversal")
	}
}

func TestEventIgnoresExtraArgs(t *testing.T) {
	dir := hostTestEnv(t)
	s := testStore(dir)
	if err := s.Put(conv.Conversation{ID: "01EV", Tool: "codex", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	// codex notify appends a JSON payload argument.
	if _, _, err := runEvent("", "event", "--conversation", "01EV", "--kind", "idle", `{"type":"agent-turn-complete"}`); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := s.Get("01EV"); c.State != conv.StateIdle {
		t.Fatalf("record %+v", c)
	}
}

func TestEventConcurrentAppends(t *testing.T) {
	dir := hostTestEnv(t)
	s := testStore(dir)
	if err := s.Put(conv.Conversation{ID: "01EV", Tool: "fake", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := "working"
			if i%2 == 1 {
				kind = "waiting"
			}
			_, _, err := runEvent("", "event", "--conversation", "01EV", "--kind", kind,
				"--reason", strings.Repeat("r", 100+i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	evs := readEvents(t, eventsFile(dir, "01EV"))
	if len(evs) != 32 {
		t.Fatalf("want 32 lines, got %d", len(evs))
	}
	seen := map[int]bool{}
	for _, ev := range evs {
		seen[len(ev.Reason)] = true
	}
	if len(seen) != 32 {
		t.Fatalf("lines interleaved or lost: %d distinct", len(seen))
	}
	c, _, _ := s.Get("01EV")
	if c.State != conv.StateWorking && c.State != conv.StateWaiting {
		t.Fatalf("record %+v", c)
	}
}

// TestEventWithoutRuntimeDir: hooks and the tmux status line may run with
// a reduced environment; neither needs XDG_RUNTIME_DIR.
func TestEventWithoutRuntimeDir(t *testing.T) {
	dir := hostTestEnv(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	s := testStore(dir)
	putLive(t, s, conv.Conversation{ID: "01EV", State: conv.StateWorking}, true)
	if _, errOut, err := runEvent("", "event", "--conversation", "01EV", "--kind", "waiting"); err != nil || errOut != "" {
		t.Fatalf("%v %s", err, errOut)
	}
	if c, _, _ := s.Get("01EV"); c.State != conv.StateWaiting {
		t.Fatalf("record %+v", c)
	}
	if out, _, _ := runEvent("", "status", "--tmux"); out != "1 waiting\n" {
		t.Fatalf("status --tmux: %q", out)
	}
}

func TestEventLogRotates(t *testing.T) {
	dir := hostTestEnv(t)
	s := testStore(dir)
	if err := s.Put(conv.Conversation{ID: "01EV", Tool: "fake", State: conv.StateWorking}); err != nil {
		t.Fatal(err)
	}
	p := eventsFile(dir, "01EV")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte(`{"kind":"activity"}`+"\n"), (1<<20)/20+1)
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runEvent("", "event", "--conversation", "01EV", "--kind", "waiting"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() <= int64(len(big)) {
		t.Fatalf("old log not kept: %v %v", fi, err)
	}
	if _, _, err := runEvent("", "event", "--conversation", "01EV", "--kind", "working"); err != nil {
		t.Fatal(err)
	}
	if evs := readEvents(t, p); len(evs) != 1 || evs[0].Kind != "working" {
		t.Fatalf("new log %+v", evs)
	}
}

// TestEventDrainsLargeHookInput: a hook's input larger than what is parsed
// (a tool result) is read to the end, and the event still applies.
func TestEventDrainsLargeHookInput(t *testing.T) {
	dir := hostTestEnv(t)
	s := testStore(dir)
	if err := s.Put(conv.Conversation{ID: "01EV", Tool: "fake", State: conv.StateWaiting}); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"tool_response":"` + strings.Repeat("x", 300<<10) + `"}`)
	cmd := NewRootCmd()
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetIn(in)
	cmd.SetArgs([]string{"event", "--conversation", "01EV", "--kind", "working", "--stdin-json"})
	if err := cmd.Execute(); err != nil || o.Len() != 0 {
		t.Fatalf("%v %q", err, o.String())
	}
	if in.Len() != 0 {
		t.Fatalf("%d bytes of hook input left unread", in.Len())
	}
	if c, _, _ := s.Get("01EV"); c.State != conv.StateWorking {
		t.Fatalf("record %+v", c)
	}
}
