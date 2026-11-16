package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// procFS reads files under /proc; tests replace it.
var procFS = os.ReadFile

// PaneIdle reports whether the pane process pid sits at its prompt: it owns
// the terminal's foreground process group (tpgid == pgrp) and has no
// children. A process that no longer exists counts as idle.
func (c Client) PaneIdle(pid int) (bool, error) {
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	stat, err := procFS(statPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return false, fmt.Errorf("tmux: malformed %s", statPath)
	}
	// After ") ": state(3) ppid(4) pgrp(5) session(6) tty_nr(7) tpgid(8).
	f := strings.Fields(s[i+1:])
	if len(f) < 6 {
		return false, fmt.Errorf("tmux: malformed %s", statPath)
	}
	pgrp, err1 := strconv.Atoi(f[2])
	tpgid, err2 := strconv.Atoi(f[5])
	if err1 != nil || err2 != nil {
		return false, fmt.Errorf("tmux: malformed %s", statPath)
	}
	if tpgid != pgrp {
		return false, nil
	}
	children, err := procFS(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if errors.Is(err, fs.ErrNotExist) {
		// Gone between the two reads, or a kernel without the children file.
		if _, serr := procFS(statPath); errors.Is(serr, fs.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("tmux: cannot read children of %d: %w", pid, err)
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(children)) == "", nil
}
