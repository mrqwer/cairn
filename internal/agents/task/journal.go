package task

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Journal records a task creation in progress as <Dir>/<id>.toml, flocked
// by the creating process for as long as it runs. The task file is written
// last, so a process that dies between its first git change and that write
// leaves an entry nobody holds: Recover finds it and undoes the git changes
// (R3), so no worktree or branch is left without a task.
type Journal struct{ Dir string }

// Begin records t and returns end, which removes the entry and must be
// called once the task file is written or the creation is compensated.
// The entry is locked before it appears under its name, so Recover never
// takes a live one for a dead one.
func (j Journal) Begin(t Task) (end func(), err error) {
	if !validID(t.ID) {
		return nil, fmt.Errorf("invalid task id %q", t.ID)
	}
	data, err := t.Encode()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(j.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(j.Dir, ".pending-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	fail := func(err error) (func(), error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	path := filepath.Join(j.Dir, t.ID+".toml")
	if err := os.Rename(tmp, path); err != nil {
		return fail(err)
	}
	done := false
	return func() {
		if done {
			return
		}
		done = true
		_ = os.Remove(path)
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Pending lists the tasks of the entries no live process holds, without
// acting on them (read commands report these; see Recover). A broken entry
// is listed with only its file name as ID.
func (j Journal) Pending() ([]Task, error) {
	ms, err := filepath.Glob(filepath.Join(j.Dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(ms)
	var out []Task
	for _, m := range ms {
		f, err := os.Open(m)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			_ = f.Close()
			continue // its process is still at work
		}
		data, rerr := os.ReadFile(m)
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		if rerr != nil {
			continue
		}
		t, err := Decode(data)
		if err != nil {
			t = Task{ID: filepath.Base(m)}
		}
		out = append(out, t)
	}
	return out, nil
}

func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\.") && !strings.HasPrefix(id, "-") && len(id) <= 63
}

// Recovered is one interrupted creation that Recover handled: Left is what
// its compensation could not remove (nil when committed: the task exists
// and nothing was undone).
type Recovered struct {
	Task      Task
	Committed bool
	Left      []string
}

// Recover handles every entry no live process holds: when committed says
// its task file exists the entry is only dropped, otherwise compensate
// undoes it first. A broken entry is reported and dropped.
func (j Journal) Recover(committed func(Task) bool, compensate func(Task) []string) ([]Recovered, error) {
	ms, err := filepath.Glob(filepath.Join(j.Dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(ms)
	var out []Recovered
	for _, m := range ms {
		f, err := os.Open(m)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = f.Close()
			continue // its process is still at work
		}
		// Another Recover may have handled it between Glob and the lock.
		if _, err := os.Stat(m); err != nil {
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
			continue
		}
		r, keep := recoverOne(m, committed, compensate)
		if !keep {
			_ = os.Remove(m)
		}
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		out = append(out, r)
	}
	return out, nil
}

func recoverOne(path string, committed func(Task) bool, compensate func(Task) []string) (Recovered, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Recovered{Left: []string{fmt.Sprintf("%s: %v", path, err)}}, true
	}
	t, err := Decode(data)
	if err != nil {
		return Recovered{Left: []string{fmt.Sprintf("%s: broken entry dropped: %v", path, err)}}, false
	}
	if committed(t) {
		return Recovered{Task: t, Committed: true}, false
	}
	return Recovered{Task: t, Left: compensate(t)}, false
}
