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
	{ID: "01C", Tool: "agy", Project: "demo"},
}

func found(string) (string, error) { return "/usr/bin/ccusage", nil }

const ccusageKey = "/usr/bin/ccusage session --json --offline"

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
		if u.Source != profile.SourceUnavailable || u.Tokens != 0 || !u.Estimate {
			t.Fatalf("%s: %+v", id, u)
		}
	}
}

func TestUsageMalformedWarns(t *testing.T) {
	for name, resp := range map[string]execx.Response{
		"garbage": {Out: []byte("not json at all")},
		"exit":    {Out: []byte("boom"), Err: errors.New("exit status 1")},
	} {
		t.Run(name, func(t *testing.T) {
			run := &execx.FakeRunner{Responses: map[string]execx.Response{ccusageKey: resp}}
			got, err := profile.CollectUsage(context.Background(), run, found, usageConvs)
			var w *profile.Warning
			if !errors.As(err, &w) || !strings.Contains(err.Error(), "ccusage") {
				t.Fatalf("want a ccusage warning, got %v", err)
			}
			if len(got) != len(usageConvs) || got["01A"].Source != profile.SourceUnavailable {
				t.Fatalf("results stay usable: %+v", got)
			}
		})
	}
	// Entries that cannot be read are left out; the rest is used.
	out := `ccusage v99 (a note on stdout)
{"sessions": [
  {"sessionId": "` + uuidA + `", "totalTokens": 1200, "totalCost": 0.5},
  {"sessionId": 42, "totalTokens": "lots"},
  "junk",
  {"sessionId": "-home-u-demo/` + uuidB + `", "inputTokens": 10, "outputTokens": 5, "cacheReadTokens": 100, "costUSD": 0.01}
]}`
	run := &execx.FakeRunner{Responses: map[string]execx.Response{ccusageKey: {Out: []byte(out)}}}
	got, err := profile.CollectUsage(context.Background(), run, found, usageConvs)
	if err != nil {
		t.Fatalf("tolerated entries: %v", err)
	}
	if u := got["01A"]; u.Tokens != 1200 || u.CostUSD != 0.5 || u.Source != profile.SourceCcusage {
		t.Fatalf("01A %+v", u)
	}
	if u := got["01B"]; u.Tokens != 115 || u.CostUSD != 0.01 || u.Source != profile.SourceCcusage {
		t.Fatalf("01B %+v", u)
	}
	if u := got["01C"]; u.Source != profile.SourceNone || u.Tokens != 0 {
		t.Fatalf("01C %+v", u)
	}
}

func TestUsageMarkedEstimate(t *testing.T) {
	out := `[{"session_id": "` + uuidA + `", "total_tokens": 7, "cost": 1.25}]`
	run := &execx.FakeRunner{Responses: map[string]execx.Response{ccusageKey: {Out: []byte(out)}}}
	convs := append([]conv.Conversation{{ID: "01N", Tool: "x", Usage: &conv.Usage{InputTokens: 3, OutputTokens: 4, CostUSD: 2}}}, usageConvs...)
	got, err := profile.CollectUsage(context.Background(), run, found, convs)
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
	if u := got["01N"]; u.Source != profile.SourceNative || u.Tokens != 7 || u.CostUSD != 2 {
		t.Fatalf("native usage wins: %+v", u)
	}
	// ccusage runs once, with no shell, and only when a conversation has a
	// native id to match.
	if len(run.Calls) != 1 || run.Calls[0][0] != "/usr/bin/ccusage" {
		t.Fatalf("calls %v", run.Calls)
	}
	run = &execx.FakeRunner{}
	if _, err := profile.CollectUsage(context.Background(), run, found, usageConvs[2:]); err != nil || len(run.Calls) != 0 {
		t.Fatalf("no native ids: %v %v", err, run.Calls)
	}
}
