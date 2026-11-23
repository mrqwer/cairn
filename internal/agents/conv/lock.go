package conv

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// ErrLocked means the conversation is already resumed elsewhere. Holder is the
// pid recorded in the lock file (may be empty if unreadable).
type ErrLocked struct{ Holder string }

func (e ErrLocked) Error() string {
	if e.Holder == "" {
		return "conversation is in use by another cairn process"
	}
	return "conversation is in use by another cairn process (pid " + e.Holder + ")"
}

// safeName escapes every byte outside [A-Za-z0-9._-] so a tool or native id
// can never alter the lock path. A leading dot is escaped too.
func safeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' && i > 0
		if ok {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// AcquireLock takes a non-blocking exclusive flock on
// <Dir>/locks/<tool>-<native_id>.lock and writes this pid into it. The kernel
// drops the lock when the holder dies, so a lock file left by a dead pid is
// simply taken over. release is idempotent.
func (s Store) AcquireLock(tool, nativeID string) (release func(), err error) {
	if err := s.Root.MkdirAll(s.Dir+"/locks", 0o700); err != nil {
		return nil, err
	}
	p := s.lockPath(tool, nativeID)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder := ""
		if b, rerr := os.ReadFile(p); rerr == nil {
			if f := strings.Fields(string(b)); len(f) > 0 {
				holder = f[0]
			}
		}
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked{Holder: holder}
		}
		return nil, err
	}
	if err := writePid(f); err != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("record lock holder: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Clear the holder while still locked: an empty file is a
			// lock nobody holds (see LockLive).
			_ = f.Truncate(0)
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

// writePid replaces the lock file content with "<pid> <boot id>". A
// contender that reads between flock and this write sees an empty or old
// holder; the ErrLocked message copes with an empty one.
func writePid(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	content := strconv.Itoa(os.Getpid())
	if id := bootID(); id != "" {
		content += " " + id
	}
	_, err := f.WriteAt([]byte(content), 0)
	return err
}

func (s Store) lockPath(tool, key string) string {
	return s.path("locks/" + safeName(tool) + "-" + safeName(key) + ".lock")
}

// LockLive reports, without taking it, whether the lock looks held: its
// file names a running process and the current boot (a released lock is
// emptied; a pid from an earlier boot may be reused by now). It reads only
// the lock file, so it never makes a contender see the lock busy. Best
// effort: a holder killed after writing whose pid was reused within the
// same boot counts as live.
func (s Store) LockLive(tool, key string) bool {
	b, err := os.ReadFile(s.lockPath(tool, key))
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[1] != bootID() {
		return false
	}
	pid, err := strconv.Atoi(f[0])
	if err != nil || pid <= 0 {
		return false
	}
	err = unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// bootID identifies the running boot ("" when unknown).
func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
