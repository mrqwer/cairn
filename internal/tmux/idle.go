package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procFS reads files under /proc; tests replace it.
var procFS = os.ReadFile

// shells are the program names accepted as an interactive shell, besides the
// basename of $SHELL.
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"mksh": true, "tcsh": true, "csh": true, "nu": true, "elvish": true, "xonsh": true,
}

func isShell(comm string) bool {
	comm = strings.TrimPrefix(comm, "-")
	if shells[comm] {
		return true
	}
	sh := filepath.Base(os.Getenv("SHELL"))
	return sh != "." && sh != "/" && comm == sh
}

// PaneIdle reports whether the pane process pid is a shell sitting at its
// prompt: its comm is a known shell (a program exec'd directly as the pane
// process, such as "sleep" or "nvim", is never idle), it owns the terminal's
// foreground process group (tpgid == pgrp) and it has no children. A process
// that no longer exists counts as idle.
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
	comm, gone, err := readMore(statPath, fmt.Sprintf("/proc/%d/comm", pid))
	if gone || err != nil {
		return gone, err
	}
	if !isShell(strings.TrimSpace(comm)) {
		return false, nil
	}
	children, gone, err := readMore(statPath, fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if gone || err != nil {
		return gone, err
	}
	return strings.TrimSpace(children) == "", nil
}

// readMore reads another /proc file of a process whose stat was readable.
// gone is true when the process vanished in between; a missing file of a
// live process (e.g. a kernel without the children file) is an error.
func readMore(statPath, path string) (data string, gone bool, err error) {
	b, err := procFS(path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := procFS(statPath); errors.Is(serr, fs.ErrNotExist) {
			return "", true, nil
		}
		return "", false, fmt.Errorf("tmux: cannot read %s: %w", path, err)
	}
	if err != nil {
		return "", false, err
	}
	return string(b), false, nil
}

// IsShell reports whether comm (a process or pane_current_command name) is a
// known interactive shell.
func IsShell(comm string) bool { return isShell(comm) }
