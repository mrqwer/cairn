package config

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// FieldError describes one invalid configuration value.
type FieldError struct{ Key, Msg string }

func (e FieldError) Error() string { return e.Key + ": " + e.Msg }

const (
	minHibernateAfter = time.Minute
	maxHibernateAfter = 24 * time.Hour
)

func oneOf(key, val string, allowed ...string) *FieldError {
	for _, a := range allowed {
		if val == a {
			return nil
		}
	}
	return &FieldError{key, fmt.Sprintf("%q is not one of %v", val, allowed)}
}

// Validate checks value ranges and enumerations.
func (c *Config) Validate() []FieldError {
	var errs []FieldError
	add := func(e *FieldError) {
		if e != nil {
			errs = append(errs, *e)
		}
	}
	s := c.Host.Sleep
	lid := []string{"suspend-then-hibernate", "suspend", "hibernate", "ignore"}
	add(oneOf("host.sleep.lid", s.Lid, lid...))
	add(oneOf("host.sleep.lid_ac", s.LidAC, lid...))
	add(oneOf("host.sleep.critical_battery", s.CriticalBattery, "hibernate", "hybrid-sleep", "poweroff"))
	add(oneOf("host.sleep.idle_battery", s.IdleBattery, "hibernate", "suspend", "nothing"))
	if d := s.HibernateAfter.Duration; d < minHibernateAfter || d > maxHibernateAfter {
		errs = append(errs, FieldError{"host.sleep.hibernate_after",
			fmt.Sprintf("%s is outside 1min..24h", s.HibernateAfter.SystemdSpan())})
	}
	for _, p := range []struct{ key, val string }{
		{"host.sleep.hibernation_swap.path", s.HibernationSwap.Path},
		{"host.gpu.nvidia.temporary_file_path", c.Host.GPU.Nvidia.TemporaryFilePath},
	} {
		if p.val == "" {
			continue
		}
		if msg := checkPath(p.val); msg != "" {
			errs = append(errs, FieldError{p.key, fmt.Sprintf("%q %s", p.val, msg)})
		}
	}
	return errs
}

// checkPath requires an absolute, already-clean path without ".." parts or
// control characters; these values reach root-side file operations.
func checkPath(p string) string {
	switch {
	case !path.IsAbs(p):
		return "is not an absolute path"
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "contains control characters"
	case slices.Contains(strings.Split(p, "/"), ".."):
		return "contains a .. component"
	case path.Clean(p) != p:
		return "is not a clean path"
	}
	return ""
}
