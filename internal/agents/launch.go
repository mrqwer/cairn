// Package agents launches and supervises AI tool conversations: argv for
// start and resume, discovery of a tool-assigned id, and the pane wrapper.
package agents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

// PaneArgv is the command of an agent pane: the wrapper that shows the
// conversation and starts its tool lazily.
func PaneArgv(self, convID string) []string {
	return []string{self, "agents", "pane", convID}
}

// Discovery says how the native id of a just-started conversation is found.
type Discovery struct {
	Mode string // dir-diff | list | state-file
}

// NewNativeID returns a random RFC 4122 version 4 UUID, the id cairn
// pre-assigns to tools that accept one.
func NewNativeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("agents: random source failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func vars(c conv.Conversation, profileArgs []string) adapter.Vars {
	// {id} is the tool-side id: cairn's own ULID is never given to a tool.
	return adapter.Vars{ID: c.NativeID, NativeID: c.NativeID, Name: c.Name, Cwd: c.Cwd, ProfileArgs: profileArgs}
}

// BuildStart returns the argv that starts conversation c for the first time.
// A tool that pre-assigns ids needs c.NativeID set (see NewNativeID). For a
// tool that neither pre-assigns an id nor takes a name, the returned
// Discovery tells the caller how to find the id after launch.
func BuildStart(a adapter.Adapter, c conv.Conversation, profileArgs []string) ([]string, *Discovery, error) {
	if a.Capabilities.PreassignID && c.NativeID == "" {
		return nil, nil, fmt.Errorf("adapter %s pre-assigns ids but conversation %s has no native id", a.Name, c.ID)
	}
	argv, err := a.Render("start", vars(c, profileArgs))
	if err != nil {
		return nil, nil, err
	}
	var d *Discovery
	if !a.Capabilities.PreassignID {
		switch a.Capabilities.Discovery {
		case "dir-diff", "list", "state-file":
			d = &Discovery{Mode: a.Capabilities.Discovery}
		}
	}
	return argv, d, nil
}

// BuildResume returns the argv that resumes c: by native id, or by name when
// the tool resumes by name and the id is unknown.
func BuildResume(a adapter.Adapter, c conv.Conversation, profileArgs []string) ([]string, error) {
	switch {
	case c.NativeID != "" && a.Capabilities.ResumeByID:
	case c.NativeID == "" && a.Capabilities.ResumeByName && c.Name != "":
	default:
		return nil, fmt.Errorf("conversation %s cannot be resumed with %s: native id unknown", c.ID, a.Name)
	}
	return a.Render("resume", vars(c, profileArgs))
}

// BuildDelete returns the argv of the tool's native delete of c. It is not
// undoable; callers confirm first.
func BuildDelete(a adapter.Adapter, c conv.Conversation) ([]string, error) {
	if !a.Capabilities.Delete {
		return nil, fmt.Errorf("%s has no native delete (capabilities.delete is off)", a.Name)
	}
	if !ValidNativeID(c.NativeID) {
		return nil, fmt.Errorf("conversation %s has no usable native id to delete (%q)", c.ID, c.NativeID)
	}
	return a.Render("delete", vars(c, nil))
}

