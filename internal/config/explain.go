package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Origin records which layer set a key. File is the file path for file
// layers, "embedded" for defaults and the variable name for env.
type Origin struct{ Layer, File string }

// Provenance maps dotted keys to the Origin of their effective value.
type Provenance map[string]Origin

// Value returns the effective value of key as text.
func (c *Config) Value(key string) (string, bool) {
	s, g := c.Host.Sleep, c.Host.GPU.Nvidia
	switch key {
	case "profile":
		return c.Profile, true
	case "host.sleep.lid":
		return s.Lid, true
	case "host.sleep.lid_ac":
		return s.LidAC, true
	case "host.sleep.critical_battery":
		return s.CriticalBattery, true
	case "host.sleep.idle_battery":
		return s.IdleBattery, true
	case "host.sleep.hibernate_after":
		return s.HibernateAfter.SystemdSpan(), true
	case "host.sleep.hibernation_swap.path":
		return s.HibernationSwap.Path, true
	case "host.gpu.nvidia.manage":
		return strconv.FormatBool(g.Manage), true
	case "host.gpu.nvidia.temporary_file_path":
		return g.TemporaryFilePath, true
	}
	return "", false
}

// Explain renders "<value>  <layer>  (<file>)" for key.
func Explain(c *Config, prov Provenance, key string) (string, error) {
	v, ok := c.Value(key)
	if !ok {
		return "", fmt.Errorf("unknown key %q (known: %s)", key, strings.Join(Keys(), ", "))
	}
	o := prov[key]
	return fmt.Sprintf("%s  %s  (%s)", v, o.Layer, o.File), nil
}

// Keys lists every known dotted key, sorted.
func Keys() []string {
	ks := make([]string, 0, len(knownKeys))
	for k := range knownKeys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
