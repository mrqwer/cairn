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

// Profile is one [profiles.<name>] table of <Config>/profiles.toml.
type Profile struct {
	Name     string
	Settings map[string]string
}

// LoadError is one problem in profiles.toml; the profile it names is not
// loaded.
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

// nameRe is the profile name rule (names reach project files and output).
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidName reports whether s is a usable profile name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Path is the profiles file.
func Path(dirs xdg.Dirs) string { return filepath.Join(dirs.Config, "profiles.toml") }

// Load reads <Config>/profiles.toml; a missing file is no profiles. A
// profile with an unknown key, a non-string value or a value a tool could
// misread as a flag is reported and left out; a file that does not parse
// is one LoadError.
func Load(root fsx.Root, dirs xdg.Dirs) (map[string]Profile, []LoadError, error) {
	file := Path(dirs)
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
		p := Profile{Name: name, Settings: map[string]string{}}
		var probs []string
		for _, k := range sortedKeys(t) {
			if !isKey(k) {
				probs = append(probs, "unknown key "+k+" (keys: "+strings.Join(Keys, ", ")+")")
				continue
			}
			v, ok := t[k].(string)
			if !ok {
				probs = append(probs, k+": must be a string")
				continue
			}
			if err := CheckValue(v); err != nil {
				probs = append(probs, k+": "+err.Error())
				continue
			}
			p.Settings[k] = v
		}
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

// Settings are the profile's settings over the adapter's defaults for the
// keys it leaves out (sandbox is on by default where the tool has it).
func Settings(a adapter.Adapter, p Profile) map[string]string {
	s := map[string]string{}
	for k, v := range a.ProfileDefaults {
		s[k] = v
	}
	for k, v := range p.Settings {
		s[k] = v
	}
	return s
}

// Args maps p to the tool's flags (with the adapter's defaults) and lists
// the keys the tool cannot express, which are reported, not fatal.
func Args(a adapter.Adapter, p Profile) (args, unsupported []string) {
	return a.ProfileArgs(Settings(a, p))
}

// knownRisky are flags that skip permission checks or the sandbox in the
// built-in tools; they count for every adapter, so a user adapter that
// leaves risky_flags out cannot pass them unacknowledged.
var knownRisky = []string{
	"--dangerously-skip-permissions",
	"--allow-dangerously-skip-permissions",
	"--permission-mode bypassPermissions",
	"--dangerously-bypass-approvals-and-sandbox",
	"--yolo",
	"--sandbox danger-full-access",
}

// RiskyArgs returns the risky flags (the adapter's risky_flags and the
// built-in list) that occur in args: "--flag" matches the argument or
// "--flag=…"; "--flag value" matches the two arguments or "--flag=value"
// (the value case-insensitively).
func RiskyArgs(a adapter.Adapter, args []string) []string {
	var hits []string
	seen := map[string]bool{}
	for _, rf := range append(append([]string{}, a.RiskyFlags...), knownRisky...) {
		flag, val, pair := strings.Cut(rf, " ")
		for i, arg := range args {
			var hit bool
			if pair {
				hit = arg == flag && i+1 < len(args) && strings.EqualFold(args[i+1], val) ||
					strings.HasPrefix(arg, flag+"=") && strings.EqualFold(arg[len(flag)+1:], val)
			} else {
				hit = arg == flag || strings.HasPrefix(arg, flag+"=")
			}
			if hit && !seen[rf] {
				seen[rf] = true
				hits = append(hits, rf)
			}
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
