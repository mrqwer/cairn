// Package detect inspects the host (distro, power, GPU, boot) without side effects.
package detect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
)

// Tri is a three-valued answer. The zero value is Unknown, so a field left
// unset never claims a capability.
type Tri int

const (
	Unknown Tri = iota
	Yes
	No
)

// Swap is one entry of /proc/swaps.
type Swap struct {
	Path, Type       string
	SizeKiB, UsedKiB int64
	Priority         int
}

// Resume is the configured hibernation resume target.
type Resume struct {
	Device string
	Offset uint64
}

// System is the detected host description.
type System struct {
	Distro               string
	SystemdVersion       int
	PkgManager           string
	GPU                  string
	BootLoader           string
	Desktop              string
	KernelRunning        string
	KernelModulesPresent bool
	Swaps                []Swap
	Resume               Resume
	MemSleepDeep         bool
	InitramfsResume      Tri
	NvidiaParams         map[string]string
	UPowerDropIns        bool
	// HibernateSupported is No when kernel lockdown (integrity or
	// confidentiality) is active or /sys/power/state does not offer "disk".
	HibernateSupported Tri
	// Lockdown is the active kernel lockdown mode ("none", "integrity",
	// "confidentiality"), empty when unknown.
	Lockdown string
}

var (
	firstNumber = regexp.MustCompile(`\d+`)
	hooksLine   = regexp.MustCompile(`(?m)^\s*HOOKS\s*=\s*\(([^)]*)\)`)
)

// Detect gathers facts about the host. Missing optional sources yield zero
// values; a present but malformed file is an error naming that file.
func Detect(ctx context.Context, root fsx.Root, r execx.Runner, getenv func(string) string) (System, error) {
	var s System

	if out, err := r.Run(ctx, "systemctl", "--version"); err == nil {
		if m := firstNumber.Find(out); m != nil {
			s.SystemdVersion, _ = strconv.Atoi(string(m))
		}
	}
	if out, err := r.Run(ctx, "uname", "-r"); err == nil {
		s.KernelRunning = strings.TrimSpace(string(out))
	}
	if s.KernelRunning != "" {
		s.KernelModulesPresent = root.Exists("usr/lib/modules/" + s.KernelRunning)
	}

	if b, ok, err := read(root, "etc/os-release"); err != nil {
		return s, err
	} else if ok {
		s.Distro = osReleaseID(string(b))
	}

	for _, p := range []string{"pacman", "apt-get", "dnf"} {
		if root.Exists("usr/bin/" + p) {
			s.PkgManager = p
			break
		}
	}
	if root.Exists("sys/module/nvidia") {
		s.GPU = "nvidia"
	}
	switch {
	case root.Exists("boot/loader/loader.conf"):
		s.BootLoader = "systemd-boot"
	case root.Exists("boot/grub/grub.cfg"):
		s.BootLoader = "grub"
	default:
		s.BootLoader = "unknown"
	}
	s.Desktop = desktop(getenv("XDG_CURRENT_DESKTOP"))

	if b, ok, err := read(root, "proc/swaps"); err != nil {
		return s, err
	} else if ok {
		if s.Swaps, err = parseSwaps(string(b)); err != nil {
			return s, fmt.Errorf("detect: proc/swaps: %w", err)
		}
	}

	if b, ok, err := read(root, "sys/power/resume"); err != nil {
		return s, err
	} else if ok {
		s.Resume.Device = strings.TrimSpace(string(b))
	}
	if b, ok, err := read(root, "sys/power/resume_offset"); err != nil {
		return s, err
	} else if ok {
		if t := strings.TrimSpace(string(b)); t != "" {
			v, err := strconv.ParseUint(t, 10, 64)
			if err != nil {
				return s, fmt.Errorf("detect: sys/power/resume_offset: %w", err)
			}
			s.Resume.Offset = v
		}
	}
	if b, ok, err := read(root, "sys/power/mem_sleep"); err != nil {
		return s, err
	} else if ok {
		s.MemSleepDeep = strings.Contains(string(b), "[deep]")
	}

	if b, ok, err := read(root, "etc/mkinitcpio.conf"); err != nil {
		return s, err
	} else if ok {
		if m := hooksLine.FindStringSubmatch(string(b)); m != nil {
			for _, h := range strings.Fields(m[1]) {
				if h == "resume" || h == "systemd" {
					s.InitramfsResume = Yes
					break
				}
			}
		}
	}
	if s.InitramfsResume != Yes && root.Exists("etc/dracut.conf") {
		s.InitramfsResume = Yes
	}

	if b, ok, err := read(root, "proc/driver/nvidia/params"); err != nil {
		return s, err
	} else if ok {
		s.NvidiaParams = parseKV(string(b))
	}
	s.UPowerDropIns = root.Exists("etc/UPower/UPower.conf.d")
	s.HibernateSupported, s.Lockdown = hibernateSupport(root)
	return s, nil
}

var lockdownMode = regexp.MustCompile(`\[([a-z]+)\]`)

// hibernateSupport reads the lockdown mode and the sleep states the kernel
// offers. Unreadable files are not errors: securityfs may be unavailable.
func hibernateSupport(root fsx.Root) (Tri, string) {
	mode := ""
	if b, err := root.ReadFile("sys/kernel/security/lockdown"); err == nil {
		if m := lockdownMode.FindStringSubmatch(string(b)); m != nil {
			mode = m[1]
		}
	}
	if mode == "integrity" || mode == "confidentiality" {
		return No, mode
	}
	b, err := root.ReadFile("sys/power/state")
	if err != nil {
		return Unknown, mode
	}
	for _, f := range strings.Fields(string(b)) {
		if f == "disk" {
			return Yes, mode
		}
	}
	return No, mode
}

// read returns the file content; ok is false when it does not exist.
func read(root fsx.Root, p string) ([]byte, bool, error) {
	b, err := root.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("detect: %s: %w", p, err)
	}
	return b, true, nil
}

func osReleaseID(content string) string {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ID="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func desktop(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if strings.Contains(v, "gnome") {
		return "gnome"
	}
	return v
}

func parseSwaps(content string) ([]Swap, error) {
	var out []Swap
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if i == 0 || len(f) == 0 {
			continue
		}
		if len(f) < 5 {
			return nil, fmt.Errorf("line %d: want 5 columns, got %d", i+1, len(f))
		}
		size, e1 := strconv.ParseInt(f[2], 10, 64)
		used, e2 := strconv.ParseInt(f[3], 10, 64)
		prio, e3 := strconv.Atoi(f[4])
		if err := errors.Join(e1, e2, e3); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, Swap{Path: f[0], Type: f[1], SizeKiB: size, UsedKiB: used, Priority: prio})
	}
	return out, nil
}

func parseKV(content string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}
