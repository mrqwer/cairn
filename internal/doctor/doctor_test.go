package doctor

import (
	"bytes"
	"context"
	"testing"
)

type fakeCheck struct {
	id, title string
	run       func() Result
}

func (f fakeCheck) ID() string                 { return f.id }
func (f fakeCheck) Title() string              { return f.title }
func (f fakeCheck) Run(context.Context) Result { return f.run() }

func TestExitCode(t *testing.T) {
	if got := ExitCode([]Result{{Status: OK}, {Status: Warn}, {Status: Unknown}}); got != 0 {
		t.Fatalf("OK+Warn: got %d, want 0", got)
	}
	if got := ExitCode([]Result{{Status: OK}, {Status: Crit}}); got != 2 {
		t.Fatalf("Crit: got %d, want 2", got)
	}
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("empty: got %d, want 0", got)
	}
}

func TestPanicBecomesUnknown(t *testing.T) {
	checks := []Check{
		fakeCheck{"a", "Fine", func() Result { return Result{Status: OK, Evidence: "good"} }},
		fakeCheck{"b", "Boom", func() Result { panic("kaboom") }},
	}
	rs := Run(context.Background(), checks)
	if len(rs) != 2 {
		t.Fatalf("got %d results", len(rs))
	}
	if rs[0].ID != "a" || rs[0].Title != "Fine" || rs[0].Status != OK {
		t.Fatalf("unexpected first result: %+v", rs[0])
	}
	if rs[1].ID != "b" || rs[1].Title != "Boom" || rs[1].Status != Unknown || rs[1].Evidence != "kaboom" {
		t.Fatalf("unexpected panic result: %+v", rs[1])
	}
}

func TestRenderShowsFixOnlyForNonOK(t *testing.T) {
	rs := []Result{
		{ID: "a", Title: "Sleep target", Status: OK, Evidence: "masked", Fix: "ignored"},
		{ID: "b", Title: "Lid switch", Status: Warn, Evidence: "suspend", Fix: "set ignore"},
		{ID: "c", Title: "Hook", Status: Crit, Evidence: "missing"},
		{ID: "d", Title: "Probe", Status: Unknown, Evidence: "boom", Fix: "retry"},
	}
	var buf bytes.Buffer
	Render(&buf, rs)
	want := "ok    Sleep target  masked\n" +
		"warn  Lid switch  suspend\n" +
		"      fix: set ignore\n" +
		"CRIT  Hook  missing\n" +
		"?     Probe  boom\n" +
		"      fix: retry\n"
	if buf.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{OK: "ok", Warn: "warn", Crit: "CRIT", Unknown: "?"} {
		if s.String() != want {
			t.Fatalf("%d: got %q want %q", s, s.String(), want)
		}
	}
}
