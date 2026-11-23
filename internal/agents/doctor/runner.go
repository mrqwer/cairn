package doctor

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// killGrace bounds how long Run waits for the output pipe after the
// process group was killed.
const killGrace = 500 * time.Millisecond

// Runner runs probe commands directly (no shell) in their own process
// group: when ctx ends the whole group is killed, so a wrapper's
// grandchildren cannot hold the output open, and Run returns within
// killGrace. Env, when non-nil, is the complete environment (see
// MinimalEnv); stdin is /dev/null.
type Runner struct {
	Dir string
	Env []string
}

// Run runs name with args and returns its combined output.
func (r Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	if r.Env != nil {
		cmd.Env = r.Env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The group id is the child's pid (Setpgid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = killGrace
	return cmd.CombinedOutput()
}

// MinimalEnv is the environment of probe commands: PATH, HOME (tmp when
// unset) and LANG. Nothing else of the caller's environment (tokens, keys)
// reaches the tool.
func MinimalEnv(getenv func(string) string, tmp string) []string {
	var env []string
	if v := getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	home := getenv("HOME")
	if home == "" {
		home = tmp
	}
	env = append(env, "HOME="+home)
	if v := getenv("LANG"); v != "" {
		env = append(env, "LANG="+v)
	}
	return env
}
