package doctor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/doctor"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

const fullHelp = `Usage: fake [options] [command]
  --session-id <uuid>
  -n, --name <name>
  --resume <id>
  --fork-session
  --model <model>
  --settings <file>
Commands:
  agents [options]   --json --all
`

// isolate points HOME and TMPDIR at fresh temp dirs and returns them.
func isolate(t *testing.T) (home, tmp string) {
	t.Helper()
	home, tmp = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmp)
	return home, tmp
}

func fakeAdapter(bin string) adapter.Adapter {
	return adapter.Adapter{
		Name:         "fake",
		Binary:       bin,
		VersionCheck: []string{bin, "--version"},
		Capabilities: adapter.Capabilities{NameAtStart: true, PreassignID: true, ResumeByID: true,
			Fork: true, ListJSON: true, Hooks: "settings-file"},
		Commands: map[string][]string{
			"start":  {bin, "--session-id", "{id}", "--name", "{name}", "{profile_args...}"},
			"resume": {bin, "--resume", "{id}", "{profile_args...}"},
			"fork":   {bin, "--resume", "{id}", "--fork-session", "--session-id", "{new_id}"},
			"list":   {bin, "agents", "--json"},
		},
		ProfileFlags: map[string][]string{"model": {"--model", "{value}"}},
		Hooks: adapter.HookConfig{Flag: "--settings", Format: adapter.HookFormatClaudeSettings,
			Events: map[string]string{"Stop": "idle", "UserPromptSubmit": "working"}},
		ProbeFlags: [][]string{{bin, "--help"}},
	}
}

func fakeTool(t *testing.T, spec testtool.Spec) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	if spec.Name == "" {
		spec.Name = "fake"
	}
	bin = testtool.Write(t, dir, spec)
	return bin, filepath.Join(dir, spec.Name+".log")
}

func find(fs []doctor.Finding, check string) []doctor.Finding {
	var out []doctor.Finding
	for _, f := range fs {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func one(t *testing.T, fs []doctor.Finding, check string) doctor.Finding {
	t.Helper()
	got := find(fs, check)
	if len(got) != 1 {
		t.Fatalf("want one %q finding, got %+v (all: %+v)", check, got, fs)
	}
	return got[0]
}

func hasFail(fs []doctor.Finding) bool {
	for _, f := range fs {
		if f.Level == "fail" {
			return true
		}
	}
	return false
}

func TestProbeMissingBinarySkips(t *testing.T) {
	isolate(t)
	run := &execx.FakeRunner{}
	fs := doctor.Probe(context.Background(), fakeAdapter("fake"), run,
		func(string) (string, error) { return "", exec.ErrNotFound })
	if len(fs) != 1 || fs[0] != (doctor.Finding{Tool: "fake", Check: "binary", Level: "skip", Detail: "not installed"}) {
		t.Fatalf("findings %+v", fs)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("nothing may run for a missing tool: %v", run.Calls)
	}
}

func TestProbeVersionRecorded(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Version: "fake 9.1.0 (Fake Code)", Help: fullHelp, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "binary"); f.Level != "ok" || f.Detail != bin {
		t.Fatalf("binary %+v", f)
	}
	if f := one(t, fs, "version"); f.Level != "ok" || f.Detail != "fake 9.1.0 (Fake Code)" {
		t.Fatalf("version %+v", f)
	}
	if hasFail(fs) {
		t.Fatalf("a conforming tool must not fail: %+v", fs)
	}
	for _, f := range fs {
		if f.Level == "warn" {
			t.Fatalf("a conforming tool must not warn: %+v", fs)
		}
	}
}

func TestProbeListJSONInvalidFails(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: `not json`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "list"); f.Level != "fail" || !strings.Contains(f.Detail, "not JSON") {
		t.Fatalf("list %+v", f)
	}
	for _, empty := range []string{"[]", "", "{}"} {
		bin, _ := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: empty})
		fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
		if f := one(t, fs, "list"); f.Level != "ok" {
			t.Fatalf("empty list %q must be accepted: %+v", empty, f)
		}
	}
}

