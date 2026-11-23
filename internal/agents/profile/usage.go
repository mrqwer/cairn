package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/execx"
)

// Usage sources.
const (
	// SourceNative is usage the tool reported into the record.
	SourceNative = "native"
	// SourceCcusage is usage read from ccusage.
	SourceCcusage = "ccusage"
	// SourceNone means ccusage ran but knows nothing of the conversation.
	SourceNone = "none"
	// SourceUnavailable means there is no source: ccusage is not installed
	// or its output could not be used.
	SourceUnavailable = "unavailable"
)

// UsageTimeout bounds one ccusage run.
const UsageTimeout = 30 * time.Second

// ccusageArgs asks for per-session totals as JSON with cached prices (no
// network).
var ccusageArgs = []string{"session", "--json", "--offline"}

// Usage is the token and cost total of one conversation. Costs are always
// estimates (Estimate is true): neither ccusage nor a tool's own count is a
// bill.
type Usage struct {
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Estimate bool    `json:"estimate"`
	Source   string  `json:"source"`
}

// Warning is a problem that leaves usage incomplete but is not a failure.
type Warning struct{ Msg string }

func (w *Warning) Error() string { return w.Msg }

// CollectUsage returns the usage of every conversation by id: the record's
// own usage when the tool reported it, else ccusage's (an optional tool,
// run once without a shell, bounded by UsageTimeout, matched by native
// id). A missing ccusage is not an error (Source "unavailable"); a failed
// run or unreadable output is a *Warning, and the map is still complete.
// Grouping is the caller's.
func CollectUsage(ctx context.Context, run execx.Runner, lookPath func(string) (string, error), convs []conv.Conversation) (map[string]Usage, error) {
	out := make(map[string]Usage, len(convs))
	need := false
	for _, c := range convs {
		if u := c.Usage; u != nil && (u.InputTokens+u.OutputTokens > 0 || u.CostUSD > 0) {
			out[c.ID] = Usage{Tokens: u.InputTokens + u.OutputTokens, CostUSD: u.CostUSD, Estimate: true, Source: SourceNative}
			continue
		}
		out[c.ID] = Usage{Estimate: true, Source: SourceUnavailable}
		need = need || c.NativeID != ""
	}
	if !need {
		return out, nil
	}
	bin, err := lookPath("ccusage")
	if err != nil || bin == "" {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, UsageTimeout)
	defer cancel()
	data, err := run.Run(ctx, bin, ccusageArgs...)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return out, &Warning{Msg: fmt.Sprintf("ccusage failed (%v); usage is unavailable", err)}
	}
	entries, ok := parseCcusage(data)
	if !ok {
		return out, &Warning{Msg: "ccusage printed no session JSON cairn can read; usage is unavailable"}
	}
	for _, c := range convs {
		if out[c.ID].Source == SourceNative {
			continue
		}
		u := Usage{Estimate: true, Source: SourceNone}
		if c.NativeID != "" {
			for _, e := range entries {
				if e.id == c.NativeID || strings.Contains(e.id, c.NativeID) {
					u.Tokens += e.tokens
					u.CostUSD += e.cost
					u.Source = SourceCcusage
				}
			}
		}
		out[c.ID] = u
	}
	return out, nil
}

type ccEntry struct {
	id     string
	tokens int64
	cost   float64
}

// parseCcusage reads the first JSON value of data that starts a line (a
// note printed before it is skipped): an array of sessions, or an object
// holding one under "sessions" or "data". Entries without a string id or
// with numbers that are not numbers are left out.
func parseCcusage(data []byte) ([]ccEntry, bool) {
	var v any
	found := false
	for off := 0; off < len(data) && !found; {
		line := data[off:]
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line = line[:i+1]
		}
		if t := bytes.TrimLeft(line, " \t\r"); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			dec := json.NewDecoder(bytes.NewReader(data[off:]))
			dec.UseNumber()
			found = dec.Decode(&v) == nil
		}
		off += len(line)
	}
	if !found {
		return nil, false
	}
	var list []any
	switch x := v.(type) {
	case []any:
		list = x
	case map[string]any:
		l, ok := x["sessions"].([]any)
		if !ok {
			if l, ok = x["data"].([]any); !ok {
				return nil, false
			}
		}
		list = l
	}
	var out []ccEntry
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if e, ok := readEntry(m); ok {
				out = append(out, e)
			}
		}
	}
	return out, true
}

// readEntry reads one session; ok is false when it has no string id or a
// number field holds something else.
func readEntry(m map[string]any) (ccEntry, bool) {
	var e ccEntry
	for _, k := range []string{"sessionId", "session_id", "sessionID", "id"} {
		if s, ok := m[k].(string); ok && s != "" {
			e.id = s
			break
		}
	}
	if e.id == "" {
		return e, false
	}
	// num returns the first of keys present; ok is false for a value that
	// is not a non-negative number.
	num := func(keys ...string) (f float64, present, ok bool) {
		for _, k := range keys {
			v, has := m[k]
			if !has {
				continue
			}
			n, isNum := v.(json.Number)
			if !isNum {
				return 0, true, false
			}
			f, err := n.Float64()
			if err != nil || f < 0 {
				return 0, true, false
			}
			return f, true, true
		}
		return 0, false, true
	}
	t, present, ok := num("totalTokens", "total_tokens")
	switch {
	case !ok:
		return e, false
	case present:
		e.tokens = int64(t)
	default:
		for _, ks := range [][]string{{"inputTokens", "input_tokens"}, {"outputTokens", "output_tokens"},
			{"cacheCreationTokens", "cache_creation_tokens"}, {"cacheReadTokens", "cache_read_tokens"}} {
			t, _, ok := num(ks...)
			if !ok {
				return e, false
			}
			e.tokens += int64(t)
		}
	}
	c, _, ok := num("totalCost", "costUSD", "cost_usd", "total_cost", "cost")
	if !ok {
		return e, false
	}
	e.cost = c
	return e, true
}
