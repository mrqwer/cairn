package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/hooks"
)

// Bounds of one cairn event run: a hook must never stall its tool.
const (
	eventLockWait  = 2 * time.Second
	eventStdinWait = time.Second
	eventStdinMax  = 64 << 10
)

// newEventCmd is "cairn event", run by tool hooks. It never fails and never
// prints to stdout (a tool may read a hook's stdout as input): problems are
// one line on stderr and exit status 0, so a hook never breaks the tool.
func newEventCmd() *cobra.Command {
	var id, kind, reason, nativeID string
	var stdinJSON bool
	cmd := &cobra.Command{
		Use:   "event --conversation <id> --kind <kind>",
		Short: "Record a state event of an agent conversation (run by tool hooks)",
		Long: "Appends the event to <state>/agents/events/<id>.jsonl and updates the conversation's state.\n" +
			"Kinds: working, waiting, idle, done, failed, stopped, ready, activity. Extra arguments are ignored.\n" +
			"Always exits 0; problems are reported on stderr.",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			note := func(format string, a ...any) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cairn event: "+format+"\n", a...)
			}
			if id == "" {
				note("--conversation is required")
				return nil
			}
			if stdinJSON && !interactive(cmd.InOrStdin()) {
				msg, sid := readHookInput(cmd.InOrStdin())
				if reason == "" {
					reason = msg
				}
				if nativeID == "" {
					nativeID = sid
				}
			}
			dirs, err := stateDirs()
			if err != nil {
				note("%v", err)
				return nil
			}
			ev := agents.Event{TS: time.Now().UTC(), Conversation: id, Kind: kind, Reason: reason, NativeID: nativeID}
			s := agentStore(dirs)
			if _, err := hooks.Post(s, ev, eventLockWait); err != nil {
				note("%v", err)
				return nil
			}
			// A conversation that stopped taking a slot lets the oldest
			// queued one start.
			switch kind {
			case conv.StateIdle, conv.StateDone, conv.StateFailed, conv.StateStopped:
				if _, _, err := agents.Promote(s, agentQueue(dirs, s, nil), time.Now().UTC(), eventLockWait); err != nil {
					note("queue: %v", err)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&id, "conversation", "", "conversation id")
	f.StringVar(&kind, "kind", "", "working | waiting | idle | done | failed | stopped | ready | activity")
	f.StringVar(&reason, "reason", "", "why the conversation waits")
	f.StringVar(&nativeID, "native-id", "", "the tool's own id of the conversation (fills an unknown one)")
	f.BoolVar(&stdinJSON, "stdin-json", false, "read the hook's JSON input (message, session_id) from stdin")
	return cmd
}

// readHookInput reads a hook's JSON input, bounded in size and time, and
// returns its message and session id; anything unreadable gives "".
func readHookInput(in io.Reader) (message, sessionID string) {
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(in, eventStdinMax))
		// Drain the rest (a tool's output can be large) so the tool's
		// write to the hook does not fail.
		_, _ = io.Copy(io.Discard, in)
		done <- b
	}()
	var b []byte
	select {
	case b = <-done:
	case <-time.After(eventStdinWait):
		return "", ""
	}
	var v struct {
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(b, &v) != nil {
		return "", ""
	}
	return v.Message, v.SessionID
}
