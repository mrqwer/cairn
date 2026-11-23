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
	for i, f := range a.History.SecretFields {
		if !fieldName(f) {
			add(fmt.Sprintf("history.secret_fields[%d]", i), fmt.Sprintf("must be a field name of [A-Za-z0-9_.-], got %q", f))
		}
	}
	switch c.Hooks {
	case "", "none", "settings-file", "config-override":
	default:
		add("capabilities.hooks", fmt.Sprintf("must be none, settings-file or config-override, got %q", c.Hooks))
	}
	errs = append(errs, a.validateHooks()...)
	switch c.Discovery {
	case "none", "dir-diff", "list", "state-file":
	case "":
		if !c.PreassignID && !c.NameAtStart {
			add("capabilities.discovery", "required when neither preassign_id nor name_at_start is set")
		}
	default:
		add("capabilities.discovery", fmt.Sprintf("must be none, dir-diff, list or state-file, got %q", c.Discovery))
	}

	errs = append(errs, a.validateProbe()...)

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
	return append(errs, a.validateProfile()...)
}

// validateProfile checks profile_defaults and risky_flags: a default needs
// a flag to map it, is not a flag itself, and a tool with a sandbox flag
// must say what "on" is (sandbox is on by default, spec §9.7).
func (a Adapter) validateProfile() []FieldError {
	var errs []FieldError
	keys := make([]string, 0, len(a.ProfileDefaults))
	for k := range a.ProfileDefaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := a.ProfileDefaults[k]
		switch {
		case len(a.ProfileFlags[k]) == 0:
			errs = append(errs, FieldError{"profile_defaults." + k, "needs profile_flags." + k})
		case v == "" || strings.HasPrefix(v, "-") || strings.ContainsAny(v, "\x00\n\r\t"):
			errs = append(errs, FieldError{"profile_defaults." + k, fmt.Sprintf("%q must be a value, not empty or starting with \"-\"", v)})
		}
	}
	if len(a.ProfileFlags["sandbox"]) > 0 && a.ProfileDefaults["sandbox"] == "" {
		errs = append(errs, FieldError{"profile_defaults.sandbox", "required when profile_flags.sandbox is set (sandbox is on by default)"})
	}
	for i, f := range a.RiskyFlags {
		if !strings.HasPrefix(f, "-") || strings.Count(f, " ") > 1 {
			errs = append(errs, FieldError{fmt.Sprintf("risky_flags[%d]", i), fmt.Sprintf("must be \"--flag\" or \"--flag value\", got %q", f)})
		}
	}
	return errs
}

func validKind(k string) bool {
	for _, x := range EventKinds {
		if k == x {
			return true
		}
	}
	return false
}

func onlyChars(s, extra string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(extra, r)
		if !ok {
			return false
		}
	}
	return true
}

// validateHooks checks the [hooks] section the hooks mode needs.
func (a Adapter) validateHooks() []FieldError {
	var errs []FieldError
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	h, mode := a.Hooks, a.Capabilities.Hooks
	switch mode {
	case "settings-file":
		if h.Flag == "" {
			add("hooks.flag", "required when capabilities.hooks is settings-file")
		}
		if h.Format != HookFormatClaudeSettings {
			add("hooks.format", fmt.Sprintf("unknown settings format %q (known: %s)", h.Format, HookFormatClaudeSettings))
		}
		if len(h.Events) == 0 {
			add("hooks.events", "required when capabilities.hooks is settings-file")
		}
		names := make([]string, 0, len(h.Events))
		for n := range h.Events {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !onlyChars(n, "_") {
				add("hooks.events", fmt.Sprintf("event name %q must be letters, digits and _", n))
				continue
			}
			if !validKind(h.Events[n]) {
				add("hooks.events."+n, fmt.Sprintf("unknown event kind %q", h.Events[n]))
			}
		}
	case "config-override":
		if h.Flag == "" {
			add("hooks.flag", "required when capabilities.hooks is config-override")
		}
		switch {
		case h.Key == "":
			add("hooks.key", "required when capabilities.hooks is config-override")
		case !onlyChars(h.Key, "_-."):
			add("hooks.key", fmt.Sprintf("%q must be letters, digits, _, - and .", h.Key))
		}
		if !validKind(h.Kind) {
			add("hooks.kind", fmt.Sprintf("unknown event kind %q", h.Kind))
		}
	}
	return errs
}

// validateProbe checks probe_flags: each entry is a help command, so a
// doctor probe can never start a conversation.
func (a Adapter) validateProbe() []FieldError {
	var errs []FieldError
	for i, argv := range a.ProbeFlags {
		f := fmt.Sprintf("probe_flags[%d]", i)
		if len(argv) == 0 {
			errs = append(errs, FieldError{f, "must not be empty"})
			continue
		}
		help, bad := false, ""
		for _, e := range argv {
			switch e {
			case "--help", "-h", "help":
				help = true
			}
			if bad == "" && strings.ContainsAny(e, "{}") {
				bad = e
			}
		}
		if bad != "" {
			errs = append(errs, FieldError{f, fmt.Sprintf("placeholders are not allowed, got %q", bad)})
			continue
		}
		if !help {
			errs = append(errs, FieldError{f, fmt.Sprintf(`must contain "--help", "-h" or "help" (a probe never starts the tool), got %q`, argv)})
		}
	}
	return errs
}

// fieldName reports whether s is a non-empty name of [A-Za-z0-9_.-], safe to
// quote into a pattern.
func fieldName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
