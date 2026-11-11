package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/doctor"
)

type stubCheck struct{ r doctor.Result }

func (s stubCheck) ID() string                        { return s.r.ID }
func (s stubCheck) Title() string                     { return s.r.Title }
func (s stubCheck) Run(context.Context) doctor.Result { return s.r }

func runDoctor(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestDoctorNoChecks(t *testing.T) {
	out, err := runDoctor(t, "doctor", "host")
	if err != nil || strings.TrimSpace(out) != "no checks registered" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestDoctorCritExitAndJSON(t *testing.T) {
	old := hostChecks
	t.Cleanup(func() { hostChecks = old })
	hostChecks = func(context.Context) ([]doctor.Check, error) {
		return []doctor.Check{stubCheck{doctor.Result{ID: "x", Title: "X", Status: doctor.Crit, Evidence: "bad", Fix: "fix it"}}}, nil
	}
	out, err := runDoctor(t, "doctor", "--json")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(out, `"status": "CRIT"`) || !strings.Contains(out, `"fix": "fix it"`) {
		t.Fatalf("out=%s", out)
	}
	out, _ = runDoctor(t, "doctor")
	if !strings.Contains(out, "CRIT  X  bad\n      fix: fix it\n") {
		t.Fatalf("out=%q", out)
	}
}
