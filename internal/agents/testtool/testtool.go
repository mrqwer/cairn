// Package testtool writes fake AI tool executables for tests.
package testtool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spec describes the fake tool's behaviour.
type Spec struct {
	Name    string // executable name, also the log file stem
	Version string // printed for --version
	// ListJSON is printed when the first argument is "agents" and --json is present.
	ListJSON string
	ExitCode int
}

// Write creates an executable POSIX sh script in dir that appends its argv and
// environment to <dir>/<name>.log, and returns its path.
func Write(t testing.TB, dir string, spec Spec) string {
	t.Helper()
	if spec.Name == "" {
		spec.Name = "fake"
	}
	if spec.Version == "" {
		spec.Version = "0.0.0"
	}
	log := filepath.Join(dir, spec.Name+".log")
	list := filepath.Join(dir, spec.Name+".list.json")
	if err := os.WriteFile(list, []byte(spec.ListJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo %s
  exit 0
fi
{
  echo "ARGV:"
  for a in "$@"; do printf '  %%s\n' "$a"; done
  echo "ENV:"
  env | sort
  echo "END"
} >> %s
if [ "$1" = "agents" ]; then
  for a in "$@"; do
    if [ "$a" = "--json" ]; then cat %s; exit 0; fi
  done
fi
exit %d
`, shQuote(spec.Version), shQuote(log), shQuote(list), spec.ExitCode)
	p := filepath.Join(dir, spec.Name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return p
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
