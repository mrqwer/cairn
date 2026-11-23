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
	Env          Env                 `toml:"env"`
	History      History             `toml:"history"`
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
}

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
