package doctortest_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/doctor/doctortest"
	"github.com/mrqwer/cairn/internal/agents/testtool"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

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
			doctortest.Conformance(t, a, fake)
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
		doctortest.Conformance(r, a, fake)
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
