package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mrqwer/cairn/internal/xdg"
)

// applyLockWait is how long a command waits for another cairn that is
// planning or applying before it gives up.
var applyLockWait = 5 * time.Second

// applyMu serialises the commands of one process (tests run several in one
// process; flock alone is invisible to the race detector). The flock is what
// serialises processes.
var applyMu sync.Mutex

// applyLockPath is the single-writer lock of the tmux workspace commands.
func applyLockPath(d xdg.Dirs) string {
	return filepath.Join(d.State, "workspace", "apply.lock")
}

// lockApply takes the exclusive flock on <State>/workspace/apply.lock, held
// around plan and apply so two cairn processes never build the same window
// twice. It waits up to applyLockWait (and stops early when ctx is done),
// then fails naming the lock file. The returned func releases it and may be
// called more than once.
func lockApply(cmd *cobra.Command) (func(), error) {
	dirs, err := xdg.Resolve(os.Getenv)
	if err != nil {
		return nil, err
	}
	path := applyLockPath(dirs)
	if err := wsRoot.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(wsRoot.Path(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ctx := ctxOf(cmd)
	deadline := time.Now().Add(applyLockWait)
	for {
		var err error = unix.EWOULDBLOCK
		if applyMu.TryLock() {
			if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				applyMu.Unlock()
			}
		}
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			_ = f.Close()
			if ctx.Err() != nil {
				return nil, &ExitError{Code: exitInterrupted}
			}
			return nil, fmt.Errorf("another cairn is applying the workspace (waited %s for the lock %s); try again in a moment", applyLockWait, path)
		}
		time.Sleep(25 * time.Millisecond)
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close(); applyMu.Unlock() })
	}, nil
}
