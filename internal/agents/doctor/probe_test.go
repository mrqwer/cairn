package doctor_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/doctor"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/execx"
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
	if f := one(t, fs, "list"); f.Level != "skip" || !strings.Contains(f.Detail, "not run") {
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

// listCalls are the logged calls of the list subcommand.
func listCalls(t *testing.T, log string) [][]string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var out [][]string
	for _, c := range parseCalls(string(data)) {
		if len(c) > 0 && c[0] == "agents" && !slices.Contains(c, "--help") {
			out = append(out, c)
		}
	}
	return out
}

// Without help to check the list subcommand against, list is not run.
func TestProbeListNotRunWithoutProbeFlags(t *testing.T) {
	isolate(t)
	bin, log := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: `[]`})
	a := fakeAdapter(bin)
	a.ProbeFlags = nil
	fs := doctor.Probe(context.Background(), a, doctor.Runner{}, exec.LookPath)
	if f := one(t, fs, "list"); f.Level != "skip" || !strings.Contains(f.Detail, "not run") {
		t.Fatalf("list %+v", f)
	}
	if c := listCalls(t, log); len(c) != 0 {
		t.Fatalf("list ran: %v", c)
	}
}

func TestProbeListNotRunWhenHelpUnreadable(t *testing.T) {
	isolate(t)
	bin, log := fakeTool(t, testtool.Spec{Help: "", ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), doctor.Runner{}, exec.LookPath)
	if f := one(t, fs, "list"); f.Level != "skip" || !strings.Contains(f.Detail, "not run") {
		t.Fatalf("list %+v", f)
	}
	if c := listCalls(t, log); len(c) != 0 {
		t.Fatalf("list ran: %v", c)
	}
}

// A subcommand mentioned only in prose (or in a wrapped description) is
// not listed: list is not run.
func TestProbeSubcommandInProseIsNotListed(t *testing.T) {
	isolate(t)
	help := strings.Replace(fullHelp, "  agents [options]   --json --all\n",
		"Run fake agents to see sessions.\n                    agents, styles and more\n  --json --all\n", 1)
	bin, log := fakeTool(t, testtool.Spec{Version: "fake 5", Help: help, ListJSON: `[]`})
	fs := doctor.Probe(context.Background(), fakeAdapter(bin), doctor.Runner{}, exec.LookPath)
	if f := one(t, fs, "subcommand"); f.Level != "warn" {
		t.Fatalf("subcommand %+v", f)
	}
	if c := listCalls(t, log); len(c) != 0 {
		t.Fatalf("list ran: %v", c)
	}
}

// A version_check that does not look like a version query is not run.
func TestProbeVersionCheckMustLookLikeVersion(t *testing.T) {
	isolate(t)
	bin, log := fakeTool(t, testtool.Spec{Help: fullHelp, ListJSON: `[]`})
	a := fakeAdapter(bin)
	a.VersionCheck = []string{bin, "chat"}
	fs := doctor.Probe(context.Background(), a, doctor.Runner{}, exec.LookPath)
	if f := one(t, fs, "version"); f.Level != "skip" || !strings.Contains(f.Detail, "not run") {
		t.Fatalf("version %+v", f)
	}
	data, _ := os.ReadFile(log)
	for _, c := range parseCalls(string(data)) {
		if slices.Contains(c, "chat") {
			t.Fatalf("version_check ran: %v", c)
		}
	}
}

// A wrapper that leaves a grandchild holding the output pipe is killed
// with its whole process group, within the timeout plus a small bound.
func TestProbeTimeoutKillsGrandchildren(t *testing.T) {
	isolate(t)
	old := doctor.Timeout
	t.Cleanup(func() { doctor.Timeout = old })
	doctor.Timeout = 200 * time.Millisecond
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := filepath.Join(dir, "wrapper")
	script := "#!/bin/sh\nsleep 30 &\necho $! > '" + pidFile + "'\nwait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	a := adapter.Adapter{Name: "wrap", Binary: bin, VersionCheck: []string{bin, "--version"},
		Capabilities: adapter.Capabilities{Discovery: "none"}, Commands: map[string][]string{"start": {bin}}}
	start := time.Now()
	fs := doctor.Probe(context.Background(), a, doctor.Runner{}, exec.LookPath)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("probe took %v (a grandchild kept the pipe open)", d)
	}
	if f := one(t, fs, "version"); f.Level != "fail" || !strings.Contains(f.Detail, "timed out") {
		t.Fatalf("version %+v", f)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMinimalEnv(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin", "HOME": "/home/u", "LANG": "C.UTF-8", "API_TOKEN": "x", "XDG_CONFIG_HOME": "/c"}
	got := doctor.MinimalEnv(func(k string) string { return env[k] }, "/tmp/p")
	want := []string{"PATH=/usr/bin", "HOME=/home/u", "LANG=C.UTF-8"}
	if !slices.Equal(got, want) {
		t.Fatalf("env %q, want %q", got, want)
	}
	delete(env, "HOME")
	delete(env, "LANG")
	got = doctor.MinimalEnv(func(k string) string { return env[k] }, "/tmp/p")
	if want := []string{"PATH=/usr/bin", "HOME=/tmp/p"}; !slices.Equal(got, want) {
		t.Fatalf("env %q, want %q", got, want)
	}
}

// Failure output can echo secrets: the detail is redacted and short.
func TestProbeFailureDetailRedacted(t *testing.T) {
	isolate(t)
	bin := filepath.Join(t.TempDir(), "leaky")
	secret := "sk-ant-abcdefghijklmnopqrstuvwxyz0123456789"
	script := "#!/bin/sh\necho 'error: bad api_key=hunter2hunter2 token " + secret + " at https://user:pw@example.com/x " + strings.Repeat("y", 400) + "'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	a := adapter.Adapter{Name: "leaky", Binary: bin, VersionCheck: []string{bin, "--version"},
		Capabilities: adapter.Capabilities{Discovery: "none"}, Commands: map[string][]string{"start": {bin}}}
	f := one(t, doctor.Probe(context.Background(), a, doctor.Runner{}, exec.LookPath), "version")
	if f.Level != "fail" {
		t.Fatalf("version %+v", f)
	}
	for _, leak := range []string{"hunter2", secret, "user:pw", "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"} {
		if strings.Contains(f.Detail, leak) {
			t.Fatalf("detail leaks %q: %s", leak, f.Detail)
		}
	}
	if !strings.Contains(f.Detail, "[redacted]") {
		t.Fatalf("detail %s", f.Detail)
	}
}
