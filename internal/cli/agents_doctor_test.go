package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentsdoctor "github.com/mrqwer/cairn/internal/agents/doctor"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/doctor"
)

const doctorHelp = `Usage: probe [options]
  --session-id <uuid>
  --model <m>
Commands:
  agents   --json
`

// writeProbeAdapter writes a user adapter "probe" whose binary is a fake
// tool (an absolute path, which the test lookPath guard lets through).
func writeProbeAdapter(t *testing.T, dir string, spec testtool.Spec) string {
	t.Helper()
	spec.Name = "probe"
	bin := testtool.Write(t, t.TempDir(), spec)
	toml := `name = "probe"
binary = "` + bin + `"
version_check = ["` + bin + `", "--version"]
probe_flags = [["` + bin + `", "--help"]]

[capabilities]
preassign_id = true
list_json = true
hooks = "none"

[commands]
start = ["` + bin + `", "--session-id", "{id}", "{profile_args...}"]
list = ["` + bin + `", "agents", "--json"]

[profile_flags]
model = ["--model", "{value}"]
`
	p := filepath.Join(dir, "config/cairn/tools/probe.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin
}

func decodeFindings(t *testing.T, out string) []agentsdoctor.Finding {
	t.Helper()
	var fs []agentsdoctor.Finding
	if err := json.Unmarshal([]byte(out), &fs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return fs
}

func findingOf(fs []agentsdoctor.Finding, tool, check string) (agentsdoctor.Finding, bool) {
	for _, f := range fs {
		if f.Tool == tool && f.Check == check {
			return f, true
		}
	}
	return agentsdoctor.Finding{}, false
}

func TestAgentsDoctorOKExitsZero(t *testing.T) {
	dir, _ := wsTestEnv(t)
	writeProbeAdapter(t, dir, testtool.Spec{Version: "probe 1.0", Help: doctorHelp, ListJSON: `[]`})
	out, _, err := runSplit(t, "agents", "doctor", "--json")
	if exitCode(err) != 0 {
		t.Fatalf("exit %v\n%s", err, out)
	}
	fs := decodeFindings(t, out)
	if f, ok := findingOf(fs, "probe", "version"); !ok || f.Level != "ok" || f.Detail != "probe 1.0" {
		t.Fatalf("version %+v in %+v", f, fs)
	}
	// Built-ins are looked up by bare name, which the test guard refuses:
	// they read as not installed and never run.
	for _, n := range []string{"claude", "codex", "agy"} {
		if f, ok := findingOf(fs, n, "binary"); !ok || f.Level != "skip" || f.Detail != "not installed" {
			t.Fatalf("%s binary %+v", n, f)
		}
	}
}

func TestAgentsDoctorFailExitsOne(t *testing.T) {
	dir, _ := wsTestEnv(t)
	writeProbeAdapter(t, dir, testtool.Spec{Version: "probe 1.0", Help: doctorHelp, ListJSON: `nope`})
	out, _, err := runSplit(t, "agents", "doctor", "--tool", "probe", "--json")
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1, got %v\n%s", err, out)
	}
	fs := decodeFindings(t, out)
	if f, ok := findingOf(fs, "probe", "list"); !ok || f.Level != "fail" {
		t.Fatalf("list %+v", f)
	}
	for _, f := range fs {
		if f.Tool != "probe" {
			t.Fatalf("--tool probe printed %+v", f)
		}
	}
}

func TestAgentsDoctorWarnExitsZeroAndPrintsText(t *testing.T) {
	dir, _ := wsTestEnv(t)
	writeProbeAdapter(t, dir, testtool.Spec{Version: "probe 2.0", Help: strings.Replace(doctorHelp, "  --model <m>\n", "", 1), ListJSON: `[]`})
	out, _, err := runSplit(t, "agents", "doctor", "--tool", "probe")
	if exitCode(err) != 0 {
		t.Fatalf("a warning must not fail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warn") || !strings.Contains(out, "flag --model not in help of probe 2.0") {
		t.Fatalf("text output:\n%s", out)
	}
}

func TestAgentsDoctorUnknownTool(t *testing.T) {
	wsTestEnv(t)
	_, _, err := runSplit(t, "agents", "doctor", "--tool", "nope")
	if err == nil || !strings.Contains(err.Error(), `unknown tool "nope"`) {
		t.Fatalf("err %v", err)
	}
}

func TestAgentsDoctorBrokenAdapterFails(t *testing.T) {
	dir, _ := wsTestEnv(t)
	p := filepath.Join(dir, "config/cairn/tools/bad.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("name = \"bad\"\nbogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSplit(t, "agents", "doctor", "--json")
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1, got %v\n%s", err, out)
	}
	fs := decodeFindings(t, out)
	f, ok := findingOf(fs, "bad.toml", "load")
	if !ok || f.Level != "fail" || !strings.Contains(f.Detail, "bogus") {
		t.Fatalf("load %+v in %+v", f, fs)
	}
}

func TestDoctorAgentsScope(t *testing.T) {
	dir, _ := wsTestEnv(t)
	writeProbeAdapter(t, dir, testtool.Spec{Version: "probe 1.0", Help: doctorHelp, ListJSON: `nope`})
	out, err := runDoctor(t, "doctor", "agents", "--json")
	if exitCode(err) != 2 {
		t.Fatalf("a failing probe is critical in cairn doctor: %v\n%s", err, out)
	}
	var rs []jsonResult
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]jsonResult{}
	for _, r := range rs {
		got[r.ID] = r
	}
	if r := got["agents.probe.version"]; r.Status != "ok" || r.Evidence != "probe 1.0" {
		t.Fatalf("version %+v in %+v", r, rs)
	}
	if r := got["agents.probe.list"]; r.Status != "CRIT" {
		t.Fatalf("list %+v", r)
	}
	if r := got["agents.claude.binary"]; r.Status != "ok" || r.Evidence != "skipped: not installed" {
		t.Fatalf("claude %+v", r)
	}
	for id := range got {
		if !strings.HasPrefix(id, "agents.") {
			t.Fatalf("doctor agents ran %s", id)
		}
	}
}

func TestDoctorScopesRunAgentsOnlyWhenAsked(t *testing.T) {
	wsTestEnv(t)
	oldHost, oldAgents := hostChecks, agentsChecks
	t.Cleanup(func() { hostChecks, agentsChecks = oldHost, oldAgents })
	hostChecks = func(context.Context) ([]doctor.Check, error) { return nil, nil }
	calls := 0
	agentsChecks = func(context.Context) ([]doctor.Check, error) {
		calls++
		return []doctor.Check{stubCheck{doctor.Result{ID: "agents.x.binary", Title: "AgentX"}}}, nil
	}
	stubWorkspaceEnv(t, &roRunner{version: "3.4"})
	for _, scope := range []string{"host", "workspace"} {
		if out, _ := runDoctor(t, "doctor", scope); strings.Contains(out, "AgentX") || calls != 0 {
			t.Fatalf("doctor %s ran agent probes:\n%s", scope, out)
		}
	}
	if out, _ := runDoctor(t, "doctor", "--json"); !strings.Contains(out, `"id": "agents.x.binary"`) || calls != 1 {
		t.Fatalf("doctor without a scope must include agents:\n%s", out)
	}
}
