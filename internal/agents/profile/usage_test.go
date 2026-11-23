package profile_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/profile"
	"github.com/mrqwer/cairn/internal/execx"
)

const (
	uuidA = "0b6f3b1e-6f5c-4c43-9d0a-1d2b3c4d5e6f"
	uuidB = "7c1d2e3f-0a1b-4c2d-8e3f-405162738495"
)

var usageConvs = []conv.Conversation{
	{ID: "01A", Tool: "claude", NativeID: uuidA, Project: "api"},
	{ID: "01B", Tool: "claude", NativeID: uuidB, Project: "demo"},
	{ID: "01C", Tool: "claude", Project: "demo"},
}

func found(string) (string, error) { return "/usr/bin/ccusage", nil }

func ccKey(id string) string { return "/usr/bin/ccusage session --id " + id + " --json --offline" }

func TestUsageMissingCcusageNotError(t *testing.T) {
	run := &execx.FakeRunner{}
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	got, err := profile.CollectUsage(context.Background(), run, missing, usageConvs)
	if err != nil {
		t.Fatalf("a missing ccusage is not an error: %v", err)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("nothing may run: %v", run.Calls)
	}
	if len(got) != len(usageConvs) {
		t.Fatalf("every conversation gets an entry: %+v", got)
	}
	for id, u := range got {
		if u.Source != profile.SourceUnavailable || u.Tokens != 0 || !u.Estimate || u.Reason == "" || strings.Contains(u.Reason, "\n") {
			t.Fatalf("%s: %+v", id, u)
		}
	}
	if !strings.Contains(got["01A"].Reason, "not installed") {
		t.Fatalf("reason %q", got["01A"].Reason)
	}
}

func TestUsageMalformedWarns(t *testing.T) {
	for name, resp := range map[string]execx.Response{
		"garbage": {Out: []byte("not json at all")},
		"exit":    {Out: []byte("boom"), Err: errors.New("exit status 1")},
		"huge":    {Out: []byte(`{"sessionId":"` + uuidA + `","totalTokens":1,"pad":"` + strings.Repeat("x", profile.MaxUsageOutput) + `"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			run := &execx.FakeRunner{Responses: map[string]execx.Response{ccKey(uuidA): resp, ccKey(uuidB): resp}}
			got, err := profile.CollectUsage(context.Background(), run, found, usageConvs)
			var w *profile.Warning
			if !errors.As(err, &w) || !strings.Contains(err.Error(), "ccusage") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("want a one-line ccusage warning, got %v", err)
			}
			if u := got["01A"]; u.Source != profile.SourceUnavailable || u.Reason == "" {
				t.Fatalf("results stay usable: %+v", got)
			}
		})
	}
}

func TestUsageExactMatchOnly(t *testing.T) {
	run := &execx.FakeRunner{Responses: map[string]execx.Response{
		ccKey(uuidA): {Out: []byte("a note\n" + `{"sessionId": "` + uuidA + `", "totalTokens": 1200, "totalCost": 0.5}`)},
		// An older ccusage keys sessions by project directory: no match.
		ccKey(uuidB): {Out: []byte(`{"sessions": [{"sessionId": "-home-u-demo/` + uuidB + `", "totalTokens": 9, "totalCost": 1}]}`)},
	}}
	got, err := profile.CollectUsage(context.Background(), run, found, usageConvs)
	if err != nil {
		t.Fatal(err)
	}
	if u := got["01A"]; u.Tokens != 1200 || u.CostUSD != 0.5 || u.Source != profile.SourceCcusage {
		t.Fatalf("01A %+v", u)
	}
	if u := got["01B"]; u.Source != profile.SourceUnavailable || u.Tokens != 0 || !strings.Contains(u.Reason, uuidB) {
		t.Fatalf("01B must not match a containing id: %+v", u)
	}
	if u := got["01C"]; u.Source != profile.SourceUnavailable || !strings.Contains(u.Reason, "no session id") {
		t.Fatalf("01C %+v", u)
	}
	// One query per conversation with a usable native id, no shell.
	if len(run.Calls) != 2 || run.Calls[0][0] != "/usr/bin/ccusage" {
		t.Fatalf("calls %v", run.Calls)
	}
	// A native id that could pass as a flag is never queried.
	run = &execx.FakeRunner{}
	got, _ = profile.CollectUsage(context.Background(), run, found, []conv.Conversation{{ID: "01X", NativeID: "--all"}})
	if len(run.Calls) != 0 || got["01X"].Source != profile.SourceUnavailable {
		t.Fatalf("flag-like id: %v %+v", run.Calls, got)
	}
}

func TestUsageMarkedEstimate(t *testing.T) {
	run := &execx.FakeRunner{Responses: map[string]execx.Response{
		ccKey(uuidA): {Out: []byte(`{"session_id": "` + uuidA + `", "total_tokens": 7, "cost": 1.25}`)},
		ccKey(uuidB): {Out: []byte(`{}`)},
	}}
	got, err := profile.CollectUsage(context.Background(), run, found, usageConvs)
	if err != nil {
		t.Fatal(err)
	}
	for id, u := range got {
		if !u.Estimate {
			t.Fatalf("%s: every figure is an estimate: %+v", id, u)
		}
	}
	if u := got["01A"]; u.Tokens != 7 || u.CostUSD != 1.25 {
		t.Fatalf("01A %+v", u)
	}
}
