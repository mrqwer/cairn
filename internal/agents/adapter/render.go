package adapter

import (
	"fmt"
	"sort"
	"strings"
)

const profileArgsPlaceholder = "{profile_args...}"

var knownPlaceholders = map[string]bool{
	"id": true, "name": true, "new_id": true, "native_id": true, "cwd": true, "value": true,
}

// scan walks one template element in a single pass, calling emit with each
// literal run and each placeholder name. Substituted text is never rescanned.
func scan(elem string, lit func(string), ph func(string) error) error {
	for i := 0; i < len(elem); {
		j := strings.IndexByte(elem[i:], '{')
		if j < 0 {
			lit(elem[i:])
			return nil
		}
		lit(elem[i : i+j])
		i += j
		end := strings.IndexByte(elem[i:], '}')
		if end < 0 {
			lit(elem[i:])
			return nil
		}
		if err := ph(elem[i+1 : i+end]); err != nil {
			return err
		}
		i += end + 1
	}
	return nil
}

type ctxKind int

const (
	ctxCommand ctxKind = iota // commands.*: every placeholder except {value}
	ctxFlag                   // profile_flags.*: only {value}
)

// checkPlaceholder is the single rule for one placeholder name in one element.
// ponytail: there is no escape for a literal "{...}" in an element; such text
// fails as an unknown placeholder. Known limit, add a "{{" escape if needed.
func checkPlaceholder(name, elem string, k ctxKind) error {
	switch k {
	case ctxFlag:
		if name == "value" {
			return nil
		}
		if name == "profile_args..." || knownPlaceholders[name] {
			return fmt.Errorf("{%s} is not valid in profile_flags (only {value}), got %q", name, elem)
		}
	default:
		if name == "profile_args..." {
			return fmt.Errorf("{profile_args...} must be a whole element, got %q", elem)
		}
		if name == "value" {
			return fmt.Errorf("{value} is only valid in profile_flags, got %q", elem)
		}
		if knownPlaceholders[name] {
			return nil
		}
	}
	return fmt.Errorf("unknown placeholder {%s} in %q", name, elem)
}

// checkElement validates the placeholders of one template element.
func checkElement(elem string, k ctxKind) error {
	if k == ctxCommand && elem == profileArgsPlaceholder {
		return nil
	}
	return scan(elem, func(string) {}, func(name string) error { return checkPlaceholder(name, elem, k) })
}

func expand(tpl []string, vals map[string]string, spliced []string, k ctxKind) ([]string, error) {
	out := make([]string, 0, len(tpl))
	for _, elem := range tpl {
		if k == ctxCommand && elem == profileArgsPlaceholder {
			out = append(out, spliced...)
			continue
		}
		var b strings.Builder
		err := scan(elem, func(s string) { b.WriteString(s) }, func(name string) error {
			if err := checkPlaceholder(name, elem, k); err != nil {
				return err
			}
			b.WriteString(vals[name])
			return nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, b.String())
	}
	return out, nil
}

// Render turns the named command template into argv. Each placeholder inside
// an element is replaced as one string; {profile_args...} splices zero or more
// elements.
func (a Adapter) Render(cmd string, v Vars) ([]string, error) {
	tpl, ok := a.Commands[cmd]
	if !ok {
		return nil, fmt.Errorf("adapter %s: no %q command", a.Name, cmd)
	}
	switch cmd {
	case "fork":
		if !a.Capabilities.Fork {
			return nil, fmt.Errorf("adapter %s: fork is not supported (capabilities.fork is off)", a.Name)
		}
	case "list":
		if !a.Capabilities.ListJSON {
			return nil, fmt.Errorf("adapter %s: list is not supported (capabilities.list_json is off)", a.Name)
		}
	}
	return expand(tpl, map[string]string{
		"id": v.ID, "name": v.Name, "new_id": v.NewID, "native_id": v.NativeID, "cwd": v.Cwd,
	}, v.ProfileArgs, ctxCommand)
}

// ProfileArgs maps generic profile settings to this tool's flags in sorted key
// order. Keys the tool has no mapping for are returned, not treated as errors.
func (a Adapter) ProfileArgs(settings map[string]string) (args, unsupported []string) {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tpl, ok := a.ProfileFlags[k]
		if !ok {
			unsupported = append(unsupported, k)
			continue
		}
		out, err := expand(tpl, map[string]string{"value": settings[k]}, nil, ctxFlag)
		if err != nil {
			unsupported = append(unsupported, k)
			continue
		}
		args = append(args, out...)
	}
	return args, unsupported
}

// Validate checks the adapter against the schema rules.
func (a Adapter) Validate() []FieldError {
	var errs []FieldError
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	c := a.Capabilities

	if a.Name == "" {
		add("name", "required")
	}
	start, hasStart := a.Commands["start"]
	if !hasStart || len(start) == 0 {
		add("commands.start", "required")
	}
	for _, e := range start {
		if strings.Contains(e, "{id}") && !c.PreassignID {
			add("commands.start", "{id} requires capabilities.preassign_id")
			break
		}
	}
	if (c.ResumeByID || c.ResumeByName) && len(a.Commands["resume"]) == 0 {
		add("commands.resume", "required when resume_by_id or resume_by_name is set")
	}
	for _, r := range []struct {
		on   bool
		cmd  string
		capn string
	}{{c.Fork, "fork", "fork"}, {c.ListJSON, "list", "list_json"}, {c.Delete, "delete", "delete"}} {
		if r.on && len(a.Commands[r.cmd]) == 0 {
			add("commands."+r.cmd, "required when capabilities."+r.capn+" is set")
		}
	}
	if a.History.Locator != "" && a.History.Parser == "" {
		add("history.parser", "required when history.locator is set")
	}
	if a.History.Parser != "" && a.History.Locator == "" {
		add("history.locator", "required when history.parser is set")
	}
	switch c.Hooks {
	case "", "none", "settings-file", "config-override":
	default:
		add("capabilities.hooks", fmt.Sprintf("must be none, settings-file or config-override, got %q", c.Hooks))
	}
	switch c.Discovery {
	case "none", "dir-diff", "list", "state-file":
	case "":
		if !c.PreassignID && !c.NameAtStart {
			add("capabilities.discovery", "required when neither preassign_id nor name_at_start is set")
		}
	default:
		add("capabilities.discovery", fmt.Sprintf("must be none, dir-diff, list or state-file, got %q", c.Discovery))
	}

	names := make([]string, 0, len(a.Commands))
	for n := range a.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, e := range a.Commands[n] {
			if err := checkElement(e, ctxCommand); err != nil {
				add("commands."+n, err.Error())
			}
		}
	}
	flags := make([]string, 0, len(a.ProfileFlags))
	for n := range a.ProfileFlags {
		flags = append(flags, n)
	}
	sort.Strings(flags)
	for _, n := range flags {
		for _, e := range a.ProfileFlags[n] {
			if err := checkElement(e, ctxFlag); err != nil {
				add("profile_flags."+n, err.Error())
			}
		}
	}
	return errs
}
