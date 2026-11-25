// Package doctortest is the adapter conformance suite: it drives an adapter
// against a fake tool (internal/agents/testtool). It is a separate package
// so that the cairn binary does not link the testing package.
package doctortest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mrqwer/cairn/internal/agents"
	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/conv"
	"github.com/mrqwer/cairn/internal/agents/doctor"
	"github.com/mrqwer/cairn/internal/agents/hooks"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Values the conformance suite renders: a name with spaces and quotes must
// reach the tool as one argument.
const (
	confConvID   = "conf01"
	confNativeID = "0b9d6f1e-6c1a-4f3e-9d2b-7a5c3e1f2d40"
	confNewID    = "5e2a7c9d-1b3f-4e6a-8c0d-9f1e2b3a4c5d"
	confName     = `conformance "name" with spaces`
)

// Conformance drives adapter a against fake, a testtool script (see
// internal/agents/testtool) whose log is fake+".log": start, resume, fork,
// list and delete are rendered as cairn renders them (profile arguments
// spliced, hook arguments right after the binary), run directly with the
// fake in place of the binary, and the argv the fake received is checked.
// The hook settings file or override is generated and its command is run
// the way the tool runs it, against a stand-in cairn that records its argv.
func Conformance(t testing.TB, a adapter.Adapter, fake string) {
	t.Helper()
	if errs := a.Validate(); len(errs) > 0 {
		t.Fatalf("%s: adapter is invalid: %v", a.Name, errs)
	}
	checkBinary(t, a)
	cf := conformance{t: t, a: a, fake: fake, log: fake + ".log", dir: t.TempDir()}

	settings := map[string]string{}
	for k := range a.ProfileFlags {
		settings[k] = "v " + k
	}
	pargs, unsupported := a.ProfileArgs(settings)
	if len(unsupported) > 0 {
		t.Errorf("%s: profile flags %v do not render", a.Name, unsupported)
	}
	hookArgs, self := cf.hooks()

	c := conv.Conversation{ID: confConvID, Name: confName, NativeID: confNativeID, Cwd: cf.dir, Tool: a.Name}
	start := c
	if !a.Capabilities.PreassignID {
		start.NativeID = ""
	}
	argv, _, err := agents.BuildStart(a, start, pargs)
	if err != nil {
		t.Fatalf("%s: start: %v", a.Name, err)
	}
	argv = agents.WithHookArgs(argv, hookArgs)
	cf.expect("start", argv, hookArgs, pargs)
	if a.Capabilities.NameAtStart {
		cf.contains("start", argv, confName)
	}
	if a.Capabilities.PreassignID {
		cf.contains("start", argv, confNativeID)
	}
	cf.drive("start", argv)

	if a.Capabilities.ResumeByID {
		argv, err := agents.BuildResume(a, c, pargs)
		if err != nil {
			t.Fatalf("%s: resume: %v", a.Name, err)
		}
		argv = agents.WithHookArgs(argv, hookArgs)
		cf.expect("resume", argv, hookArgs, pargs)
		cf.contains("resume", argv, confNativeID)
		cf.drive("resume", argv)
	}
	if a.Capabilities.ResumeByName {
		byName := c
		byName.NativeID = ""
		argv, err := agents.BuildResume(a, byName, pargs)
		if err != nil {
			t.Fatalf("%s: resume by name: %v", a.Name, err)
		}
		if !a.Capabilities.ResumeByID {
			cf.contains("resume by name", argv, confName)
		}
		cf.drive("resume by name", argv)
	}
	if a.Capabilities.Fork {
		argv, err := a.Render("fork", adapter.Vars{ID: confNativeID, NativeID: confNativeID, NewID: confNewID, Name: confName})
		if err != nil {
			t.Fatalf("%s: fork: %v", a.Name, err)
		}
		cf.expect("fork", argv, nil, nil)
		cf.contains("fork", argv, confNativeID)
		cf.contains("fork", argv, confNewID)
		cf.drive("fork", argv)
	}
	if a.Capabilities.ListJSON {
		argv, err := a.Render("list", adapter.Vars{})
		if err != nil {
			t.Fatalf("%s: list: %v", a.Name, err)
		}
		cf.expect("list", argv, nil, nil)
		out := cf.drive("list", argv)
		if !jsonOrEmpty(out) {
			t.Errorf("%s: list output %q is not JSON", a.Name, out)
		}
	}
	if a.Capabilities.Delete {
		argv, err := agents.BuildDelete(a, c)
		if err != nil {
			t.Fatalf("%s: delete: %v", a.Name, err)
		}
		cf.expect("delete", argv, nil, nil)
		cf.contains("delete", argv, confNativeID)
		cf.drive("delete", argv)
	}
	cf.runHook(self)
}

