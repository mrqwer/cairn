package plan

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/mrqwer/cairn/internal/fsx"
)

const systemUnitDir = "/etc/systemd/system"

var (
	// rootDirs may contain any file cairn writes as root.
	rootDirs = []string{
		"/etc/systemd/logind.conf.d",
		"/etc/systemd/sleep.conf.d",
		"/var/lib/cairn",
	}
	// rootFiles are exact paths cairn may write as root.
	rootFiles = []string{
		"/usr/lib/systemd/system-sleep/50-cairn",
		"/etc/UPower/UPower.conf.d/50-cairn.conf",
		"/etc/modprobe.d/nvidia-utils.conf",
	}
	// repairDropInDirs hold foreign drop-ins repair may remove.
	repairDropInDirs = []string{
		systemUnitDir + "/systemd-hibernate.service.d",
		systemUnitDir + "/systemd-suspend-then-hibernate.service.d",
	}
	bootEntriesDir = "/boot/loader/entries"

	// Units outside the cairn-* namespace that root-side policies may toggle.
	policyUnits = []string{
		"nvidia-suspend.service",
		"nvidia-hibernate.service",
		"nvidia-resume.service",
		"nvidia-suspend-then-hibernate.service",
		"paccache.timer",
	}
	// extra paths may never point into these trees.
	systemTrees = []string{"/etc", "/usr", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/proc", "/sys", "/dev", "/run", "/root", "/efi"}

	unitName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@_.\-]*\.(service|timer|path|socket|target|mount)$`)
	gsSchema  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-]*$`)
	gsKeyName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]*$`)
)

// cleanAbs returns the cleaned form of an absolute path without ".." parts.
func cleanAbs(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return "", false
	}
	return path.Clean(p), true
}

func under(p, dir string) bool { return strings.HasPrefix(p, dir+"/") }

func inExtra(p string, extra []string) bool {
	for _, e := range extra {
		if c, ok := cleanAbs(e); ok && c == p {
			return true
		}
	}
	return false
}

// normalRootPath is the allowlist shared by every mode.
func normalRootPath(p string) bool {
	for _, d := range rootDirs {
		if under(p, d) {
			return true
		}
	}
	if slices.Contains(rootFiles, p) {
		return true
	}
	dir, base := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if dir == systemUnitDir {
		return strings.HasPrefix(base, "cairn-") && unitName.MatchString(base)
	}
	return path.Dir(dir) == systemUnitDir && strings.HasSuffix(dir, ".service.d") && base == "50-cairn.conf"
}

// repairRemovable: a top-level unit file or a foreign sleep-service drop-in.
func repairRemovable(p string) bool {
	dir, base := path.Dir(p), path.Base(p)
	if dir == systemUnitDir {
		return strings.HasSuffix(base, ".service") && unitName.MatchString(base)
	}
	return slices.Contains(repairDropInDirs, dir) && !strings.HasPrefix(base, ".")
}

func bootEntry(p string) bool {
	base := path.Base(p)
	return path.Dir(p) == bootEntriesDir && strings.HasSuffix(base, ".conf") && !strings.HasPrefix(base, ".")
}

// AllowedRootPath reports whether cairn may touch p as root. extra holds
// config-derived paths (the NVIDIA temporary directory), matched exactly.
// Repair mode additionally allows top-level unit files and foreign sleep
// drop-ins (for removal) and systemd-boot entries (for rewriting); see
// CheckRootAction for the per-kind restrictions. Relative paths and paths
// with ".." are rejected; unknown modes allow nothing.
func AllowedRootPath(p string, extra []string, mode string) bool {
	c, ok := cleanAbs(p)
	if !ok {
		return false
	}
	switch mode {
	case ModeApply:
		return normalRootPath(c) || inExtra(c, extra)
	case ModeRepair:
		return normalRootPath(c) || inExtra(c, extra) || repairRemovable(c) || bootEntry(c)
	}
	return false
}

func rootUnit(u string) bool {
	return unitName.MatchString(u) && (strings.HasPrefix(u, "cairn-") || slices.Contains(policyUnits, u))
}

func userUnit(u string) bool { return unitName.MatchString(u) && strings.HasPrefix(u, "cairn-") }

func enableVerb(v string) bool { return v == "enable" || v == "disable" }

func rootCommand(cmd []string) bool {
	switch {
	case slices.Equal(cmd, []string{"systemctl", "daemon-reload"}):
		return true
	case len(cmd) == 3 && cmd[0] == "systemctl" && enableVerb(cmd[1]):
		return rootUnit(cmd[2])
	}
	return false
}

func userCommand(cmd []string) bool {
	switch {
	case slices.Equal(cmd, []string{"systemctl", "--user", "daemon-reload"}):
		return true
	case len(cmd) == 4 && cmd[0] == "systemctl" && cmd[1] == "--user" && enableVerb(cmd[2]):
		return userUnit(cmd[3])
	case len(cmd) == 4 && cmd[0] == "gsettings" && cmd[1] == "get",
		len(cmd) == 5 && cmd[0] == "gsettings" && cmd[1] == "set":
		return gsSchema.MatchString(cmd[2]) && gsKeyName.MatchString(cmd[3])
	}
	return false
}

// CheckRootAction validates one action for execution as root in mode.
func CheckRootAction(a Action, extra []string, mode string) error {
	switch a.Kind {
	case WriteFile, RemoveFile, MakeDir:
		if !AllowedRootPath(a.Path, extra, mode) {
			return fmt.Errorf("%s %q: path not allowed for root in %s mode", a.Kind, a.Path, mode)
		}
		c, _ := cleanAbs(a.Path)
		if normalRootPath(c) {
			return nil
		}
		var only Kind
		switch {
		case inExtra(c, extra):
			only = MakeDir
		case repairRemovable(c):
			only = RemoveFile
		case bootEntry(c):
			only = WriteFile
		}
		if a.Kind != only {
			return fmt.Errorf("%s %q: only %s is allowed on this path", a.Kind, a.Path, only)
		}
	case EnableUnit, DisableUnit:
		if a.UserUnit || !rootUnit(a.Unit) {
			return fmt.Errorf("%s %q: unit not allowed for root", a.Kind, a.Unit)
		}
	case RunCommand:
		if !rootCommand(a.Cmd) {
			return fmt.Errorf("RunCommand %q: command not allowed for root", a.Cmd)
		}
	default:
		return fmt.Errorf("%s: not a root action", a.Kind)
	}
	return nil
}

// CheckUserAction validates one action for execution as the invoking user.
func CheckUserAction(a Action) error {
	switch a.Kind {
	case WriteFile, RemoveFile, MakeDir:
		if _, ok := cleanAbs(a.Path); !ok {
			return fmt.Errorf("%s %q: path must be absolute with no .. components", a.Kind, a.Path)
		}
	case EnableUnit, DisableUnit:
		if !a.UserUnit || !userUnit(a.Unit) {
			return fmt.Errorf("%s %q: only cairn-* user units are allowed", a.Kind, a.Unit)
		}
	case SetGSetting:
		if !gsSchema.MatchString(a.Schema) || !gsKeyName.MatchString(a.Key) {
			return fmt.Errorf("SetGSetting %q %q: invalid schema or key", a.Schema, a.Key)
		}
	case RunCommand:
		if !userCommand(a.Cmd) {
			return fmt.Errorf("RunCommand %q: command not allowed", a.Cmd)
		}
	default:
		return fmt.Errorf("%s: unknown action kind", a.Kind)
	}
	return nil
}

// checkExtra rejects config-derived root paths that could reach system trees.
func checkExtra(extra []string) error {
	for _, e := range extra {
		c, ok := cleanAbs(e)
		if !ok || c == "/" {
			return fmt.Errorf("extra root path %q: must be an absolute path below /", e)
		}
		for _, t := range systemTrees {
			if c == t || under(c, t) {
				return fmt.Errorf("extra root path %q: inside system tree %s", e, t)
			}
		}
	}
	return nil
}

// CheckSelfOwnership refuses a binary that a non-root user could replace:
// self and every parent directory must be root-owned and not group/world
// writable, because apply-root runs that binary through sudo.
func CheckSelfOwnership(root fsx.Root, self string) error {
	c, ok := cleanAbs(self)
	if !ok {
		return fmt.Errorf("cairn binary path %q is not absolute", self)
	}
	for p := c; ; p = path.Dir(p) {
		why := ""
		st, err := root.Stat(p)
		switch {
		case err != nil:
			why = "not inspectable (" + err.Error() + ")"
		case st.Sys() == nil:
			why = "owner unknown"
		default:
			sys, ok := st.Sys().(*syscall.Stat_t)
			switch {
			case !ok:
				why = "owner unknown"
			case sys.Uid != 0:
				why = fmt.Sprintf("owned by uid %d, not root", sys.Uid)
			case st.Mode().Perm()&0o022 != 0:
				why = fmt.Sprintf("mode %v is group/world writable", st.Mode().Perm())
			}
		}
		if why != "" {
			return errors.New("refusing to run " + c + " as root: " + p + " is " + why +
				"; install a root-owned copy with `just install` " +
				"(sudo install -m 0755 -o root -g root bin/cairn /usr/local/bin/cairn) and run that")
		}
		if p == "/" {
			return nil
		}
	}
}
