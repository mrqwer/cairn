// Package hooks passes cairn's event hooks to a tool, per process: a
// cairn-owned settings file or a command-line config override. The user's
// own tool settings are never read or written.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// hookTimeout bounds one hook run, in seconds; cairn event itself returns
// at once.
const hookTimeout = 10

// Dir is where the per-conversation settings files live.
func Dir(dirs xdg.Dirs) string { return dirs.State + "/agents/hooks" }

// EventArgv is the command a hook runs for kind.
func EventArgv(self, convID, kind string) []string {
	return []string{self, "event", "--conversation", convID, "--kind", kind}
}

// Inject returns the extra tool arguments that make the tool post its
// events to `cairn event` for conversation convID, self being the cairn
// executable. settings-file writes <State>/agents/hooks/<convID>.json (0600)
// and returns [flag, path]; config-override returns [flag, key=<argv>];
// none (or no hooks) returns nil, and the conversation's state then comes
// from observation only.
func Inject(a adapter.Adapter, convID, self string, dirs xdg.Dirs, root fsx.Root) ([]string, error) {
	if !conv.ValidID(convID) {
		return nil, fmt.Errorf("hooks: invalid conversation id %q", convID)
	}
	if err := checkSelf(self); err != nil {
		return nil, err
	}
	h := a.Hooks
	switch a.Capabilities.Hooks {
	case "settings-file":
		if h.Format != adapter.HookFormatClaudeSettings || h.Flag == "" || len(h.Events) == 0 {
			return nil, fmt.Errorf("hooks: adapter %s has an incomplete [hooks] section", a.Name)
		}
		data, err := claudeSettings(h.Events, self, convID)
		if err != nil {
			return nil, err
		}
		if err := root.MkdirAll(Dir(dirs), 0o700); err != nil {
			return nil, err
		}
		p := Dir(dirs) + "/" + convID + ".json"
		if err := root.WriteAtomic(p, data, 0o600); err != nil {
			return nil, err
		}
		return []string{h.Flag, root.Path(p)}, nil
	case "config-override":
		if h.Flag == "" || h.Key == "" || h.Kind == "" {
			return nil, fmt.Errorf("hooks: adapter %s has an incomplete [hooks] section", a.Name)
		}
		v, err := tomlArray(EventArgv(self, convID, h.Kind))
		if err != nil {
			return nil, err
		}
		return []string{h.Flag, h.Key + "=" + v}, nil
	}
	return nil, nil
}

// checkSelf refuses a cairn path that cannot be passed through a settings
// file and a shell intact.
func checkSelf(self string) error {
	if self == "" || !utf8.ValidString(self) || strings.IndexFunc(self, unicode.IsControl) >= 0 {
		return fmt.Errorf("hooks: unusable cairn executable path %q", self)
	}
	return nil
}

// shQuote makes s one shell word: the tool runs hook commands through sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroup struct {
	Hooks []hookCmd `json:"hooks"`
}

// claudeSettings renders format claude-settings@1. --stdin-json lets cairn
// event read the hook's JSON input for a waiting reason.
func claudeSettings(events map[string]string, self, convID string) ([]byte, error) {
	names := make([]string, 0, len(events))
	for n := range events {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string][]hookGroup{}
	for _, n := range names {
		kind := events[n]
		if !lowerWord(kind) {
			return nil, fmt.Errorf("hooks: event kind %q of %s is not a plain word", kind, n)
		}
		cmd := shQuote(self) + " event --conversation " + shQuote(convID) + " --kind " + kind + " --stdin-json"
		out[n] = []hookGroup{{Hooks: []hookCmd{{Type: "command", Command: cmd, Timeout: hookTimeout}}}}
	}
	return marshal(map[string]any{"hooks": out})
}

// lowerWord: a kind is put into the shell command unquoted.
func lowerWord(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

// tomlArray renders argv as a TOML inline array of basic strings (JSON
// string escapes are valid TOML).
func tomlArray(argv []string) (string, error) {
	b, err := marshal(argv)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(b)), nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, errors.New("hooks: " + err.Error())
	}
	return buf.Bytes(), nil
}