func TestProbeFlagMissingFromHelp(t *testing.T) {
	isolate(t)
	help := strings.Replace(fullHelp, "  --fork-session\n", "", 1)
	bin, _ := fakeTool(t, testtool.Spec{Version: "fake 2.0", Help: help, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	got := find(fs, "flag")
	if len(got) != 1 || got[0].Level != "warn" || got[0].Detail != "flag --fork-session not in help of fake 2.0" {
		t.Fatalf("flag findings %+v", got)
	}
	if hasFail(fs) {
		t.Fatalf("a missing flag is a warning: %+v", fs)
	}
}

// A long flag must match as a whole word: --name is not found in --names.
func TestProbeFlagMatchesWholeWord(t *testing.T) {
	isolate(t)
	help := strings.Replace(fullHelp, "-n, --name <name>", "--names <list>", 1)
	bin, _ := fakeTool(t, testtool.Spec{Version: "fake 2.0", Help: help, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	got := find(fs, "flag")
	if len(got) != 1 || got[0].Detail != "flag --name not in help of fake 2.0" {
		t.Fatalf("flag findings %+v", got)
	}
}

func TestProbeNeverWritesOutsideTemp(t *testing.T) {
	home, tmp := isolate(t)
	bin, log := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "hooks"); f.Level != "ok" {
		t.Fatalf("hooks %+v", f)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := parseCalls(string(data))
	if len(calls) == 0 {
		t.Fatal("the probe ran no logged command (help and list are logged)")
	}
	for _, c := range calls {
		joined := strings.Join(c, " ")
		if joined != "--help" && joined != "agents --json" {
			t.Fatalf("the probe ran %q: only help and list may run", joined)
		}
	}
	for _, d := range []string{home, tmp} {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("the probe left files in %s: %v", d, ents)
		}
	}
}

// parseCalls reads the argv of each call from a testtool log.
func parseCalls(log string) [][]string {
	var calls [][]string
	var cur []string
	in := false
	for _, l := range strings.Split(log, "\n") {
		switch {
		case l == "ARGV:":
			in, cur = true, []string{}
		case l == "ENV:":
			if in {
				calls = append(calls, cur)
			}
			in = false
		case in:
			cur = append(cur, strings.TrimPrefix(l, "  "))
		}
	}
	return calls
}

// The list command is not run when its subcommand is missing from the
// help: an unknown word could be taken as a prompt and start a session.
func TestProbeListNotRunWhenSubcommandMissing(t *testing.T) {
	isolate(t)
	help := strings.Replace(fullHelp, "  agents [options]   --json --all\n", "", 1)
	bin, log := fakeTool(t, testtool.Spec{Version: "fake 3", Help: help, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "list"); f.Level != "warn" || !strings.Contains(f.Detail, "not run") {
		t.Fatalf("list %+v", f)
	}
	if f := one(t, fs, "subcommand"); f.Level != "warn" || f.Detail != "subcommand agents not in help of fake 3" {
		t.Fatalf("subcommand %+v", f)
	}
	data, _ := os.ReadFile(log)
	for _, c := range parseCalls(string(data)) {
		if len(c) > 0 && c[0] == "agents" {
			t.Fatalf("list ran: %v", c)
		}
	}
}

// Hook arguments go right after the binary, before any subcommand, so the
// hooks flag must be in the top-level help (probe_flags[0]).
func TestProbeHooksFlagMustBeTopLevel(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Version: "fake 4", Help: strings.Replace(fullHelp, "  --settings <file>\n", "", 1), ListJSON: `[]`})
	a := fakeAdapter(bin)
	sub := filepath.Join(t.TempDir(), "sub-help")
	// A second help command that does mention --settings.
	if err := os.WriteFile(sub, []byte("#!/bin/sh\necho '  --settings <file>'\n"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	a.ProbeFlags = append(a.ProbeFlags, []string{sub, "--help"})
	fs := doctor.Probe(context.Background(), a, execx.OSRunner{}, exec.LookPath)
	got := find(fs, "flag")
	if len(got) != 1 || got[0].Level != "warn" ||
		got[0].Detail != "flag --settings not in top-level help of fake 4 (cairn passes it before any subcommand)" {
		t.Fatalf("flag findings %+v", got)
	}
}

func TestProbeTimeoutFails(t *testing.T) {
	isolate(t)
	old := doctor.Timeout
	t.Cleanup(func() { doctor.Timeout = old })
	doctor.Timeout = 200 * time.Millisecond
	bin := filepath.Join(t.TempDir(), "slow")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	a := adapter.Adapter{Name: "slow", Binary: bin, VersionCheck: []string{bin, "--version"},
		Capabilities: adapter.Capabilities{Discovery: "none"}, Commands: map[string][]string{"start": {bin}}}
	start := time.Now()
	fs := doctor.Probe(context.Background(), a, execx.OSRunner{}, exec.LookPath)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("probe took %v", time.Since(start))
	}
	if f := one(t, fs, "version"); f.Level != "fail" || !strings.Contains(f.Detail, "timed out") {
		t.Fatalf("version %+v", f)
	}
}

func TestProbeCapabilityWithoutCommandFails(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: `[]`})
	a := fakeAdapter(bin)
	delete(a.Commands, "fork")
	fs := doctor.Probe(context.Background(), a, execx.OSRunner{}, exec.LookPath)
	got := find(fs, "capabilities")
	if len(got) != 1 || got[0].Level != "fail" || got[0].Detail != "fork is declared but commands.fork is missing" {
		t.Fatalf("capabilities %+v", got)
	}
}

func TestProbeDiscoveryWithoutLocatorWarns(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Help: "  --conversation <id>\n"})
	a := adapter.Adapter{Name: "fake", Binary: bin, VersionCheck: []string{bin, "--version"},
		Capabilities: adapter.Capabilities{ResumeByID: true, Hooks: "none", Discovery: "state-file"},
		Commands:     map[string][]string{"start": {bin}, "resume": {bin, "--conversation", "{native_id}"}},
		ProbeFlags:   [][]string{{bin, "--help"}}}
	fs := doctor.Probe(context.Background(), a, execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "discovery"); f.Level != "warn" || !strings.Contains(f.Detail, "history.locator") {
		t.Fatalf("discovery %+v", f)
	}
	if f := one(t, fs, "hooks"); f.Level != "skip" {
		t.Fatalf("hooks %+v", f)
	}
}

