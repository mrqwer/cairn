// Package repair builds the one-time root repairs of cairn host repair:
// removing hibernate hooks that conflict with cairn's and pointing
// systemd-boot entries at the hibernation swap file.
package repair

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/execx"
	"github.com/mrqwer/cairn/internal/fsx"
	"github.com/mrqwer/cairn/internal/host/sleep"
	"github.com/mrqwer/cairn/internal/inventory"
	"github.com/mrqwer/cairn/internal/plan"
)

// Policy is recorded on every repair action.
const Policy = "repair"

const (
	unitDir      = "/etc/systemd/system"
	ourDropIn    = "50-cairn.conf"
	fallbackUnit = unitDir + "/sleep-fallback.service"
	bootDir      = "/boot"
	entriesDir   = bootDir + "/loader/entries"
	loaderConf   = bootDir + "/loader/loader.conf"
)

var dropInDirs = []string{
	unitDir + "/systemd-hibernate.service.d",
	unitDir + "/systemd-suspend-then-hibernate.service.d",
}

// Seams for tests: the block device of a file and the command runner (blkid).
var (
	deviceOf                = sleep.DeviceOf
	runner   execx.Runner = execx.OSRunner{}
)

// Plan returns the repair actions for the system under root plus report-only
// notes. Conflicting OnFailure= hooks and sleep-fallback.service are removed
// (apply backs them up first); on systemd-boot, entries for the running
// kernel get resume= and resume_offset= for the hibernation swap file.
func Plan(root fsx.Root, sys detect.System, cfg config.Sleep, offset sleep.OffsetFunc) plan.Plan {
	p := plan.Plan{Mode: plan.ModeRepair}
	p.Actions = conflicts(root)
	if len(p.Actions) > 0 {
		p.Actions = append(p.Actions, plan.Action{
			Kind: plan.RunCommand, Cmd: []string{"systemctl", "daemon-reload"}, NeedsRoot: true, Policy: Policy,
		})
	}
	if sys.BootLoader == "systemd-boot" {
		acts, notes := bootEntries(root, sys, cfg, offset)
		p.Actions = append(acts, p.Actions...)
		p.Notes = notes
	}
	p.Notes = append(p.Notes, Report(root, sys)...)
	return p
}

// conflicts finds foreign hibernate drop-ins that set OnFailure= and the
// generic sleep-fallback.service.
func conflicts(root fsx.Root) []plan.Action {
	var out []plan.Action
	for _, dir := range dropInDirs {
		for _, name := range regularFiles(root, dir, "") {
			if name == ourDropIn {
				continue
			}
			p := dir + "/" + name
			if data, err := root.ReadFile(p); err == nil && setsOnFailure(string(data)) {
				out = append(out, plan.Action{
					Kind: plan.RemoveFile, Path: p, NeedsRoot: true, Policy: Policy,
					Reason: "OnFailure= hook conflicts with cairn's fallback",
				})
			}
		}
	}
	if st, err := os.Lstat(root.Path(fallbackUnit)); err == nil && st.Mode().IsRegular() {
		out = append(out, plan.Action{
			Kind: plan.RemoveFile, Path: fallbackUnit, NeedsRoot: true, Policy: Policy,
			Reason: "replaced by cairn-sleep-fallback.service",
		})
	}
	return out
}

// regularFiles lists the regular, non-hidden files in dir with suffix, sorted.
func regularFiles(root fsx.Root, dir, suffix string) []string {
	ents, err := os.ReadDir(root.Path(dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.Type().IsRegular() && !strings.HasPrefix(n, ".") && strings.HasSuffix(n, suffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func setsOnFailure(content string) bool {
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "OnFailure=") {
			return true
		}
	}
	return false
}

// entryLine is one "key value" line of a systemd-boot entry.
func entryLine(l string) (key, value string) {
	f := strings.Fields(l)
	if len(f) == 0 {
		return "", ""
	}
	return f[0], strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), f[0]))
}

// bootEntries rewrites the options of the entries for the running kernel.
func bootEntries(root fsx.Root, sys detect.System, cfg config.Sleep, offset sleep.OffsetFunc) ([]plan.Action, []string) {
	swap, ok := sleep.SelectSwap(cfg, sys)
	if !ok {
		return nil, []string{"no active hibernation swap file; boot entries not updated"}
	}
	off, err := offset(swap.Path)
	if err != nil {
		return nil, []string{fmt.Sprintf("resume offset of %s: %v; boot entries not updated", swap.Path, err)}
	}
	uuid, err := swapUUID(root, swap.Path)
	if err != nil {
		return nil, []string{err.Error() + "; boot entries not updated"}
	}
	match, err := kernelMatcher(root, sys.KernelRunning)
	if err != nil {
		return nil, []string{err.Error() + "; boot entries not updated"}
	}
	want := map[string]string{"resume": "UUID=" + uuid, "resume_offset": strconv.FormatUint(off, 10)}
	var out []plan.Action
	for _, name := range regularFiles(root, entriesDir, ".conf") {
		p := entriesDir + "/" + name
		st, err := root.Stat(p)
		if err != nil {
			continue
		}
		data, err := root.ReadFile(p)
		if err != nil {
			continue
		}
		content, ok := rewriteEntry(string(data), match, want)
		if !ok || content == string(data) {
			continue
		}
		out = append(out, plan.Action{
			Kind: plan.WriteFile, Path: p, Content: []byte(content), Mode: st.Mode().Perm() &^ 0o022,
			NeedsRoot: true, Class: inventory.Repaired, Policy: Policy,
			Reason: "resume=" + want["resume"] + " resume_offset=" + want["resume_offset"],
		})
	}
	return out, nil
}

