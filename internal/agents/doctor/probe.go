// Package doctor probes installed AI tools against their adapters without
// side effects (`cairn agents doctor`), and holds the adapter conformance
// suite that drives an adapter against a fake tool.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/agents/hooks"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Finding levels.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelFail = "fail"
	LevelSkip = "skip"
)

// Finding is one probe result. Level is ok, warn, fail or skip.
type Finding struct {
	Tool   string `json:"tool"`
	Check  string `json:"check"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// Timeout bounds each command a probe runs.
var Timeout = 5 * time.Second

// probeConvID and probeSelf only fill the hook file a probe generates in a
// temp dir; nothing runs them.
const (
	probeConvID = "doctor-probe"
	probeSelf   = "cairn"
)

type prober struct {
	ctx     context.Context
	a       adapter.Adapter
	run     execx.Runner
	out     []Finding
	version string
}

func (p *prober) add(check, level, format string, args ...any) {
	p.out = append(p.out, Finding{Tool: p.a.Name, Check: check, Level: level, Detail: fmt.Sprintf(format, args...)})
}

// Probe checks adapter a against the installed tool without side effects.
// It runs only the version check, the adapter's help commands (probe_flags,
// each guaranteed to contain a help argument) and the list command, each
// directly (no shell) and bounded by Timeout; it never starts a
// conversation. Hook files are generated in a temp dir that is removed.
// A tool that is not installed is one skip finding, not a failure.
func Probe(ctx context.Context, a adapter.Adapter, run execx.Runner, lookPath func(string) (string, error)) []Finding {
	p := &prober{ctx: ctx, a: a, run: run, version: "unknown version"}
	bin := a.Binary
	if bin == "" && len(a.Commands["start"]) > 0 {
		bin = a.Commands["start"][0]
	}
	if bin == "" {
		p.add("binary", LevelSkip, "not installed")
		return p.out
	}
	path, err := lookPath(bin)
	if err != nil {
		p.add("binary", LevelSkip, "not installed")
		return p.out
	}
	p.add("binary", LevelOK, "%s", path)
	p.checkVersion()
	p.checkCapabilities()
	p.checkDiscovery()
	p.checkHooks()
	missing := p.checkHelp()
	p.checkList(missing)
	return p.out
}

var errTimeout = errors.New("timed out")

// exec runs argv bounded by Timeout and returns its output.
func (p *prober) exec(argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(p.ctx, Timeout)
	defer cancel()
	out, err := p.run.Run(ctx, argv[0], argv[1:]...)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), fmt.Errorf("%w after %s", errTimeout, Timeout)
	}
	return string(out), err
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > 200 {
				l = string(r[:200]) + "…"
			}
			return l
		}
	}
	return ""
}

func (p *prober) checkVersion() {
	vc := p.a.VersionCheck
	if len(vc) == 0 {
		p.add("version", LevelSkip, "no version_check declared")
		return
	}
	out, err := p.exec(vc)
	v := firstLine(out)
	if err != nil {
		detail := fmt.Sprintf("%s failed: %v", strings.Join(vc, " "), err)
		if v != "" {
			detail += ": " + v
		}
		p.add("version", LevelFail, "%s", detail)
		return
	}
	if v == "" {
		p.add("version", LevelWarn, "%s printed nothing", strings.Join(vc, " "))
		return
	}
	p.version = v
	p.add("version", LevelOK, "%s", v)
}

func (p *prober) checkCapabilities() {
	c := p.a.Capabilities
	var bad []string
	if len(p.a.Commands["start"]) == 0 {
		bad = append(bad, "commands.start is missing")
	}
	for _, r := range []struct {
		on        bool
		capn, cmd string
	}{
		{c.ResumeByID, "resume_by_id", "resume"}, {c.ResumeByName, "resume_by_name", "resume"},
		{c.Fork, "fork", "fork"}, {c.ListJSON, "list_json", "list"}, {c.Delete, "delete", "delete"},
	} {
		if r.on && len(p.a.Commands[r.cmd]) == 0 {
			bad = append(bad, fmt.Sprintf("%s is declared but commands.%s is missing", r.capn, r.cmd))
		}
	}
	for _, b := range bad {
		p.add("capabilities", LevelFail, "%s", b)
	}
	if len(bad) == 0 {
		p.add("capabilities", LevelOK, "every declared capability has its command")
	}
}

func (p *prober) checkDiscovery() {
	c := p.a.Capabilities
	if c.PreassignID {
		return
	}
	switch c.Discovery {
	case "dir-diff", "state-file":
		if p.a.History.Locator == "" {
			p.add("discovery", LevelWarn, "discovery %s needs history.locator: the tool's conversation id cannot be found, so resume by id is unavailable", c.Discovery)
			return
		}
	case "list":
		if !c.ListJSON {
			p.add("discovery", LevelWarn, "discovery list needs capabilities.list_json: the tool's conversation id cannot be found")
			return
		}
	default:
		return
	}
	p.add("discovery", LevelOK, "discovery %s is configured", c.Discovery)
}

func (p *prober) checkHooks() {
	h := p.a.Hooks
	switch p.a.Capabilities.Hooks {
	case "settings-file":
		p.checkSettingsFile()
		p.add("hooks.delivery", LevelSkip, "not probed (needs a running conversation): whether the tool merges the %s file with the user's own settings and fires %s; until a hook arrives the state is observed",
			h.Flag, strings.Join(sortedKeys(h.Events), ", "))
	case "config-override":
		p.checkOverride()
		p.add("hooks.delivery", LevelSkip, "not probed (needs a running conversation): whether %s %s=… applies to this process only and posts %s; until a hook arrives the state is observed",
			h.Flag, h.Key, h.Kind)
	default:
		p.add("hooks", LevelSkip, "no hooks: the state is observed from the pane")
	}
}

// checkSettingsFile generates the hook settings file in a temp dir that is
// removed afterwards, and checks it is valid JSON with every event.
func (p *prober) checkSettingsFile() {
	tmp, err := os.MkdirTemp("", "cairn-doctor-")
	if err != nil {
		p.add("hooks", LevelFail, "cannot create a temp dir: %v", err)
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	args, err := hooks.Inject(p.a, probeConvID, probeSelf, xdg.Dirs{State: tmp}, fsx.Root{Dir: "/"})
	if err != nil {
		p.add("hooks", LevelFail, "cannot generate the hook file: %v", err)
		return
	}
	if len(args) != 2 {
		p.add("hooks", LevelFail, "hook arguments %q are not [flag, file]", args)
		return
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		p.add("hooks", LevelFail, "cannot read the generated hook file: %v", err)
		return
	}
	if err := checkSettingsDoc(data, p.a.Hooks.Events); err != nil {
		p.add("hooks", LevelFail, "the generated hook file %v", err)
		return
	}
	p.add("hooks", LevelOK, "the generated hook file is valid JSON (%d events)", len(p.a.Hooks.Events))
}

type settingsDoc struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// checkSettingsDoc checks a claude-settings@1 file has exactly the events,
// each with one command hook.
func checkSettingsDoc(data []byte, events map[string]string) error {
	var doc settingsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("is not valid JSON: %v", err)
	}
	for e := range events {
		g := doc.Hooks[e]
		if len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0].Type != "command" || g[0].Hooks[0].Command == "" {
			return fmt.Errorf("has no command hook for %s", e)
		}
	}
	if len(doc.Hooks) != len(events) {
		return fmt.Errorf("has %d events, want %d", len(doc.Hooks), len(events))
	}
	return nil
}

// checkOverride checks the config override value parses as a TOML array.
func (p *prober) checkOverride() {
	args, err := hooks.Inject(p.a, probeConvID, probeSelf, xdg.Dirs{}, fsx.Root{Dir: "/"})
	if err != nil {
		p.add("hooks", LevelFail, "cannot build the hook override: %v", err)
		return
	}
	if len(args) != 2 {
		p.add("hooks", LevelFail, "hook arguments %q are not [flag, key=value]", args)
		return
	}
	key, _, err := parseOverride(args[1])
	if err != nil {
		p.add("hooks", LevelFail, "the hook override %s %v", args[1], err)
		return
	}
	p.add("hooks", LevelOK, "the hook override %s=[…] parses as a TOML array", key)
}

// parseOverride splits key=<TOML array of strings>.
func parseOverride(kv string) (string, []string, error) {
	key, val, ok := strings.Cut(kv, "=")
	if !ok {
		return "", nil, errors.New("has no '='")
	}
	var v struct{ V []string }
	if err := toml.Unmarshal([]byte("V = "+val), &v); err != nil {
		return "", nil, fmt.Errorf("is not a TOML array of strings: %v", err)
	}
	if len(v.V) == 0 {
		return "", nil, errors.New("is an empty array")
	}
	return key, v.V, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hasWord reports whether text mentions w as a whole word (--name is not
// found in --names or --session-name).
func hasWord(text, w string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_-])`).MatchString(text)
}