// EventsPath is the event log of conversation id under the store.
func EventsPath(s conv.Store, id string) string { return s.Dir + "/events/" + id + ".jsonl" }

// maxReason bounds a waiting reason, in runes, so an event line stays small
// enough for one append write.
const maxReason = 200

// CleanReason makes a hook-supplied reason one short printable line.
func CleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxReason {
		s = string(r[:maxReason-1]) + "…"
	}
	return s
}

// Post records one hook event under the store lock (waiting at most wait
// for it): it appends ev as one line to the conversation's event log (a
// single O_APPEND write) and folds it into the record, which is rewritten
// only when something changed. An invalid id or a conversation that does
// not exist is an error and nothing is written.
//
// When the lock stays taken the result is conv.ErrBusy: the event is still
// logged and kept in a pending file, which the next event of the
// conversation folds first (in time order).
func Post(s conv.Store, ev agents.Event, wait time.Duration) (conv.Conversation, error) {
	if !conv.ValidID(ev.Conversation) {
		return conv.Conversation{}, fmt.Errorf("invalid conversation id %q", ev.Conversation)
	}
	if !agents.ValidKind(ev.Kind) {
		return conv.Conversation{}, fmt.Errorf("unknown event kind %q", ev.Kind)
	}
	ev.Reason = CleanReason(ev.Reason)
	if !agents.ValidNativeID(ev.NativeID) {
		ev.NativeID = ""
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return conv.Conversation{}, err
	}
	line = append(line, '\n')
	logPath := s.Root.Path(EventsPath(s, ev.Conversation))
	pending := logPath + ".pending"
	c, err := s.TryUpdate(ev.Conversation, wait, func(c *conv.Conversation) error {
		if err := appendLine(s, logPath, line); err != nil {
			return err
		}
		rotate(logPath)
		before := *c
		for _, p := range drain(pending) {
			*c = agents.Fold(*c, p)
		}
		*c = agents.Fold(*c, ev)
		if reflect.DeepEqual(before, *c) {
			return conv.ErrNoChange
		}
		return nil
	})
	if errors.Is(err, conv.ErrBusy) {
		// Read without the lock: keep the event only for a conversation
		// that exists.
		if cs, serr := s.Scan(func(r conv.Conversation) bool { return r.ID == ev.Conversation }); serr == nil && len(cs) == 1 {
			_ = appendLine(s, logPath, line)
			_ = appendLine(s, pending, line)
		}
	}
	return c, err
}

// appendLine appends one line with a single O_APPEND write (0600).
func appendLine(s conv.Store, path string, line []byte) error {
	if err := s.Root.MkdirAll(s.Dir+"/events", 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// drain takes the pending events (caller holds the store lock), oldest
// first. The file is moved aside before reading, so an event added at the
// same time lands in a new pending file for the next drain.
func drain(pending string) []agents.Event {
	taken := pending + ".taken"
	if os.Rename(pending, taken) != nil {
		return nil
	}
	b, err := os.ReadFile(taken)
	_ = os.Remove(taken)
	if err != nil {
		return nil
	}
	var out []agents.Event
	for _, l := range bytes.Split(b, []byte("\n")) {
		var e agents.Event
		if len(bytes.TrimSpace(l)) > 0 && json.Unmarshal(l, &e) == nil && agents.ValidKind(e.Kind) {
			e.Reason = CleanReason(e.Reason)
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// MaxEventLog is the size at which an event log is moved to <log>.1 (the
// one older generation kept); the records hold the folded state, the log
// is for inspection.
const MaxEventLog = 1 << 20

// rotate moves an oversized log aside. It runs under the store lock, so
// only one poster rotates; a writer still holding the old file appends to
// <log>.1, which loses nothing.
func rotate(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > MaxEventLog {
		_ = os.Rename(path, path+".1")
	}
}
