package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// LoadError reports one project file that could not be loaded.
type LoadError struct {
	File string
	Err  error
}

func (e LoadError) Error() string { return e.File + ": " + e.Err.Error() }
func (e LoadError) Unwrap() error { return e.Err }

// Load reads every <dirs.Config>/projects/*.toml. A missing directory means
// no projects. A broken file becomes a LoadError and does not stop the rest.
func Load(root fsx.Root, dirs xdg.Dirs) ([]Project, []LoadError, error) {
	dir := filepath.Join(dirs.Config, "projects")
	entries, err := os.ReadDir(root.Path(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var (
		projects []Project
		lerrs    []LoadError
		seen     = map[string]string{}
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		file := filepath.Join(dir, e.Name())
		data, err := root.ReadFile(file)
		if err != nil {
			lerrs = append(lerrs, LoadError{file, err})
			continue
		}
		p, err := parse(file, data, dirs.Home)
		if err != nil {
			lerrs = append(lerrs, LoadError{file, err})
			continue
		}
		if want := strings.TrimSuffix(e.Name(), ".toml"); p.ID != want {
			lerrs = append(lerrs, LoadError{file, fmt.Errorf("id %q must equal the file name %q", p.ID, want)})
			continue
		}
		if prev, dup := seen[p.ID]; dup {
			lerrs = append(lerrs, LoadError{file, fmt.Errorf("duplicate id %q (also in %s)", p.ID, prev)})
			continue
		}
		seen[p.ID] = file
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects, lerrs, nil
}

func parse(file string, data []byte, home string) (Project, error) {
	p, err := Decode(data)
	if err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			var keys []string
			for _, de := range sm.Errors {
				keys = append(keys, strings.Join(de.Key(), "."))
			}
			return Project{}, fmt.Errorf("unknown key %s", strings.Join(keys, ", "))
		}
		return Project{}, err
	}
	p.ApplyDefaults(home)
	if ferrs := p.Validate(); len(ferrs) > 0 {
		msgs := make([]string, len(ferrs))
		for i, fe := range ferrs {
			msgs[i] = fe.Error()
		}
		return Project{}, errors.New(strings.Join(msgs, "; "))
	}
	return p, nil
}

// Decode strictly decodes one project file without applying defaults.
func Decode(data []byte) (Project, error) {
	var p Project
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Project{}, err
	}
	return p, nil
}

// Marshal renders p as TOML with a fixed field order, omitting defaults.
func Marshal(p Project) ([]byte, error) {
	var b strings.Builder
	kv := func(k, v string) { b.WriteString(k + " = " + v + "\n") }
	kv("id", quote(p.ID))
	kv("name", quote(p.Name))
	kv("root", quote(p.Root))
	if p.Session != "" && p.Session != p.ID {
		kv("session", quote(p.Session))
	}
	if p.Status != "" && p.Status != StatusActive {
		kv("status", quote(p.Status))
	}
	if len(p.Tags) > 0 {
		kv("tags", quoteList(p.Tags))
	}
	for _, w := range p.Windows {
		b.WriteString("\n[[windows]]\n")
		kv("key", quote(w.Key))
		kv("name", quote(w.Name))
		for i, pn := range w.Panes {
			b.WriteString("\n[[windows.panes]]\n")
			if pn.Role != "" && pn.Role != RoleShell {
				kv("role", quote(pn.Role))
			}
			if pn.Cwd != "" && pn.Cwd != p.Root {
				kv("cwd", quote(pn.Cwd))
			}
			if len(pn.Run) > 0 {
				kv("run", quoteList(pn.Run))
			}
			if pn.Restart != "" && pn.Restart != DefaultRestart(pn.Role) {
				kv("restart", quote(pn.Restart))
			}
			if pn.Split != "" {
				kv("split", quote(pn.Split))
			}
			if pn.From != 0 && pn.From != i {
				kv("from", strconv.Itoa(pn.From))
			}
			if pn.Size != 0 && pn.Size != DefaultSize {
				kv("size", strconv.Itoa(pn.Size))
			}
			if pn.Title != "" {
				kv("title", quote(pn.Title))
			}
		}
	}
	return []byte(b.String()), nil
}

func quoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// quote renders s as a TOML basic string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