// checkBinary: the doctor looks up a.Binary, so every command must launch it.
func checkBinary(t testing.TB, a adapter.Adapter) {
	t.Helper()
	if a.Binary == "" {
		return
	}
	if len(a.VersionCheck) > 0 && a.VersionCheck[0] != a.Binary {
		t.Errorf("%s: version_check runs %q, not the binary %q", a.Name, a.VersionCheck[0], a.Binary)
	}
	for _, n := range sortedKeys(a.Commands) {
		if tpl := a.Commands[n]; len(tpl) > 0 && tpl[0] != a.Binary {
			t.Errorf("%s: commands.%s runs %q, not the binary %q", a.Name, n, tpl[0], a.Binary)
		}
	}
	for i, h := range a.ProbeFlags {
		if h[0] != a.Binary {
			t.Errorf("%s: probe_flags[%d] runs %q, not the binary %q", a.Name, i, h[0], a.Binary)
		}
	}
}

type conformance struct {
	t              testing.TB
	a              adapter.Adapter
	fake, log, dir string
	hookKinds      []string // kinds the hook commands must post, in event order
	hookCmds       []string // settings-file: shell commands; config-override: one argv as TOML
}

// expect checks an argv before it runs: no placeholder is left, hook
// arguments come right after the binary, profile arguments are contiguous.
func (cf *conformance) expect(what string, argv, hookArgs, pargs []string) {
	cf.t.Helper()
	for _, e := range argv {
		if strings.Contains(e, "{") && strings.Contains(e, "}") && e != confName {
			cf.t.Errorf("%s %s: unresolved placeholder in %q", cf.a.Name, what, argv)
		}
	}
	if len(hookArgs) > 0 && (len(argv) < 1+len(hookArgs) || !reflect.DeepEqual(argv[1:1+len(hookArgs)], hookArgs)) {
		cf.t.Errorf("%s %s: hook arguments %q must follow the binary: %q", cf.a.Name, what, hookArgs, argv)
	}
	if len(pargs) > 0 && !containsSeq(argv, pargs) {
		cf.t.Errorf("%s %s: profile arguments %q not spliced into %q", cf.a.Name, what, pargs, argv)
	}
}

func (cf *conformance) contains(what string, argv []string, want string) {
	cf.t.Helper()
	for _, e := range argv {
		if e == want {
			return
		}
	}
	cf.t.Errorf("%s %s: %q is not one argument of %q", cf.a.Name, what, want, argv)
}

