// Package profile holds named agent profiles (generic settings each tool's
// adapter maps to its own flags), the per-project risk acknowledgement for
// flags that skip permission checks, and usage reporting (spec §9.7, §9.8).
package profile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/agents/adapter"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/workspace"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Keys are the generic profile settings; an adapter maps each to its tool's
// flags in [profile_flags].
var Keys = []string{"effort", "mcp_set", "model", "permission_mode", "sandbox"}

// Profile is one [profiles.<name>] table of the profiles file. Tools holds
// per-tool values ([profiles.<name>.<tool>]) that replace Settings for
// that tool.
type Profile struct {
	Name     string
	Settings map[string]string
	Tools    map[string]map[string]string
}

// LoadError is one problem in the profiles file; the profile it names is
// not loaded.
type LoadError struct {
	File    string
	Profile string
	Msg     string
}

func (e LoadError) Error() string {
	if e.Profile == "" {
		return e.File + ": " + e.Msg
	}
	return fmt.Sprintf("%s: profile %s: %s", e.File, e.Profile, e.Msg)
}

// nameRe is the profile (and tool) name rule: names reach project files,
// tables and output.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidName reports whether s is a usable profile name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Path is the profiles file: <Config>/agents/profiles.toml (apart from
// the config layers in <Config>/profiles/, which are something else).
func Path(dirs xdg.Dirs) string { return filepath.Join(dirs.Config, "agents", "profiles.toml") }

// OldPath is where the profiles file was first kept; it is still read
// when Path does not exist.
func OldPath(dirs xdg.Dirs) string { return filepath.Join(dirs.Config, "profiles.toml") }

// UsesOldPath reports whether profiles are read from OldPath.
func UsesOldPath(root fsx.Root, dirs xdg.Dirs) bool {
	return !root.Exists(Path(dirs)) && root.Exists(OldPath(dirs))
}

// Load reads the profiles file (Path, else OldPath); none is no profiles.
// A profile with an unknown key, a non-string value, a value a tool could
// misread as a flag or a bad tool table is reported and left out; a file
// that does not parse is one LoadError.
func Load(root fsx.Root, dirs xdg.Dirs) (map[string]Profile, []LoadError, error) {
	file := Path(dirs)
	if UsesOldPath(root, dirs) {
		file = OldPath(dirs)
	}
	data, err := root.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Profile{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var raw map[string]any
	if err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return map[string]Profile{}, []LoadError{{File: file, Msg: err.Error()}}, nil
	}
	out := map[string]Profile{}
	var lerrs []LoadError
	for _, k := range sortedKeys(raw) {
		if k != "profiles" {
			lerrs = append(lerrs, LoadError{File: file, Msg: "unknown key " + k +
				" (only [profiles.<name>] tables; i_understand_the_risk is set per project)"})
		}
	}
	tables, ok := raw["profiles"].(map[string]any)
	if _, present := raw["profiles"]; present && !ok {
		lerrs = append(lerrs, LoadError{File: file, Msg: "profiles must be a table of [profiles.<name>]"})
	}
	for _, name := range sortedKeys(tables) {
		if !ValidName(name) {
			lerrs = append(lerrs, LoadError{File: file, Profile: fmt.Sprintf("%q", name),
				Msg: "the name must match [a-z0-9][a-z0-9_-]{0,62}"})
			continue
		}
		t, ok := tables[name].(map[string]any)
		if !ok {
			lerrs = append(lerrs, LoadError{File: file, Profile: name, Msg: "must be a table"})
			continue
		}
		p, probs := decodeProfile(name, t)
		if len(probs) > 0 {
			for _, m := range probs {
				lerrs = append(lerrs, LoadError{File: file, Profile: name, Msg: m})
			}
			continue
		}
		out[name] = p
	}
	return out, lerrs, nil
}

// decodeSettings reads generic keys of m into into; prefix names a tool
// table in messages. Sub-tables are skipped (the caller reads them).
func decodeSettings(prefix string, m map[string]any, into map[string]string) []string {
	var probs []string
	for _, k := range sortedKeys(m) {
		if _, isTable := m[k].(map[string]any); isTable && prefix == "" {
			continue
		}
		if !isKey(k) {
			probs = append(probs, prefix+k+": unknown key (keys: "+strings.Join(Keys, ", ")+")")
			if prefix == "" {
				probs[len(probs)-1] = "unknown key " + k + " (keys: " + strings.Join(Keys, ", ") + ")"
			}
			continue
		}
		v, ok := m[k].(string)
		if !ok {
			probs = append(probs, prefix+k+": must be a string")
			continue
		}
		if err := CheckValue(v); err != nil {
			probs = append(probs, prefix+k+": "+err.Error())
			continue
		}
		into[k] = v
	}
	return probs
}

