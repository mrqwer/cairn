package task

import (
	"sort"

	"github.com/mrqwer/cairn/internal/agents/conv"
)

// Queue is the agent concurrency limit (agents.max_running; spec §9.2).
// Running means a conversation in working, waiting or starting whose pane
// runs its tool: Live says which do (nil: all). Max ≤ 0 is no limit.
// Queued conversations wait in order of their last_activity_at (when they
// were queued), then id.
type Queue struct {
	Max  int
	Live func(conv.Conversation) bool
}

func (q Queue) running(c conv.Conversation) bool {
	switch c.State {
	case conv.StateWorking, conv.StateWaiting, conv.StateStarting:
	default:
		return false
	}
	return c.ArchivedAt == nil && (q.Live == nil || q.Live(c))
}

// Admit reports whether one more conversation may start next to running
// (every other conversation; the one asking is not in it). When not, the
// position it gets at the end of the queue.
func (q Queue) Admit(running []conv.Conversation) (ok bool, position int) {
	if q.Max <= 0 {
		return true, 0
	}
	n, queued := 0, 0
	for _, c := range running {
		switch {
		case q.running(c):
			n++
		case c.State == conv.StateQueued:
			queued++
		}
	}
	if n < q.Max {
		return true, 0
	}
	return false, queued + 1
}

// queued returns the queued conversations in queue order.
func queued(all []conv.Conversation) []conv.Conversation {
	var qs []conv.Conversation
	for _, c := range all {
		if c.State == conv.StateQueued && c.ArchivedAt == nil {
			qs = append(qs, c)
		}
	}
	sort.SliceStable(qs, func(i, j int) bool {
		if !qs[i].LastActivityAt.Equal(qs[j].LastActivityAt) {
			return qs[i].LastActivityAt.Before(qs[j].LastActivityAt)
		}
		return qs[i].ID < qs[j].ID
	})
	return qs
}

// Position is id's 1-based place in the queue (0: not queued).
func (q Queue) Position(all []conv.Conversation, id string) int {
	for i, c := range queued(all) {
		if c.ID == id {
			return i + 1
		}
	}
	return 0
}

// Next is the queued conversation to start when a slot is free: the oldest
// one whose pane is waiting for it (Live), else the oldest one.
func (q Queue) Next(all []conv.Conversation) (conv.Conversation, bool) {
	if ok, _ := q.Admit(all); !ok {
		return conv.Conversation{}, false
	}
	qs := queued(all)
	for _, c := range qs {
		if q.Live == nil || q.Live(c) {
			return c, true
		}
	}
	if len(qs) > 0 {
		return qs[0], true
	}
	return conv.Conversation{}, false
}