// swapUUID returns the filesystem UUID of the block device holding file.
func swapUUID(root fsx.Root, file string) (string, error) {
	majmin, err := deviceOf(file)
	if err != nil {
		return "", err
	}
	ue, err := root.ReadFile("/sys/dev/block/" + majmin + "/uevent")
	if err != nil {
		return "", fmt.Errorf("no block device node for %s (device %s)", file, majmin)
	}
	var name string
	for _, l := range strings.Split(string(ue), "\n") {
		if v, ok := strings.CutPrefix(l, "DEVNAME="); ok {
			name = strings.TrimSpace(v)
		}
	}
	if name == "" || strings.Contains(name, "..") {
		return "", fmt.Errorf("no device name for %s (device %s)", file, majmin)
	}
	dev := "/dev/" + name
	b, err := runner.Run(context.Background(), "blkid", "-s", "UUID", "-o", "value", dev)
	uuid := strings.TrimSpace(string(b))
	if err != nil || uuid == "" || strings.ContainsAny(uuid, " \t\n") {
		return "", fmt.Errorf("blkid found no UUID for %s", dev)
	}
	return uuid, nil
}

// kernelMatcher matches an entry's linux path against the running kernel:
// vmlinuz-<pkgbase> when the modules directory names its package (Arch),
// else any image path containing the kernel release.
func kernelMatcher(root fsx.Root, release string) (func(string) bool, error) {
	if release == "" {
		return nil, fmt.Errorf("running kernel unknown")
	}
	if b, err := root.ReadFile("/usr/lib/modules/" + release + "/pkgbase"); err == nil {
		if base := strings.TrimSpace(string(b)); base != "" {
			img := "vmlinuz-" + base
			return func(p string) bool { return path.Base(p) == img }, nil
		}
	}
	return func(p string) bool { return strings.Contains(path.Base(p), release) }, nil
}

// rewriteEntry sets want's keys in the options of an entry whose linux line
// matches; ok is false for other entries. Existing tokens are replaced in
// place, missing ones are appended to the first options line (or a new one).
func rewriteEntry(content string, match func(string) bool, want map[string]string) (string, bool) {
	lines := strings.SplitAfter(content, "\n")
	linuxAt, matched := -1, false
	for i, l := range lines {
		if k, v := entryLine(l); k == "linux" {
			linuxAt, matched = i, match(v)
			break
		}
	}
	if !matched {
		return content, false
	}
	seen := map[string]bool{}
	firstOpts := -1
	for i, l := range lines {
		k, v := entryLine(l)
		if k != "options" {
			continue
		}
		if firstOpts < 0 {
			firstOpts = i
		}
		toks := strings.Fields(v)
		for j, tok := range toks {
			name, _, _ := strings.Cut(tok, "=")
			if val, ok := want[name]; ok {
				toks[j], seen[name] = name+"="+val, true
			}
		}
		lines[i] = setOptions(l, toks)
	}
	var add []string
	for _, k := range []string{"resume", "resume_offset"} {
		if !seen[k] {
			add = append(add, k+"="+want[k])
		}
	}
	if len(add) > 0 {
		if firstOpts >= 0 {
			_, v := entryLine(lines[firstOpts])
			lines[firstOpts] = setOptions(lines[firstOpts], append(strings.Fields(v), add...))
		} else {
			nl := "options " + strings.Join(add, " ") + "\n"
			if !strings.HasSuffix(lines[linuxAt], "\n") {
				lines[linuxAt] += "\n"
			}
			lines = append(lines[:linuxAt+1], append([]string{nl}, lines[linuxAt+1:]...)...)
		}
	}
	return strings.Join(lines, ""), true
}

// setOptions rebuilds an options line, keeping its indentation and newline.
func setOptions(line string, toks []string) string {
	body := strings.TrimRight(line, "\r\n")
	end := line[len(body):]
	indent := body[:len(body)-len(strings.TrimLeft(body, " \t"))]
	return indent + "options " + strings.Join(toks, " ") + end
}

// Report lists systemd-boot problems repair only reports: a loader.conf
// without a default entry and entries whose initrd image is missing.
func Report(root fsx.Root, sys detect.System) []string {
	if sys.BootLoader != "systemd-boot" {
		return nil
	}
	var out []string
	if data, err := root.ReadFile(loaderConf); err == nil {
		hasDefault := false
		for _, l := range strings.Split(string(data), "\n") {
			if k, _ := entryLine(l); k == "default" {
				hasDefault = true
			}
		}
		if !hasDefault {
			out = append(out, loaderConf+" has no default entry; the boot menu order decides what boots")
		}
	}
	for _, name := range regularFiles(root, entriesDir, ".conf") {
		data, err := root.ReadFile(entriesDir + "/" + name)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			if k, v := entryLine(l); k == "initrd" && v != "" && !root.Exists(bootDir+"/"+strings.TrimPrefix(v, "/")) {
				out = append(out, fmt.Sprintf("%s/%s: initrd %s is missing; this entry cannot boot", entriesDir, name, v))
			}
		}
	}
	return out
}
