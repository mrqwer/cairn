package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

// Load returns the embedded built-in adapters overridden by name with
// <dirs.Config>/tools/*.toml, sorted by name. A broken or invalid file becomes
// a LoadError and does not stop the rest.
func Load(root fsx.Root, dirs xdg.Dirs) ([]Adapter, []LoadError, error) {
	byName := map[string]Adapter{}
	var lerrs []LoadError

	ents, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		data, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, nil, err
		}
		a, err := parse(data)
		if err != nil {
			lerrs = append(lerrs, LoadError{"builtin/" + e.Name(), err})
			continue
		}
		byName[a.Name] = a
	}

	dir := filepath.Join(dirs.Config, "tools")
	files, err := os.ReadDir(root.Path(dir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	for _, e := range files {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		file := filepath.Join(dir, e.Name())
		data, err := root.ReadFile(file)
		if err != nil {
			lerrs = append(lerrs, LoadError{file, err})
			continue
		}
		a, err := parse(data)
		if err != nil {
			lerrs = append(lerrs, LoadError{file, err})
			continue
		}
		byName[a.Name] = a
	}

	out := make([]Adapter, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, lerrs, nil
}

func parse(data []byte) (Adapter, error) {
	var a Adapter
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			var keys []string
			for _, de := range sm.Errors {
				keys = append(keys, strings.Join(de.Key(), "."))
			}
			return Adapter{}, fmt.Errorf("unknown key %s", strings.Join(keys, ", "))
		}
		return Adapter{}, err
	}
	if verrs := a.Validate(); len(verrs) > 0 {
		msgs := make([]string, len(verrs))
		for i, e := range verrs {
			msgs[i] = e.Error()
		}
		return Adapter{}, errors.New(strings.Join(msgs, "; "))
	}
	return a, nil
}