// What cannot be probed without a conversation is reported, not passed.
func TestProbeHookDeliveryIsUnverified(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	f := one(t, fs, "hooks.delivery")
	if f.Level != "skip" || !strings.Contains(f.Detail, "not probed") || !strings.Contains(f.Detail, "Stop") {
		t.Fatalf("hooks.delivery %+v", f)
	}
}

func TestProbeConfigOverrideParses(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Help: "  -c, --config <key=value>\nCommands:\n  resume\n"})
	a := adapter.Adapter{Name: "fake", Binary: bin, VersionCheck: []string{bin, "--version"},
		Capabilities: adapter.Capabilities{ResumeByID: true, Hooks: "config-override", Discovery: "none"},
		Commands:     map[string][]string{"start": {bin}, "resume": {bin, "resume", "{native_id}"}},
		Hooks:        adapter.HookConfig{Flag: "-c", Key: "notify", Kind: "idle"},
		ProbeFlags:   [][]string{{bin, "--help"}}}
	fs := doctor.Probe(context.Background(), a, execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "hooks"); f.Level != "ok" {
		t.Fatalf("hooks %+v", f)
	}
	if hasFail(fs) || len(find(fs, "flag")) != 0 || len(find(fs, "subcommand")) != 0 {
		t.Fatalf("findings %+v", fs)
	}
}

func TestProbeHelpUnreadableWarns(t *testing.T) {
	isolate(t)
	bin, _ := fakeTool(t, testtool.Spec{Help: "", ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), execx.OSRunner{}, exec.LookPath)
	if f := one(t, fs, "help"); f.Level != "warn" {
		t.Fatalf("help %+v", f)
	}
	if len(find(fs, "flag")) != 0 {
		t.Fatalf("no flag verdicts without help: %+v", fs)
	}
}

func TestConformanceBuiltins(t *testing.T) {
	isolate(t)
	as, lerrs, err := adapter.Load(fsx.Root{Dir: t.TempDir()}, xdg.Dirs{Config: "/cfg"})
	if err != nil || len(lerrs) != 0 {
		t.Fatalf("Load: %v %v", err, lerrs)
	}
	seen := map[string]bool{}
	for _, a := range as {
		t.Run(a.Name, func(t *testing.T) {
			fake, _ := fakeTool(t, testtool.Spec{Name: a.Name, ListJSON: `[{"id":"x"}]`})
			doctor.Conformance(t, a, fake)
		})
		seen[a.Name] = true
	}
	for _, n := range []string{"claude", "codex", "agy"} {
		if !seen[n] {
			t.Errorf("built-in %s not covered", n)
		}
	}
}

// recorder captures Conformance failures so the suite itself is tested.
type recorder struct {
	testing.TB
	msgs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	panic(errStop)
}
func (r *recorder) Helper() {}

var errStop = errors.New("stop")

func runRecorded(t *testing.T, a adapter.Adapter, fake string) []string {
	t.Helper()
	r := &recorder{TB: t}
	func() {
		defer func() {
			if p := recover(); p != nil && p != errStop {
				panic(p)
			}
		}()
		doctor.Conformance(r, a, fake)
	}()
	return r.msgs
}

// The suite catches an adapter whose argv loses a placeholder value.
func TestConformanceCatchesBrokenAdapter(t *testing.T) {
	isolate(t)
	fake, _ := fakeTool(t, testtool.Spec{ListJSON: `[]`})
	a := fakeAdapter("fake")
	for k, v := range a.Commands {
		v = append([]string(nil), v...)
		v[0] = "fake"
		a.Commands[k] = v
	}
	if msgs := runRecorded(t, a, fake); len(msgs) != 0 {
		t.Fatalf("a good adapter must pass: %v", msgs)
	}
	a.Commands["resume"] = []string{"fake", "--resume", "{name}"}
	msgs := runRecorded(t, a, fake)
	if len(msgs) == 0 || !strings.Contains(strings.Join(msgs, "|"), "resume") {
		t.Fatalf("a resume without the id must fail: %v", msgs)
	}
}
