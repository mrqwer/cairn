package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/xdg"
)

//go:embed defaults.toml
var defaultsTOML []byte

const (
	systemFile  = "/etc/cairn/cairn.toml"
	envPrefix   = "CAIRN_"
	hostEnvPref = "CAIRN_HOST_"
)

// defaults and knownKeys are derived once from the embedded defaults file,
// which therefore doubles as the schema of valid keys and value types.
var (
	defaults  map[string]any
	knownKeys map[string]struct{}
)

func init() {
	var m map[string]any
	if err := toml.Unmarshal(defaultsTOML, &m); err != nil {
		panic("config: embedded defaults.toml: " + err.Error())
	}
	defaults = flatten("", m, nil)
	knownKeys = make(map[string]struct{}, len(defaults))
	for k := range defaults {
		knownKeys[k] = struct{}{}
	}
}

func flatten(prefix string, m map[string]any, out map[string]any) map[string]any {
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flatten(key, sub, out)
			continue
		}
		out[key] = v
	}
	return out
}

func nest(flat map[string]any) map[string]any {
	root := map[string]any{}
	for key, v := range flat {
		parts := strings.Split(key, ".")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
		cur[parts[len(parts)-1]] = v
	}
	return root
}

// EnvName returns the environment variable that overrides key.
func EnvName(key string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// CheckEnv rejects CAIRN_HOST_* variables that map to no known key. Load
// takes a getenv func and cannot enumerate, so callers pass os.Environ().
func CheckEnv(environ []string) error {
	valid := map[string]struct{}{}
	for k := range knownKeys {
		valid[EnvName(k)] = struct{}{}
	}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, hostEnvPref) {
			continue
		}
		if _, ok := valid[name]; !ok {
			return fmt.Errorf("unknown config variable %s", name)
		}
	}
	return nil
}

type loader struct {
	flat map[string]any
	prov Provenance
}

func (l *loader) set(key string, v any, o Origin) {
	l.flat[key] = v
	l.prov[key] = o
}

// applyFile merges one TOML layer file; a missing file is skipped.
func (l *loader) applyFile(root fsx.Root, layer, file string) error {
	data, err := root.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: read %s: %w", file, err)
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("config: parse %s: %w", file, err)
	}
	leaves := flatten("", m, nil)
	keys := make([]string, 0, len(leaves))
	for k := range leaves {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := knownKeys[k]; !ok {
			return fmt.Errorf("config: %s: unknown key %q", file, k)
		}
		if layer == "profile" && k == "profile" {
			return fmt.Errorf("config: %s: key %q is not allowed in a profile file", file, k)
		}
		l.set(k, leaves[k], Origin{Layer: layer, File: file})
	}
	return nil
}

func (l *loader) applyEnv(getenv func(string) string) error {
	for k := range knownKeys {
		name := EnvName(k)
		raw := getenv(name)
		if raw == "" {
			continue
		}
		var v any = raw
		if _, isBool := defaults[k].(bool); isBool {
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("config: %s: %q is not a boolean", name, raw)
			}
			v = b
		}
		l.set(k, v, Origin{Layer: "env", File: name})
	}
	return nil
}

// Load merges defaults, system, user, profile and env layers (in that
// order) and returns the result with per-key provenance. Missing layer
// files are skipped; unknown keys and malformed values are errors.
func Load(root fsx.Root, dirs xdg.Dirs, getenv func(string) string) (*Config, Provenance, error) {
	l := &loader{flat: map[string]any{}, prov: Provenance{}}
	for k, v := range defaults {
		l.set(k, v, Origin{Layer: "defaults", File: "embedded"})
	}
	if err := l.applyFile(root, "system", systemFile); err != nil {
		return nil, nil, err
	}
	if err := l.applyFile(root, "user", filepath.Join(dirs.Config, "cairn.toml")); err != nil {
		return nil, nil, err
	}

	name, _ := l.flat["profile"].(string)
	if e := getenv(EnvName("profile")); e != "" {
		name = e
	}
	if name != "" {
		if name != filepath.Base(name) || name == "." || name == ".." {
			return nil, nil, fmt.Errorf("config: invalid profile name %q", name)
		}
		pf := filepath.Join(dirs.Config, "profiles", name+".toml")
		if err := l.applyFile(root, "profile", pf); err != nil {
			return nil, nil, err
		}
	}
	if err := l.applyEnv(getenv); err != nil {
		return nil, nil, err
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(nest(l.flat)); err != nil {
		return nil, nil, fmt.Errorf("config: encode merged: %w", err)
	}
	var cfg Config
	dec := toml.NewDecoder(&buf)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, nil, l.decodeError(err)
	}
	return &cfg, l.prov, nil
}

func (l *loader) decodeError(err error) error {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		key := strings.Join(de.Key(), ".")
		if o, ok := l.prov[key]; ok {
			return fmt.Errorf("config: %s (set by %s layer, %s): %s", key, o.Layer, o.File, de.Error())
		}
	}
	return fmt.Errorf("config: %w", err)
}

// Defaults returns the configuration from the embedded defaults layer alone,
// with no system, user, profile or environment layers. Root-side code that
// runs without XDG state uses it when no host snapshot exists.
func Defaults() (*Config, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(nest(defaults)); err != nil {
		return nil, fmt.Errorf("config: encode defaults: %w", err)
	}
	var cfg Config
	if err := toml.NewDecoder(&buf).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: decode defaults: %w", err)
	}
	return &cfg, nil
}