func containsSeq(s, sub []string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if reflect.DeepEqual(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// drive runs argv with the fake in place of the binary (directly, no
// shell) and checks the fake received exactly argv[1:]. It returns the
// output.
func (cf *conformance) drive(what string, argv []string) string {
	cf.t.Helper()
	out, err := execx.OSRunner{Dir: cf.dir}.Run(context.Background(), cf.fake, argv[1:]...)
	if err != nil {
		cf.t.Fatalf("%s %s: running the fake: %v: %s", cf.a.Name, what, err, out)
	}
	calls := readCalls(cf.t, cf.log)
	if len(calls) == 0 {
		cf.t.Fatalf("%s %s: the fake logged no call", cf.a.Name, what)
	}
	if got := calls[len(calls)-1]; !reflect.DeepEqual(got, argv[1:]) {
		cf.t.Errorf("%s %s: the tool received %q, want %q", cf.a.Name, what, got, argv[1:])
	}
	return string(out)
}

// readCalls parses the argv of each call from a testtool log.
func readCalls(t testing.TB, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("reading the fake's log: %v", err)
	}
	var calls [][]string
	var cur []string
	in := false
	for _, l := range strings.Split(string(data), "\n") {
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

func jsonOrEmpty(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// hooks generates the hook arguments as a pane does, with a stand-in cairn
// (in a directory with a space, which must survive the shell) that records
// its argv; it returns the arguments and the stand-in's log.
func (cf *conformance) hooks() (args []string, selfLog string) {
	t, a := cf.t, cf.a
	t.Helper()
	if a.Capabilities.Hooks != "settings-file" && a.Capabilities.Hooks != "config-override" {
		return nil, ""
	}
	binDir := filepath.Join(t.TempDir(), "cairn bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(binDir, "cairn")
	selfLog = filepath.Join(binDir, "argv.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> " + shQuote(selfLog) + "\necho >> " + shQuote(selfLog) + "\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil { //nolint:gosec // stand-in executable
		t.Fatal(err)
	}
	state := t.TempDir()
	args, err := hooks.Inject(a, confConvID, self, xdg.Dirs{State: state}, fsx.Root{Dir: "/"})
	if err != nil {
		t.Fatalf("%s: hooks: %v", a.Name, err)
	}
	if len(args) != 2 || args[0] != a.Hooks.Flag {
		t.Fatalf("%s: hook arguments %q are not [%s, value]", a.Name, args, a.Hooks.Flag)
	}
	switch a.Capabilities.Hooks {
	case "settings-file":
		if !strings.HasPrefix(args[1], state+string(filepath.Separator)) {
			t.Errorf("%s: hook file %s is outside cairn's state dir", a.Name, args[1])
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			t.Fatal(err)
		}
		cmds, err := doctor.SettingsCommands(data, a.Hooks.Events)
		if err != nil {
			t.Fatalf("%s: hook file %v", a.Name, err)
		}
		for _, e := range sortedKeys(a.Hooks.Events) {
			cf.hookKinds = append(cf.hookKinds, a.Hooks.Events[e])
			cf.hookCmds = append(cf.hookCmds, cmds[e])
		}
	case "config-override":
		key, argv, err := doctor.ParseOverride(args[1])
		if err != nil || key != a.Hooks.Key {
			t.Fatalf("%s: hook override %q: key %q, %v", a.Name, args[1], key, err)
		}
		if want := hooks.EventArgv(self, confConvID, a.Hooks.Kind); !reflect.DeepEqual(argv, want) {
			t.Errorf("%s: hook override runs %q, want %q", a.Name, argv, want)
		}
		cf.hookKinds = []string{a.Hooks.Kind}
		cf.hookCmds = []string{strings.Join(argv, "\x00")}
	}
	return args, selfLog
}

// runHook runs each hook command the way the tool does (settings-file: a
// shell command; config-override: an argv plus a JSON payload argument) and
// checks the stand-in cairn received `event --conversation <id> --kind <k>`.
func (cf *conformance) runHook(selfLog string) {
	t, a := cf.t, cf.a
	t.Helper()
	if selfLog == "" {
		return
	}
	for i, cmd := range cf.hookCmds {
		var err error
		var out []byte
		if a.Capabilities.Hooks == "settings-file" {
			out, err = execx.OSRunner{Dir: cf.dir}.Run(context.Background(), "sh", "-c", cmd)
		} else {
			argv := strings.Split(cmd, "\x00")
			out, err = execx.OSRunner{Dir: cf.dir}.Run(context.Background(), argv[0], append(argv[1:], `{"type":"agent-turn-complete"}`)...)
		}
		if err != nil {
			t.Fatalf("%s: running hook %d: %v: %s", a.Name, i, err, out)
		}
	}
	data, err := os.ReadFile(selfLog)
	if err != nil {
		t.Fatalf("%s: the hook did not run cairn: %v", a.Name, err)
	}
	runs := strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n")
	if len(runs) != len(cf.hookKinds) {
		t.Fatalf("%s: %d hook runs, want %d: %q", a.Name, len(runs), len(cf.hookKinds), data)
	}
	for i, r := range runs {
		got := strings.Split(r, "\n")
		want := hooks.EventArgv("", confConvID, cf.hookKinds[i])[1:]
		if len(got) < len(want) || !reflect.DeepEqual(got[:len(want)], want) {
			t.Errorf("%s: hook %d ran cairn with %q, want %q first", a.Name, i, got, want)
		}
	}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
