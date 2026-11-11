// Package execx abstracts running external commands so they can be faked in tests.
package execx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// OSRunner runs real commands.
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Response is a canned result for FakeRunner.
type Response struct {
	Out []byte
	Err error
}

// FakeRunner returns canned responses keyed by the space-joined command line
// and records every call.
type FakeRunner struct {
	Responses map[string]Response
	Calls     [][]string
}

func (f *FakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.Calls = append(f.Calls, call)
	key := strings.Join(call, " ")
	resp, ok := f.Responses[key]
	if !ok {
		return nil, fmt.Errorf("execx: unexpected command %q", key)
	}
	return resp.Out, resp.Err
}
