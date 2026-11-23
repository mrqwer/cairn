package conv

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mrqwer/cairn/internal/fsx"
	"golang.org/x/sys/unix"
)

const (
	dataFile  = "conversations.jsonl"
	storeLock = ".store.lock"
)

// ErrNotFound is returned by Update for an unknown id.
var ErrNotFound = errors.New("conversation not found")

// Store is an append-only JSONL file of conversation records; a reader takes
// the last line per id. Dir is an absolute path resolved under Root.
type Store struct {
	Dir  string
	Root fsx.Root
}

func (s Store) path(name string) string { return s.Root.Path(s.Dir + "/" + name) }

func (s Store) ensureDir() error { return s.Root.MkdirAll(s.Dir, 0o700) }

// ErrBusy is returned by TryUpdate when the store lock stayed taken.
var ErrBusy = errors.New("conversation store is busy")

// withLock runs fn holding the exclusive store lock (blocking).
func (s Store) withLock(fn func() error) error { return s.withLockWait(-1, fn) }

// withLockWait runs fn holding the exclusive store lock, waiting at most
// wait for it (blocking when wait < 0); a lock still taken is ErrBusy.
func (s Store) withLockWait(wait time.Duration, fn func() error) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(storeLock), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	deadline := time.Now().Add(wait)
	for {
		if wait < 0 {
			err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		} else {
			err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if errors.Is(err, unix.EWOULDBLOCK) {
				if time.Now().After(deadline) {
					return ErrBusy
				}
				time.Sleep(5 * time.Millisecond)
				continue
			}
		}
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// load is the visible view: the last record per id with its curated
// overlay applied; removed conversations are left out.
func (s Store) load() ([]Conversation, error) {
	recs, err := s.loadRaw()
	if err != nil {
		return nil, err
	}
	overlays, err := s.overlays()
	if err != nil {
		return nil, err
	}
	out := recs[:0]
	for _, c := range recs {
		if cu, ok := overlays[safeName(c.ID)+".json"]; ok {
			if cu.Removed {
				continue
			}
			cu.apply(&c)
		}
		out = append(out, c)
	}
	return out, nil
}

// loadRaw folds the file to the last record per id, in order of last write.
// Unparseable lines (including a truncated trailing one) are skipped.
func (s Store) loadRaw() ([]Conversation, error) {
	data, err := os.ReadFile(s.path(dataFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	last := map[string]int{}
	var recs []Conversation
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var c Conversation
		if json.Unmarshal(line, &c) != nil || c.ID == "" {
			continue
		}
		last[c.ID] = len(recs)
		recs = append(recs, c)
	}
	out := make([]Conversation, 0, len(last))
	for i, c := range recs {
		if last[c.ID] == i {
			out = append(out, c)
		}
	}
	return out, nil
}

// appendLocked writes one record with a single write call. Caller holds the lock.
// Only the final byte is read, to heal a truncated tail left by a crash; a
// read error is returned rather than skipping that guard. Creating the file
// fsyncs the parent directory so the new entry survives a power loss.
func (s Store) appendLocked(c Conversation) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p := s.path(dataFile)
	_, statErr := os.Stat(p)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error { _ = f.Close(); return err }
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
			return fail(fmt.Errorf("read tail of %s: %w", dataFile, err))
		}
		// A crash can leave a truncated line without newline; start on a fresh line.
		if last[0] != '\n' {
			b = append([]byte{'\n'}, b...)
		}
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if created {
		return syncDir(filepath.Dir(p))
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Put appends c as the newest record for its id.
func (s Store) Put(c Conversation) error {
	if c.ID == "" {
		return errors.New("conversation id is empty")
	}
	return s.withLock(func() error { return s.appendLocked(c) })
}

// Get returns the latest record for id.
func (s Store) Get(id string) (Conversation, bool, error) {
	recs, err := s.load()
	if err != nil {
		return Conversation{}, false, err
	}
	for _, c := range recs {
		if c.ID == id {
			return c, true, nil
		}
	}
	return Conversation{}, false, nil
}

// List returns matching conversations sorted by id (creation order).
func (s Store) List(f Filter) ([]Conversation, error) {
	recs, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []Conversation
	for _, c := range recs {
		if f.match(c) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update applies fn to the latest record under the store lock and appends the
// result. If fn fails nothing is written and the zero Conversation is
// returned. fn runs while the store lock is held and must not call any Store
// method: the flock is not reentrant and that would deadlock.
func (s Store) Update(id string, fn func(*Conversation) error) (Conversation, error) {
	return s.update(-1, id, fn)
}

// TryUpdate is Update waiting at most wait for the store lock; when it
// stays taken nothing is written and ErrBusy is returned. Hooks use it so a
// long compaction never stalls the tool.
func (s Store) TryUpdate(id string, wait time.Duration, fn func(*Conversation) error) (Conversation, error) {
	if wait < 0 {
		wait = 0
	}
	return s.update(wait, id, fn)
}

func (s Store) update(wait time.Duration, id string, fn func(*Conversation) error) (Conversation, error) {
	var res Conversation
	err := s.withLockWait(wait, func() error {
		recs, err := s.load()
		if err != nil {
			return err
		}
		found := false
		for _, c := range recs {
			if c.ID == id {
				res, found = c, true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		if err := fn(&res); err != nil {
			return err
		}
		res.ID = id
		return s.appendLocked(res)
	})
	if err != nil {
		return Conversation{}, err
	}
	return res, nil
}

// Compact atomically rewrites the file keeping the last record per id.
// Records hidden by a removed overlay are kept so undo can restore them.
func (s Store) Compact() error {
	return s.withLock(func() error {
		recs, err := s.loadRaw()
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		for _, c := range recs {
			b, err := json.Marshal(c)
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		return s.Root.WriteAtomic(s.Dir+"/"+dataFile, buf.Bytes(), 0o600)
	})
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a 26-character ULID: 48-bit millisecond timestamp then 80
// random bits, Crockford base32, so ids sort by creation time. A nil rand uses
// crypto/rand.
func NewID(now time.Time, r io.Reader) string {
	if r == nil {
		r = rand.Reader
	}
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := io.ReadFull(r, b[6:]); err != nil {
		panic("conv: random source failed: " + err.Error())
	}
	// 128 bits -> 26 groups of 5 bits (2 leading zero bits).
	out := make([]byte, 26)
	var acc, bits uint
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockford[acc&31]
			idx--
			acc >>= 5
			bits -= 5
		}
	}
	if idx == 0 {
		out[0] = crockford[acc&31]
	}
	return string(out)
}
