// Package conv holds the conversation record and its append-only JSONL store.
package conv

import "time"

// Conversation states (spec §9.3).
const (
	StateStarting = "starting"
	StateWorking  = "working"
	StateWaiting  = "waiting"
	StateIdle     = "idle"
	StateDone     = "done"
	StateFailed   = "failed"
	StateStopped  = "stopped"
	StateArchived = "archived"
	// StateQueued waits for a free slot under agents.max_running; no tool
	// runs.
	StateQueued = "queued"
)

// State sources: how the current state was learned.
const (
	SourceHook     = "hook"
	SourceNative   = "native"
	SourceObserved = "observed"
)

// Conversation is one agent conversation (spec §9.3). Pane is the @cairn_p
// key of the owning pane, not a tmux id.
type Conversation struct {
	ID       string   `json:"id"`
	Tool     string   `json:"tool,omitempty"`
	NativeID string   `json:"native_id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Purpose  string   `json:"purpose,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Project  string   `json:"project,omitempty"`
	Task     string   `json:"task,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Worktree string   `json:"worktree,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	Profile  string   `json:"profile,omitempty"`
	// Risk is set when the conversation was launched (or created) with
	// flags that skip the tool's permission checks or sandbox, allowed by
	// the project's i_understand_the_risk; it is shown as a "!" badge.
	Risk             bool       `json:"risk,omitempty"`
	LaunchArgv       []string   `json:"launch_argv,omitempty"`
	State            string     `json:"state,omitempty"`
	StateSource      string     `json:"state_source,omitempty"`
	WaitingReason    string     `json:"waiting_reason,omitempty"`
	LastActivityAt   time.Time  `json:"last_activity_at"`
	Pane             string     `json:"pane,omitempty"`
	ParentID         string     `json:"parent_id,omitempty"`
	ForkedFrom       string     `json:"forked_from,omitempty"`
	HandoffFrom      string     `json:"handoff_from,omitempty"`
	TranscriptRef    string     `json:"transcript_ref,omitempty"`
	TranscriptParser string     `json:"transcript_parser,omitempty"`
	Summary          string     `json:"summary,omitempty"`
	Pinned           bool       `json:"pinned,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ArchivedAt       *time.Time `json:"archived_at,omitempty"`
	AdapterVersion   string     `json:"adapter_version,omitempty"`
	// IgnoredEvents counts hook events that were not applied: invalid
	// transitions, stale or unknown events.
	IgnoredEvents int `json:"ignored_events,omitempty"`
}

// Filter selects conversations; empty fields match everything. Archived
// conversations are hidden unless Archived is true. Since matches on
// LastActivityAt.
type Filter struct {
	Project, Task, Tool, State, Tag string
	Archived                        bool
	Since                           time.Time
}

func (c Conversation) isArchived() bool {
	return c.ArchivedAt != nil || c.State == StateArchived
}

func (f Filter) match(c Conversation) bool {
	if c.isArchived() && !f.Archived && f.State != StateArchived {
		return false
	}
	if f.Project != "" && c.Project != f.Project {
		return false
	}
	if f.Task != "" && c.Task != f.Task {
		return false
	}
	if f.Tool != "" && c.Tool != f.Tool {
		return false
	}
	if f.State != "" && c.State != f.State {
		return false
	}
	if f.Tag != "" {
		found := false
		for _, t := range c.Tags {
			if t == f.Tag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !f.Since.IsZero() && c.LastActivityAt.Before(f.Since) {
		return false
	}
	return true
}

// ValidID reports whether s is a usable conversation id: it is put into file
// names, tmux options and hook commands, so only
// [A-Za-z0-9][A-Za-z0-9_-]{0,62} is accepted.
func ValidID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || (i > 0 && (c == '_' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// Match reports whether c passes f, as List decides.
func (f Filter) Match(c Conversation) bool { return f.match(c) }
