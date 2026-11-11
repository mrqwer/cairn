// Package config loads cairn's layered configuration with per-key provenance.
package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the fully merged configuration.
type Config struct {
	Profile string `toml:"profile"`
	Host    Host   `toml:"host"`
}

// Host holds settings for the host module.
type Host struct {
	Sleep Sleep `toml:"sleep"`
	GPU   GPU   `toml:"gpu"`
}

// Sleep configures suspend and hibernate behaviour.
type Sleep struct {
	Lid             string   `toml:"lid"`
	LidAC           string   `toml:"lid_ac"`
	CriticalBattery string   `toml:"critical_battery"`
	IdleBattery     string   `toml:"idle_battery"`
	HibernateAfter  Duration `toml:"hibernate_after"`
	HibernationSwap Swap     `toml:"hibernation_swap"`
}

// Swap locates the hibernation swap file.
type Swap struct {
	Path string `toml:"path"`
}

// GPU groups GPU vendor settings.
type GPU struct {
	Nvidia Nvidia `toml:"nvidia"`
}

// Nvidia configures NVIDIA suspend handling.
type Nvidia struct {
	Manage            bool   `toml:"manage"`
	TemporaryFilePath string `toml:"temporary_file_path"`
}

// Duration is a time span written in config as a string such as "15min",
// "2h" or "30s" (systemd spelling accepted).
type Duration struct{ time.Duration }

var (
	spanPart = regexp.MustCompile(`^\s*(\d+)\s*([a-zA-Z]+)\s*`)
	units    = map[string]time.Duration{
		"us": time.Microsecond, "usec": time.Microsecond,
		"ms": time.Millisecond, "msec": time.Millisecond,
		"s": time.Second, "sec": time.Second, "second": time.Second, "seconds": time.Second,
		"m": time.Minute, "min": time.Minute, "minute": time.Minute, "minutes": time.Minute,
		"h": time.Hour, "hr": time.Hour, "hour": time.Hour, "hours": time.Hour,
		"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	}
)

// ParseDuration parses a systemd-style time span such as "1h 30min".
func ParseDuration(s string) (Duration, error) {
	rest := s
	var total time.Duration
	if strings.TrimSpace(rest) == "" {
		return Duration{}, fmt.Errorf("invalid duration %q: empty", s)
	}
	for strings.TrimSpace(rest) != "" {
		m := spanPart.FindStringSubmatch(rest)
		if m == nil {
			return Duration{}, fmt.Errorf("invalid duration %q: want number+unit such as 15min", s)
		}
		unit, ok := units[strings.ToLower(m[2])]
		if !ok {
			return Duration{}, fmt.Errorf("invalid duration %q: unknown unit %q", s, m[2])
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n > int64(1<<62/unit) {
			return Duration{}, fmt.Errorf("invalid duration %q: value too large", s)
		}
		total += time.Duration(n) * unit
		rest = rest[len(m[0]):]
	}
	return Duration{total}, nil
}

// SystemdSpan renders d as a systemd time span, e.g. "15min", "1h 30min".
func (d Duration) SystemdSpan() string {
	rem := d.Duration
	if rem <= 0 {
		return "0"
	}
	var parts []string
	for _, u := range []struct {
		name string
		d    time.Duration
	}{{"h", time.Hour}, {"min", time.Minute}, {"s", time.Second}, {"ms", time.Millisecond}, {"us", time.Microsecond}} {
		if n := rem / u.d; n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", n, u.name))
			rem -= n * u.d
		}
	}
	return strings.Join(parts, " ")
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.SystemdSpan()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
