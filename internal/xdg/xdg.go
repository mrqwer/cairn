// Package xdg resolves cairn's per-user directories from the environment.
package xdg

import (
	"errors"
	"fmt"
	"path/filepath"
)

const app = "cairn"

// Dirs holds the cairn subdirectory under each XDG base directory.
// Home is the plain home directory.
type Dirs struct {
	Home, Config, State, Data, Cache, Runtime string
}

// Resolve computes Dirs using getenv. Relative XDG_* values are ignored
// per the XDG Base Directory spec; XDG_RUNTIME_DIR has no default and must
// be set and absolute.
func Resolve(getenv func(string) string) (Dirs, error) {
	home := getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return Dirs{}, errors.New("xdg: HOME is not set to an absolute path")
	}
	base := func(env, def string) string {
		if v := getenv(env); filepath.IsAbs(v) {
			return filepath.Join(v, app)
		}
		return filepath.Join(home, def, app)
	}
	rt := getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(rt) {
		return Dirs{}, fmt.Errorf("xdg: XDG_RUNTIME_DIR must be set to an absolute path, got %q", rt)
	}
	return Dirs{
		Home:    home,
		Config:  base("XDG_CONFIG_HOME", ".config"),
		State:   base("XDG_STATE_HOME", ".local/state"),
		Data:    base("XDG_DATA_HOME", ".local/share"),
		Cache:   base("XDG_CACHE_HOME", ".cache"),
		Runtime: filepath.Join(rt, app),
	}, nil
}
