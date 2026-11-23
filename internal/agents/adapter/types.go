// Package adapter describes AI coding tools as data: a TOML file per tool is
// loaded, validated and rendered into argv. No tool name appears in engine code.
package adapter

// Adapter is one tool description (spec §9.1).
type Adapter struct {
	Name         string              `toml:"name"`
	Binary       string              `toml:"binary"`
	VersionCheck []string            `toml:"version_check"`
	Capabilities Capabilities        `toml:"capabilities"`
	Commands     map[string][]string `toml:"commands"`
	ProfileFlags map[string][]string `toml:"profile_flags"`
	// ProfileValues lists, per generic profile key, the values the tool
	// takes; a profile with another value is refused before anything is
	// created (a profile may give per-tool values in [profiles.<n>.<tool>]).
	ProfileValues map[string][]string `toml:"profile_values"`
	// SandboxConfig names the tool's own config file and key that set its
	// sandbox, read only by `agents doctor` to report it. cairn passes a
	// sandbox flag only when a profile sets one.
	SandboxConfig SandboxConfig `toml:"sandbox_config"`
	// Usage is the usage source of the tool's conversations: "" (none) or
	// "ccusage".
	Usage string `toml:"usage"`
	// RiskyFlags are arguments that skip the tool's permission checks or
	// sandbox, as "--flag", "--flag value", "-f value" or "-c key=value"
	// (matched in every spelling); a profile or command using one needs
	// i_understand_the_risk in the project.
	RiskyFlags []string   `toml:"risky_flags"`
	Env        Env        `toml:"env"`
	History    History    `toml:"history"`
	Hooks      HookConfig `toml:"hooks"`
	// ProbeFlags are help commands (each must contain --help, -h or help)
	// whose output `cairn agents doctor` searches for the flags and
	// subcommands the commands use; the first is the top-level help, where
	// options placed right after the binary (hooks) must appear.
	ProbeFlags [][]string `toml:"probe_flags"`
}

// SandboxConfig is where a tool keeps its sandbox setting: File ("~/" is
// the home directory) and a top-level Key in it (TOML).
type SandboxConfig struct {
	File string `toml:"file"`
	Key  string `toml:"key"`
}

// Capabilities declares what a tool can do; cairn fills the gaps.
type Capabilities struct {
	NameAtStart  bool `toml:"name_at_start"`
	PreassignID  bool `toml:"preassign_id"`
	ResumeByID   bool `toml:"resume_by_id"`
	ResumeByName bool `toml:"resume_by_name"`
	Fork         bool `toml:"fork"`
	ListJSON     bool `toml:"list_json"`
	Delete       bool `toml:"delete"`
	// Hooks is none | settings-file | config-override.
	Hooks string `toml:"hooks"`
	// Discovery is none | dir-diff | list | state-file; required when the tool
	// neither pre-assigns an id nor takes a name at start.
	Discovery string `toml:"discovery"`
}

// Env adjusts the environment of the launched tool.
type Env struct {
	Unset []string          `toml:"unset"`
	Set   map[string]string `toml:"set"`
}

// History says where a tool keeps transcripts and which parser reads them.
type History struct {
	Locator string `toml:"locator"`
	Parser  string `toml:"parser"`
	// SecretFields are field names whose values the index always masks
	// (as "name: value", "name=value" or a JSON member), on top of the
	// built-in secret patterns.
	SecretFields []string `toml:"secret_fields"`
}

// HookFormatClaudeSettings is the settings-file layout of a JSON file with
// {"hooks": {<Event>: [{"hooks": [{"type": "command", "command": ...}]}]}}.
const HookFormatClaudeSettings = "claude-settings@1"

// HookConfig says how cairn's event hooks are passed to a tool, per process
// (capabilities.hooks). The tool's own settings are never edited.
type HookConfig struct {
	// Flag takes the settings file path (settings-file) or one key=value
	// override (config-override).
	Flag string `toml:"flag"`
	// Format is the layout of the settings file cairn writes (settings-file).
	Format string `toml:"format"`
	// Events maps a tool hook event to a cairn event kind (settings-file).
	Events map[string]string `toml:"events"`
	// Key is the config key set to the event command and Kind the event kind
	// it posts (config-override).
	Key  string `toml:"key"`
	Kind string `toml:"kind"`
}

// HooksReport reports whether the adapter's hooks post event kind k.
func (a Adapter) HooksReport(k string) bool {
	switch a.Capabilities.Hooks {
	case "settings-file":
		for _, v := range a.Hooks.Events {
			if v == k {
				return true
			}
		}
	case "config-override":
		return a.Hooks.Kind == k
	}
	return false
}

// EventKinds are the kinds `cairn event --kind` accepts, the single list
// adapters and the state fold use: the states working … stopped; ready
// (the tool is up and awaits input: starting → idle only, nothing in any
// other state, so a mid-turn restart signal cannot end a turn); activity
// (only the time moves).
var EventKinds = []string{"working", "waiting", "idle", "done", "failed", "stopped", "ready", "activity"}

// FieldError is one validation problem, naming the TOML key.
type FieldError struct {
	Field string
	Msg   string
}

func (e FieldError) Error() string { return e.Field + ": " + e.Msg }

// LoadError reports one adapter file that could not be loaded.
type LoadError struct {
	File string
	Err  error
}

func (e LoadError) Error() string { return e.File + ": " + e.Err.Error() }
func (e LoadError) Unwrap() error { return e.Err }

// Vars are the values substituted into command templates.
type Vars struct {
	ID, Name, NewID, NativeID, Cwd string
	ProfileArgs                    []string
}