// decodeProfile reads one profile table: generic keys, and tool tables of
// generic keys.
func decodeProfile(name string, t map[string]any) (Profile, []string) {
	p := Profile{Name: name, Settings: map[string]string{}}
	probs := decodeSettings("", t, p.Settings)
	for _, k := range sortedKeys(t) {
		sub, isTable := t[k].(map[string]any)
		if !isTable {
			continue
		}
		if !ValidName(k) {
			probs = append(probs, fmt.Sprintf("%q is not a tool name", k))
			continue
		}
		if p.Tools == nil {
			p.Tools = map[string]map[string]string{}
		}
		p.Tools[k] = map[string]string{}
		probs = append(probs, decodeSettings(k+".", sub, p.Tools[k])...)
	}
	return p, probs
}

// CheckValue rejects a setting value a tool could misread: empty, too long,
// control characters, or a leading "-" (it would be taken as a flag).
func CheckValue(v string) error {
	switch {
	case v == "":
		return errors.New("must not be empty")
	case len(v) > 200:
		return errors.New("is longer than 200 bytes")
	case strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return errors.New("must not contain control characters")
	case strings.HasPrefix(v, "-"):
		return fmt.Errorf("%q must not start with \"-\"", v)
	}
	return nil
}

func isKey(k string) bool {
	for _, x := range Keys {
		if x == k {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Resolve picks the profile of a launch: the conversation's own, else the
// task's, else the project's default_profile; src names which ("" when
// none applies).
func Resolve(convProfile, taskProfile, projectDefault string) (name, src string) {
	switch {
	case convProfile != "":
		return convProfile, "conversation"
	case taskProfile != "":
		return taskProfile, "task"
	case projectDefault != "":
		return projectDefault, "project"
	}
	return "", ""
}

// Settings are p's settings for a's tool: the per-tool values over the
// generic ones. Nothing is added: without a profile setting, the tool's
// own configuration decides (its sandbox included).
func Settings(a adapter.Adapter, p Profile) map[string]string {
	s := map[string]string{}
	for k, v := range p.Settings {
		s[k] = v
	}
	for k, v := range p.Tools[a.Name] {
		s[k] = v
	}
	return s
}

// Args maps p to the tool's flags and lists the keys the tool cannot
// express, which are reported, not fatal.
func Args(a adapter.Adapter, p Profile) (args, unsupported []string) {
	return a.ProfileArgs(Settings(a, p))
}

// Check refuses a value the tool does not take (the adapter's
// profile_values), naming where a per-tool value goes.
func Check(a adapter.Adapter, p Profile) error {
	s := Settings(a, p)
	for _, k := range sortedKeys(s) {
		allowed, ok := a.ProfileValues[k]
		if !ok || len(a.ProfileFlags[k]) == 0 {
			continue
		}
		found := false
		for _, v := range allowed {
			found = found || v == s[k]
		}
		if !found {
			return fmt.Errorf("profile %s: %s does not take %s %q (one of: %s); set a value for it in [profiles.%s.%s]",
				p.Name, a.Name, k, s[k], strings.Join(allowed, ", "), p.Name, a.Name)
		}
	}
	return nil
}

// knownRisky are flags that skip permission checks or the sandbox in the
// built-in tools; they count for every adapter, so a user adapter that
// leaves risky_flags out cannot pass them unacknowledged. Any flag starting
// with --dangerously- counts as well.
var knownRisky = []string{
	"--allow-dangerously-skip-permissions",
	"--permission-mode bypassPermissions",
	"--yolo",
	"--approve-for-me",
	"--sandbox danger-full-access",
	"-s danger-full-access",
	"-c sandbox_mode=danger-full-access",
}

// option is one option of an argv in normal form: flag ("--x", "-x", or
// "config:<key>" for a -c/--config override) and its value, if any.
type option struct {
	flag, val string
	hasVal    bool
}

func isConfigFlag(f string) bool { return f == "-c" || f == "--config" }

// unquote strips one level of TOML-style quotes from a config value.
func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// configOption turns "key=value" of a config override into its option.
func configOption(kv string) (option, bool) {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return option{}, false
	}
	return option{flag: "config:" + strings.TrimSpace(k), val: unquote(v), hasVal: true}, true
}

// normalise lists every reading of args as options: "--x=v", "--x v",
// "-xv", "-x=v", "-x v" and config overrides "-c k=v", "-ck=v",
// "--config=k=v". A flag followed by a non-flag is read both with and
// without that value, so nothing is missed.
func normalise(args []string) []option {
	var out []option
	add := func(o option) {
		out = append(out, o)
		if isConfigFlag(o.flag) && o.hasVal {
			if co, ok := configOption(o.val); ok {
				out = append(out, co)
			}
		}
	}
	for i, arg := range args {
		next, hasNext := "", i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
		if hasNext {
			next = args[i+1]
		}
		switch {
		case strings.HasPrefix(arg, "--"):
			if f, v, ok := strings.Cut(arg, "="); ok {
				add(option{flag: f, val: v, hasVal: true})
				continue
			}
			add(option{flag: arg})
			if hasNext {
				add(option{flag: arg, val: next, hasVal: true})
			}
		case strings.HasPrefix(arg, "-") && len(arg) >= 2:
			f := arg[:2]
			if rest := strings.TrimPrefix(arg[2:], "="); rest != "" {
				add(option{flag: f, val: rest, hasVal: true})
				continue
			}
			add(option{flag: f})
			if hasNext {
				add(option{flag: f, val: next, hasVal: true})
			}
		}
	}
	return out
}

// parseEntry reads a risky_flags entry into the option it matches.
func parseEntry(e string) option {
	f, v, hasVal := strings.Cut(e, " ")
	if hasVal && isConfigFlag(f) {
		if co, ok := configOption(v); ok {
			return co
		}
	}
	return option{flag: f, val: v, hasVal: hasVal}
}

// RiskyArgs returns the risky flags (the adapter's risky_flags, the
// built-in list and any --dangerously-* flag) that occur in args, in any
// spelling: long or short, joined with "=" or not, or as a config
// override; values compare case-insensitively.
func RiskyArgs(a adapter.Adapter, args []string) []string {
	opts := normalise(args)
	var hits []string
	seen := map[string]bool{}
	hit := func(s string) {
		if !seen[s] {
			seen[s] = true
			hits = append(hits, s)
		}
	}
	for _, e := range append(append([]string{}, a.RiskyFlags...), knownRisky...) {
		want := parseEntry(e)
		for _, o := range opts {
			if o.flag == want.flag && (!want.hasVal || o.hasVal && strings.EqualFold(o.val, want.val)) {
				hit(e)
			}
		}
	}
	for _, o := range opts {
		if strings.HasPrefix(o.flag, "--dangerously-") {
			hit(o.flag)
		}
	}
	return hits
}

// RiskyFlags are the risky flags a launch with p would pass: in the mapped
// profile arguments or in the adapter's own commands.
func RiskyFlags(a adapter.Adapter, p Profile) []string {
	args, _ := Args(a, p)
	all := append([]string{}, args...)
	for _, c := range sortedKeys(a.Commands) {
		all = append(all, a.Commands[c]...)
	}
	return RiskyArgs(a, all)
}

// Risky reports whether launching a's tool with p passes a flag that skips
// its permission checks or sandbox.
func Risky(a adapter.Adapter, p Profile) bool { return len(RiskyFlags(a, p)) > 0 }

// Authorize refuses a risky launch unless the project sets
// i_understand_the_risk = true under [agents]. There is no global switch.
func Authorize(p Profile, a adapter.Adapter, project workspace.Project) error {
	hits := RiskyFlags(a, p)
	if len(hits) == 0 || project.Agents.IUnderstandTheRisk {
		return nil
	}
	what := "the " + a.Name + " adapter"
	if p.Name != "" {
		what = "profile " + p.Name
	}
	return fmt.Errorf("%s passes %s to %s, which skips its permission checks or sandbox; "+
		"it is allowed only in a project that sets i_understand_the_risk = true under [agents] "+
		"(project %s does not: cairn project edit %s)",
		what, strings.Join(hits, ", "), a.Name, project.ID, project.ID)
}

// Badge is the warning mark of a conversation launched with risky flags.
func Badge(risk bool) string {
	if risk {
		return "!"
	}
	return ""
}

// Sandbox is a tool's own sandbox setting, as its config file has it.
type Sandbox struct {
	File  string
	Value string
	// Risky is set when the value would be a risky flag if cairn passed it.
	Risky bool
}

// ToolSandbox reads the sandbox setting of a's tool from its own config
// (adapter sandbox_config), for agents doctor. ok is false when the
// adapter names no such file, or the file or key is absent. The file is
// only read, never written.
func ToolSandbox(root fsx.Root, dirs xdg.Dirs, a adapter.Adapter) (s Sandbox, ok bool, err error) {
	sc := a.SandboxConfig
	if sc.File == "" || sc.Key == "" {
		return Sandbox{}, false, nil
	}
	file := sc.File
	if rest, home := strings.CutPrefix(file, "~/"); home {
		file = filepath.Join(dirs.Home, rest)
	}
	data, err := root.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return Sandbox{}, false, nil
	}
	if err != nil {
		return Sandbox{}, false, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return Sandbox{}, false, fmt.Errorf("%s: %w", file, err)
	}
	v, isStr := m[sc.Key].(string)
	if !isStr {
		return Sandbox{}, false, nil
	}
	s = Sandbox{File: file, Value: v}
	if args, _ := a.ProfileArgs(map[string]string{"sandbox": v}); len(args) > 0 {
		s.Risky = len(RiskyArgs(a, args)) > 0
	}
	return s, true, nil
}