// ValidNativeID reports whether a tool's conversation id is safe to put in a
// path, a glob and an argv element: 1-128 characters of [A-Za-z0-9._:-],
// not starting with '-' or '.', without "..".
func ValidNativeID(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '-' || s[0] == '.' || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// TranscriptPaths returns the transcript files of nativeID found through
// the adapter's history.locator ("~/" is home).
func TranscriptPaths(root fsx.Root, home string, a adapter.Adapter, nativeID string) ([]string, error) {
	if a.History.Locator == "" {
		return nil, fmt.Errorf("adapter %s declares no history.locator", a.Name)
	}
	if !ValidNativeID(nativeID) {
		return nil, fmt.Errorf("native id %q is not usable", nativeID)
	}
	loc := a.History.Locator
	if strings.HasPrefix(loc, "~/") {
		loc = filepath.Join(home, loc[2:])
	}
	return filepath.Glob(root.Path(strings.ReplaceAll(loc, "{native_id}", nativeID)))
}

// ShouldResume decides between resume and start for c. With a
// history.locator, the tool persisted the conversation only if a transcript
// for the native id exists (a tool may drop a session that never got a
// prompt); without one, a discovered id means it exists, and a pre-assigned
// id is trusted once the conversation was launched.
func ShouldResume(root fsx.Root, home string, a adapter.Adapter, c conv.Conversation) bool {
	if c.NativeID == "" {
		return false
	}
	if a.History.Locator != "" {
		ms, err := TranscriptPaths(root, home, a, c.NativeID)
		return err == nil && len(ms) > 0
	}
	if !a.Capabilities.PreassignID {
		return true
	}
	return len(c.LaunchArgv) > 0
}

// Snapshot is what existed before a launch, so Discover can tell what is new.
type Snapshot struct {
	Mode    string
	Pattern string          // dir-diff and state-file: the expanded glob
	Paths   map[string]bool // dir-diff and state-file
	IDs     map[string]bool // list
}

// Discovery errors.
var (
	ErrNotDiscovered = errors.New("no new conversation found yet")
	ErrAmbiguous     = errors.New("several new conversations match; not guessing")
)

// locatorGlob turns history.locator into a glob: "~/" is home and
// {native_id} matches anything.
func locatorGlob(a adapter.Adapter, home string) (string, error) {
	loc := a.History.Locator
	if loc == "" {
		return "", fmt.Errorf("adapter %s: discovery %s needs history.locator", a.Name, a.Capabilities.Discovery)
	}
	if strings.HasPrefix(loc, "~/") {
		loc = filepath.Join(home, loc[2:])
	}
	return strings.ReplaceAll(loc, "{native_id}", "*"), nil
}

// TakeSnapshot records the state Discover compares against. Call it before
// launching; run is used by the list mode, home expands "~/" in locators.
func TakeSnapshot(ctx context.Context, root fsx.Root, a adapter.Adapter, run execx.Runner, home string) (Snapshot, error) {
	s := Snapshot{Mode: a.Capabilities.Discovery}
	switch s.Mode {
	case "dir-diff", "state-file":
		pat, err := locatorGlob(a, home)
		if err != nil {
			return s, err
		}
		s.Pattern = pat
		s.Paths = map[string]bool{}
		ms, err := filepath.Glob(root.Path(pat))
		if err != nil {
			return s, err
		}
		for _, m := range ms {
			s.Paths[m] = true
		}
	case "list":
		entries, err := listEntries(ctx, a, run)
		if err != nil {
			return s, err
		}
		s.IDs = map[string]bool{}
		for _, e := range entries {
			s.IDs[e.id] = true
		}
	default:
		return s, fmt.Errorf("adapter %s: no discovery mode", a.Name)
	}
	return s, nil
}

// Discover finds the native id of the conversation launched at started in
// cwd: dir-diff and state-file look for a new file matching history.locator,
// modified after started and mentioning cwd; list runs commands.list and
// takes the new entry for cwd. Two candidates are ErrAmbiguous (never a
// guess), none is ErrNotDiscovered. Callers serialise launches per cwd.
func Discover(ctx context.Context, root fsx.Root, a adapter.Adapter, run execx.Runner, before Snapshot, started time.Time, cwd string) (string, error) {
	// Allow for coarse file timestamps.
	since := started.Add(-time.Second)
	var cands []string
	switch before.Mode {
	case "dir-diff", "state-file":
		ms, err := filepath.Glob(root.Path(before.Pattern))
		if err != nil {
			return "", err
		}
		for _, m := range ms {
			if before.Paths[m] {
				continue
			}
			fi, err := os.Stat(m)
			if err != nil || fi.ModTime().Before(since) {
				continue
			}
			if cwd != "" && !mentions(m, cwd) {
				continue
			}
			if id := idFromPath(a, m); ValidNativeID(id) {
				cands = append(cands, id)
			}
		}
	case "list":
		entries, err := listEntries(ctx, a, run)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if !ValidNativeID(e.id) || before.IDs[e.id] || (e.cwd != "" && cwd != "" && filepath.Clean(e.cwd) != filepath.Clean(cwd)) {
				continue
			}
			if !e.at.IsZero() && e.at.Before(since) {
				continue
			}
			cands = append(cands, e.id)
		}
	default:
		return "", fmt.Errorf("adapter %s: no discovery mode", a.Name)
	}
	sort.Strings(cands)
	cands = dedupe(cands)
	switch len(cands) {
	case 0:
		return "", ErrNotDiscovered
	case 1:
		return cands[0], nil
	}
	return "", fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(cands, ", "))
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// mentions reports whether the head of file p contains cwd as a JSON string.
func mentions(p, cwd string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	var q bytes.Buffer
	enc := json.NewEncoder(&q)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(cwd)
	return bytes.Contains(buf[:n], bytes.TrimSpace(q.Bytes()))
}

