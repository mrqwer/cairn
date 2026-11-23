package agents

import (
	"errors"
	"time"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/task"
)

// NewQueue is the concurrency limit over store s: a conversation runs when
// its pane holds its conversation lock (the tool runs, or the pane waits in
// the queue holding its place).
func NewQueue(s conv.Store, max int) task.Queue {
	return task.Queue{Max: max, Live: func(c conv.Conversation) bool { return s.LockLive(c.Tool, "cairn-"+c.ID) }}
}

func split(all []conv.Conversation, id string) (self conv.Conversation, others []conv.Conversation, found bool) {
	for _, c := range all {
		if c.ID == id {
			self, found = c, true
			continue
		}
		others = append(others, c)
	}
	return self, others, found
}

func queue(c *conv.Conversation, now time.Time) {
	c.State, c.StateSource, c.WaitingReason, c.LastActivityAt = conv.StateQueued, conv.SourceObserved, "", now
}

func admit(c *conv.Conversation, now time.Time) {
	c.State, c.StateSource, c.WaitingReason, c.LastActivityAt = conv.StateStarting, conv.SourceObserved, "", now
}

// liveQueued reports whether a conversation whose pane waits for a slot is
// in cs: a new start does not overtake it.
func liveQueued(q task.Queue, cs []conv.Conversation) bool {
	for _, c := range cs {
		if c.State == conv.StateQueued && c.ArchivedAt == nil && (q.Live == nil || q.Live(c)) {
			return true
		}
	}
	return false
}

// AdmitNew is the check of agents new/resume and task new: with no free
// slot, conversation id is recorded as queued (queued since now) and its
// queue position returned. One store transaction, so concurrent starts
// cannot both take the last slot. The pane checks again at launch
// (AdmitLaunch), where the limit is enforced.
func AdmitNew(s conv.Store, q task.Queue, id string, now time.Time) (queued bool, position int, err error) {
	err = s.Txn(func(all []conv.Conversation) ([]conv.Conversation, error) {
		self, others, found := split(all, id)
		if !found {
			return nil, conv.ErrNotFound
		}
		if self.State == conv.StateQueued {
			queued, position = true, q.Position(all, id)
			return nil, nil
		}
		if ok, _ := q.Admit(others); ok {
			return nil, nil
		}
		queue(&self, now)
		queued, position = true, q.Position(append(others, self), id)
		return []conv.Conversation{self}, nil
	})
	return queued, position, err
}

// ErrDequeued is AdmitLaunch's answer to a pane that waited in the queue
// when its conversation was taken out (Dequeue): it must not start.
var ErrDequeued = errors.New("taken out of the queue")

// AdmitLaunch is the check of a pane about to start its tool; the caller
// holds the conversation lock, so this conversation counts as live. A
// queued conversation starts when a slot is free and no older waiting
// pane is ahead of it; any other starts when a slot is free and no pane
// waits. Admitted, it is recorded as starting (taking the slot within the
// same transaction); otherwise as queued, and its position is returned.
// waiting says the pane has been queued by an earlier call: a record that
// is then neither queued nor starting (promoted) was dequeued, ErrDequeued.
func AdmitLaunch(s conv.Store, q task.Queue, id string, now time.Time, waiting bool) (ok bool, position int, err error) {
	err = s.Txn(func(all []conv.Conversation) ([]conv.Conversation, error) {
		self, others, found := split(all, id)
		if !found {
			return nil, conv.ErrNotFound
		}
		if waiting && self.State != conv.StateQueued && self.State != conv.StateStarting {
			return nil, ErrDequeued
		}
		if self.State == conv.StateQueued {
			if next, has := q.Next(all); has && next.ID == id {
				admit(&self, now)
				ok = true
				return []conv.Conversation{self}, nil
			}
			position = q.Position(all, id)
			return nil, nil
		}
		if free, _ := q.Admit(others); free && !liveQueued(q, others) {
			ok = true
			if self.State == conv.StateStarting {
				return nil, nil // promoted, or a fresh record: the slot is its
			}
			admit(&self, now)
			return []conv.Conversation{self}, nil
		}
		queue(&self, now)
		all = append(others, self)
		position = q.Position(all, id)
		return []conv.Conversation{self}, nil
	})
	return ok, position, err
}

// Promote gives a free slot to the next queued conversation (Queue.Next),
// recording it as starting; its waiting pane then starts the tool. It runs
// when a conversation stops taking a slot: cairn event idle, done, failed
// or stopped, and a pane whose tool exited. wait bounds the store lock
// wait (< 0 blocks; hooks must not stall).
func Promote(s conv.Store, q task.Queue, now time.Time, wait time.Duration) (promoted conv.Conversation, ok bool, err error) {
	err = s.TxnWait(wait, func(all []conv.Conversation) ([]conv.Conversation, error) {
		next, has := q.Next(all)
		if !has {
			return nil, nil
		}
		admit(&next, now)
		promoted, ok = next, true
		return []conv.Conversation{next}, nil
	})
	return promoted, ok, err
}

// Dequeue takes a queued conversation out of the queue (stopped), so its
// waiting pane gives up instead of starting the tool. false when it is not
// queued (its pane may have just been admitted).
func Dequeue(s conv.Store, id string) (bool, error) {
	ok := false
	_, err := s.Update(id, func(c *conv.Conversation) error {
		if c.State != conv.StateQueued {
			return conv.ErrNoChange
		}
		c.State, c.StateSource, c.WaitingReason = conv.StateStopped, conv.SourceObserved, ""
		ok = true
		return nil
	})
	return ok, err
}
