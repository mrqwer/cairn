// Package doctor runs health checks and renders their results.
package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Status is the outcome of a check.
type Status int

// Check outcomes.
const (
	OK Status = iota
	Warn
	Crit
	Unknown
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Crit:
		return "CRIT"
	default:
		return "?"
	}
}

// Result is the outcome of one check.
type Result struct {
	ID       string
	Title    string
	Status   Status
	Evidence string
	Fix      string
}

// Check is a single health check.
type Check interface {
	ID() string
	Title() string
	Run(ctx context.Context) Result
}

// Run executes every check in order. A panicking check yields an Unknown
// result carrying the panic text as evidence.
func Run(ctx context.Context, checks []Check) []Result {
	rs := make([]Result, 0, len(checks))
	for _, c := range checks {
		rs = append(rs, runOne(ctx, c))
	}
	return rs
}

func runOne(ctx context.Context, c Check) (r Result) {
	defer func() {
		if p := recover(); p != nil {
			r = Result{ID: c.ID(), Title: c.Title(), Status: Unknown, Evidence: fmt.Sprint(p)}
		}
	}()
	r = c.Run(ctx)
	if r.ID == "" {
		r.ID = c.ID()
	}
	if r.Title == "" {
		r.Title = c.Title()
	}
	return r
}

// Render writes one line per result, with a fix line under non-OK rows.
func Render(w io.Writer, rs []Result) {
	for _, r := range rs {
		line := fmt.Sprintf("%-4s  %s  %s", r.Status, r.Title, r.Evidence)
		_, _ = fmt.Fprintln(w, strings.TrimRight(line, " "))
		if r.Status != OK && r.Fix != "" {
			_, _ = fmt.Fprintf(w, "      fix: %s\n", r.Fix)
		}
	}
}

// ExitCode returns 2 if any result is Crit, else 0.
func ExitCode(rs []Result) int {
	for _, r := range rs {
		if r.Status == Crit {
			return 2
		}
	}
	return 0
}