const uuidRe = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// idFromPath extracts {native_id} from a path matched by the locator. The
// base name is matched with the id as a UUID first (tools commonly use them,
// and a '*' next to the id makes the split ambiguous otherwise), then as any
// non-empty text.
func idFromPath(a adapter.Adapter, p string) string {
	base := filepath.Base(a.History.Locator)
	if !strings.Contains(base, "{native_id}") {
		return ""
	}
	pre, post, _ := strings.Cut(base, "{native_id}")
	glob := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(regexp.QuoteMeta(s), `\*`, `.*?`), `\?`, `.`)
	}
	for _, id := range []string{uuidRe, `[^/]+`} {
		re, err := regexp.Compile("^" + glob(pre) + "(" + id + ")" + glob(post) + "$")
		if err != nil {
			return ""
		}
		if m := re.FindStringSubmatch(filepath.Base(p)); m != nil {
			return m[1]
		}
	}
	return ""
}

type listEntry struct {
	id, cwd string
	at      time.Time
}

// listEntries runs commands.list and parses it tolerantly: a JSON array of
// objects, or an object holding one; entries without an id are skipped.
func listEntries(ctx context.Context, a adapter.Adapter, run execx.Runner) ([]listEntry, error) {
	if run == nil {
		return nil, errors.New("list discovery needs a command runner")
	}
	argv, err := a.Render("list", adapter.Vars{})
	if err != nil {
		return nil, err
	}
	out, err := run.Run(ctx, argv[0], argv[1:]...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	var raw any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("%s: output is not JSON: %w", strings.Join(argv, " "), err)
	}
	items, _ := raw.([]any)
	if obj, ok := raw.(map[string]any); ok {
		for _, k := range []string{"sessions", "agents", "conversations", "items", "data"} {
			if arr, ok := obj[k].([]any); ok {
				items = arr
				break
			}
		}
	}
	var out2 []listEntry
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		e := listEntry{
			id:  firstString(m, "id", "session_id", "sessionId", "uuid"),
			cwd: firstString(m, "cwd", "project_path", "projectPath", "path"),
			at:  firstTime(m, "created_at", "createdAt", "started_at", "startedAt", "start_time"),
		}
		if e.id != "" {
			out2 = append(out2, e)
		}
	}
	return out2, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func firstTime(m map[string]any, keys ...string) time.Time {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return t
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return epoch(n)
			}
		case float64:
			return epoch(int64(v))
		}
	}
	return time.Time{}
}

// epoch reads seconds or milliseconds since 1970.
func epoch(n int64) time.Time {
	if n > 1e12 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}
