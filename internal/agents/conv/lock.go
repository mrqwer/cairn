package conv

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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
			holder = strings.TrimSpace(string(b))
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

// writePid replaces the lock file content with this process id. A contender
// that reads between flock and this write sees an empty or old holder; the
// ErrLocked message copes with an empty one.
func writePid(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return err
}

func (s Store) lockPath(tool, key string) string {
	return s.path("locks/" + safeName(tool) + "-" + safeName(key) + ".lock")
}

// LockLive reports, without taking it, whether the lock looks held: its
// file names a running process and was written since the last boot (a
// released lock is emptied; a pid from before a reboot may be reused). It
// reads only the lock file, so it never makes a contender see the lock
// busy. Best effort: a holder killed after writing its pid whose pid was
// reused since counts as live.
func (s Store) LockLive(tool, key string) bool {
	p := s.lockPath(tool, key)
	fi, err := os.Stat(p)
	if err != nil || fi.ModTime().Before(bootTime()) {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	err = unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// bootTime is when the system started (zero if unknown).
func bootTime() time.Time {
	var si unix.Sysinfo_t
	if unix.Sysinfo(&si) != nil {
		return time.Time{}
	}
	// One second of slack: uptime is whole seconds.
	return time.Now().Add(-time.Duration(si.Uptime+1) * time.Second)
}