// usedFlags lists the flags of the command and profile flag templates in a
// stable order, without values ("--x=v" is "--x").
func (p *prober) usedFlags() []string {
	var flags []string
	seen := map[string]bool{}
	addFrom := func(tpl []string, from int) {
		for _, e := range tpl[min(from, len(tpl)):] {
			if len(e) < 2 || e[0] != '-' || (e[1] >= '0' && e[1] <= '9') {
				continue
			}
			f, _, _ := strings.Cut(e, "=")
			if f == "--" || strings.ContainsAny(f, "{}") || seen[f] {
				continue
			}
			seen[f] = true
			flags = append(flags, f)
		}
	}
	for _, n := range sortedKeys(p.a.Commands) {
		addFrom(p.a.Commands[n], 1)
	}
	for _, n := range sortedKeys(p.a.ProfileFlags) {
		addFrom(p.a.ProfileFlags[n], 0)
	}
	return flags
}

// subcommands are the literal words right after the binary in the command
// templates.
func (p *prober) subcommands() []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range sortedKeys(p.a.Commands) {
		tpl := p.a.Commands[n]
		if len(tpl) < 2 {
			continue
		}
		w := tpl[1]
		if w == "" || w[0] == '-' || strings.ContainsAny(w, "{}") || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// checkHelp reads the help commands and reports the flags and subcommands
// the commands use that the help does not mention. It returns the missing
// subcommands.
func (p *prober) checkHelp() map[string]bool {
	if len(p.a.ProbeFlags) == 0 {
		p.add("help", LevelSkip, "no probe_flags declared: flags are not checked")
		return nil
	}
	texts := make([]string, len(p.a.ProbeFlags))
	readAll := true
	for i, argv := range p.a.ProbeFlags {
		out, err := p.exec(argv)
		// Many tools exit non-zero after printing help: the text counts.
		if strings.TrimSpace(out) == "" || errors.Is(err, errTimeout) {
			reason := "it printed nothing"
			if err != nil {
				reason = err.Error()
			}
			p.add("help", LevelWarn, "cannot read %s of %s: %s", strings.Join(argv, " "), p.version, reason)
			readAll = false
			continue
		}
		texts[i] = out
	}
	if texts[0] == "" {
		return nil
	}
	all := strings.Join(texts, "\n")
	hookFlag := ""
	if p.a.Capabilities.Hooks == "settings-file" || p.a.Capabilities.Hooks == "config-override" {
		hookFlag = p.a.Hooks.Flag
	}
	for _, f := range p.usedFlags() {
		if f != hookFlag && !hasWord(all, f) {
			p.add("flag", LevelWarn, "flag %s not in help of %s", f, p.version)
		}
	}
	if hookFlag != "" && !hasWord(texts[0], hookFlag) {
		p.add("flag", LevelWarn, "flag %s not in top-level help of %s (cairn passes it before any subcommand)", hookFlag, p.version)
	}
	missing := map[string]bool{}
	for _, s := range p.subcommands() {
		if !hasWord(texts[0], s) {
			missing[s] = true
			p.add("subcommand", LevelWarn, "subcommand %s not in help of %s", s, p.version)
		}
	}
	if readAll {
		p.add("help", LevelOK, "read the help of %s", p.version)
	}
	return missing
}

// checkList runs the list command and checks its output parses as JSON (an
// empty output is no conversations). It is not run when its subcommand is
// missing from the help, where the word could be taken as a prompt.
func (p *prober) checkList(missing map[string]bool) {
	if !p.a.Capabilities.ListJSON || len(p.a.Commands["list"]) == 0 {
		return
	}
	argv, err := p.a.Render("list", adapter.Vars{})
	if err != nil {
		p.add("list", LevelFail, "%v", err)
		return
	}
	if len(argv) > 1 && missing[argv[1]] {
		p.add("list", LevelWarn, "not run: subcommand %s not in help of %s", argv[1], p.version)
		return
	}
	out, err := p.exec(argv)
	if err != nil {
		detail := fmt.Sprintf("%s failed: %v", strings.Join(argv, " "), err)
		if l := firstLine(out); l != "" {
			detail += ": " + l
		}
		p.add("list", LevelFail, "%s", detail)
		return
	}
	if strings.TrimSpace(out) == "" {
		p.add("list", LevelOK, "list output is empty (no conversations)")
		return
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		p.add("list", LevelFail, "list output of %s is not JSON: %v", p.version, err)
		return
	}
	p.add("list", LevelOK, "list output parses as JSON")
}
