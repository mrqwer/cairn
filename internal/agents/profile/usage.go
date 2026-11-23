package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/execx"
)

// Usage sources.
const (
	// SourceCcusage is usage read from ccusage.
	SourceCcusage = "ccusage"
	// SourceUnavailable means there is no figure; Reason says why.
	SourceUnavailable = "unavailable"
)

// Bounds of the ccusage queries.
const (
	// UsageTimeout bounds all queries of one CollectUsage.
	UsageTimeout = 30 * time.Second
	// UsageQueryTimeout bounds one query.
	UsageQueryTimeout = 10 * time.Second
	// MaxUsageOutput is the most output of one query that is read; more
	// makes that conversation unavailable.
	MaxUsageOutput = 1 << 20
)

// ccusageArgs is the per-conversation query. These flags (session --id,
// --json, --offline) are not verified against a real ccusage install; a
// version that differs degrades to "unavailable".
func ccusageArgs(nativeID string) []string {
	return []string{"session", "--id", nativeID, "--json", "--offline"}
}

// Usage is the token and cost total of one conversation. Costs are always
// estimates (Estimate is true). Reason is a one-line explanation when the
// usage is unavailable.
type Usage struct {
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Estimate bool    `json:"estimate"`
	Source   string  `json:"source"`
	Reason   string  `json:"reason,omitempty"`
}

// Warning is a problem that leaves usage incomplete but is not a failure.
type Warning struct{ Msg string }

func (w *Warning) Error() string { return w.Msg }

// CollectUsage returns the usage of every conversation by id, from ccusage
// (optional), queried once per conversation (the caller passes only
// conversations of tools whose sessions ccusage knows), without a shell,
// bounded in time and output. A result counts only when ccusage reports
// the session with exactly the conversation's native id; anything else is
// "unavailable" with a reason. A missing ccusage is not an error; a failed
// query or unreadable output is also returned as a *Warning (the first
// one), and the map is still complete.
func CollectUsage(ctx context.Context, run execx.Runner, lookPath func(string) (string, error), convs []conv.Conversation) (map[string]Usage, error) {
	out := make(map[string]Usage, len(convs))
	none := func(reason string) Usage { return Usage{Estimate: true, Source: SourceUnavailable, Reason: reason} }
	var todo []conv.Conversation
	for _, c := range convs {
		switch {
		case c.NativeID == "":
			out[c.ID] = none("no session id is known for this conversation")
		case !conv.ValidID(c.NativeID):
			out[c.ID] = none("the session id is not one ccusage can be asked for")
		default:
			out[c.ID] = none("not queried")
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		return out, nil
	}
	bin, err := lookPath("ccusage")
	if err != nil || bin == "" {
		for _, c := range todo {
			out[c.ID] = none("ccusage is not installed (it is optional)")
		}
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, UsageTimeout)
	defer cancel()
	var warn error
	for _, c := range todo {
		u, w := query(ctx, run, bin, c.NativeID)
		out[c.ID] = u
		if w != "" && warn == nil {
			warn = &Warning{Msg: "ccusage: " + w + "; usage is unavailable where it failed"}
		}
	}
	return out, warn
}

// query asks ccusage for one session; w is a warning for a failure (not
// for a session ccusage does not know).
func query(ctx context.Context, run execx.Runner, bin, id string) (Usage, string) {
	none := func(reason string) Usage { return Usage{Estimate: true, Source: SourceUnavailable, Reason: reason} }
	if ctx.Err() != nil {
		return none("ccusage took too long"), "it took too long"
	}
	qctx, cancel := context.WithTimeout(ctx, UsageQueryTimeout)
	defer cancel()
	data, err := run.Run(qctx, bin, ccusageArgs(id)...)
	switch {
	case err != nil && qctx.Err() != nil:
		return none("ccusage took too long"), "it took too long"
	case err != nil:
		return none("ccusage failed: " + oneLine(err.Error())), "it failed (" + oneLine(err.Error()) + ")"
	case len(data) > MaxUsageOutput:
		return none("ccusage printed more than 1 MiB"), "its output is too large"
	}
	e, ok, readable := findSession(data, id)
	switch {
	case !readable:
		return none("ccusage printed nothing cairn can read"), "its output is not session JSON"
	case !ok:
		return none(fmt.Sprintf("ccusage reported no session %s", id)), ""
	}
	return Usage{Tokens: e.tokens, CostUSD: e.cost, Estimate: true, Source: SourceCcusage}, ""
}

func oneLine(s string) string {
	if i := bytes.IndexAny([]byte(s), "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

type ccEntry struct {
	id     string
	tokens int64
	cost   float64
}

// findSession reads the first JSON value of data that starts a line (a
// note printed before it is skipped): one session object, a list of them,
// or an object with one under "sessions" or "data". ok is set only for a
// session whose id is exactly id; readable is false when no JSON value is
// found.
func findSession(data []byte, id string) (e ccEntry, ok, readable bool) {
	var v any
	for off := 0; off < len(data) && !readable; {
		line := data[off:]
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line = line[:i+1]
		}
		if t := bytes.TrimLeft(line, " \t\r"); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			dec := json.NewDecoder(bytes.NewReader(data[off:]))
			dec.UseNumber()
			readable = dec.Decode(&v) == nil
		}
		off += len(line)
	}
	if !readable {
		return e, false, false
	}
	var list []any
	switch x := v.(type) {
	case []any:
		list = x
	case map[string]any:
		if l, isList := x["sessions"].([]any); isList {
			list = l
		} else if l, isList := x["data"].([]any); isList {
			list = l
		} else {
			list = []any{x}
		}
	}
	for _, it := range list {
		if m, isObj := it.(map[string]any); isObj {
			if got, good := readEntry(m); good && got.id == id {
				return got, true, true
			}
		}
	}
	return e, false, true
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
