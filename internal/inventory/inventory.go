// Package inventory records the files and settings cairn manages on a host.
package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/mrqwer/cairn/internal/fsx"
)

// Class describes what kind of managed object an entry is.
type Class string

const (
	DropIn      Class = "dropin"
	Mask        Class = "mask"
	ManagedDir  Class = "dir"
	UserSetting Class = "user-setting"
	UnitEnable  Class = "unit-enable"
	// Repaired marks a foreign file rewritten once by cairn host repair (a
	// boot entry); uninstall leaves it alone.
	Repaired Class = "repaired"
)

const formatVersion = 1

// Entry is one managed object. Backup is the path of a saved pre-existing
// file, Previous the prior value of a setting or unit state, and Upstream*
// describe the vendor file a mask hides.
type Entry struct {
	Path           string `json:"path"`
	Policy         string `json:"policy,omitempty"`
	Class          Class  `json:"class,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
	Backup         string `json:"backup,omitempty"`
	Previous       string `json:"previous,omitempty"`
	Upstream       string `json:"upstream,omitempty"`
	UpstreamSHA256 string `json:"upstream_sha256,omitempty"`
}

// Inventory is the set of managed entries, unique by path.
type Inventory struct {
	Entries []Entry
}

type file struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Load reads the inventory at path. A missing file yields an empty inventory;
// malformed content is an error.
func Load(root fsx.Root, path string) (*Inventory, error) {
	data, err := root.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Inventory{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read inventory %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse inventory %s: %w", path, err)
	}
	if f.Version != formatVersion {
		return nil, fmt.Errorf("parse inventory %s: unsupported version %d", path, f.Version)
	}
	inv := &Inventory{Entries: f.Entries}
	inv.sort()
	return inv, nil
}

// Save writes the inventory atomically with mode 0644, sorted by path.
func (i *Inventory) Save(root fsx.Root, path string) error {
	i.sort()
	entries := i.Entries
	if entries == nil {
		entries = []Entry{}
	}
	data, err := json.MarshalIndent(file{Version: formatVersion, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inventory: %w", err)
	}
	return root.WriteAtomic(path, append(data, '\n'), 0o644)
}

// Get returns the entry for path.
func (i *Inventory) Get(path string) (Entry, bool) {
	for _, e := range i.Entries {
		if e.Path == path {
			return e, true
		}
	}
	return Entry{}, false
}

// Upsert adds e or replaces the entry with the same path.
func (i *Inventory) Upsert(e Entry) {
	for n := range i.Entries {
		if i.Entries[n].Path == e.Path {
			i.Entries[n] = e
			i.sort()
			return
		}
	}
	i.Entries = append(i.Entries, e)
	i.sort()
}

// Delete removes the entry for path, if present.
func (i *Inventory) Delete(path string) {
	out := i.Entries[:0]
	for _, e := range i.Entries {
		if e.Path != path {
			out = append(out, e)
		}
	}
	i.Entries = out
}

func (i *Inventory) sort() {
	sort.Slice(i.Entries, func(a, b int) bool { return i.Entries[a].Path < i.Entries[b].Path })
}
