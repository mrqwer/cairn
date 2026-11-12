package plan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
)

// RootBuilder generates the root-level actions for mode from the host config
// and the detected system. apply-root calls it with its own detection and
// shows and confirms the result, so the unprivileged side never hands actions
// or file content to root; the user side calls it only for a best-effort
// preview. Later tasks register the
// sleep, GPU and repair policies here.
var RootBuilder = func(mode string, host config.Host, sys detect.System, self string) ([]Action, error) {
	return nil, nil
}

// RootNotes returns report-only findings for mode (for example repair
// problems cairn does not fix itself). apply-root prints them before its plan.
var RootNotes = func(mode string, host config.Host, sys detect.System) []string {
	return nil
}

// MaxRootRequest bounds the request apply-root reads from stdin.
const MaxRootRequest = 64 << 10

const requestVersion = 1

// RootRequest is the only input apply-root takes from the unprivileged side
// (on stdin). It carries no actions, file content or paths beyond the host
// config; config-derived root paths are recomputed with ExtraPaths.
type RootRequest struct {
	Version int         `json:"version"`
	Mode    string      `json:"mode"`
	Host    config.Host `json:"host"`
}

// ExtraPaths lists config-derived paths the root side may create: the NVIDIA
// temporary directory, unless it is the vendor default /var/tmp.
func ExtraPaths(host config.Host) []string {
	n := host.GPU.Nvidia
	if !n.Manage || n.TemporaryFilePath == "" || path.Clean(n.TemporaryFilePath) == "/var/tmp" {
		return nil
	}
	return []string{n.TemporaryFilePath}
}

// NewRootRequest builds the request for mode and host.
func NewRootRequest(mode string, host config.Host) RootRequest {
	return RootRequest{Version: requestVersion, Mode: mode, Host: host}
}

// Validate checks every field; any failure rejects the whole request.
func (r RootRequest) Validate() error {
	if r.Version != requestVersion {
		return fmt.Errorf("request version %d, want %d", r.Version, requestVersion)
	}
	if r.Mode != ModeApply && r.Mode != ModeRepair && r.Mode != ModeUninstall {
		return fmt.Errorf("request mode %q is not %q, %q or %q", r.Mode, ModeApply, ModeRepair, ModeUninstall)
	}
	var errs []error
	cfg := config.Config{Host: r.Host}
	for _, e := range cfg.Validate() {
		errs = append(errs, e)
	}
	errs = append(errs, checkExtra(ExtraPaths(r.Host)))
	return errors.Join(errs...)
}

// ReadRootRequest decodes one request from r, reading at most
// MaxRootRequest bytes. Errors never echo the input.
func ReadRootRequest(r io.Reader) (RootRequest, error) {
	var req RootRequest
	data, err := io.ReadAll(io.LimitReader(r, MaxRootRequest+1))
	if err != nil {
		return req, fmt.Errorf("read request: %w", err)
	}
	if len(data) > MaxRootRequest {
		return req, fmt.Errorf("request larger than %d bytes", MaxRootRequest)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return RootRequest{}, errors.New("request is not a valid cairn root request")
	}
	if _, err := dec.Token(); err != io.EOF {
		return RootRequest{}, errors.New("request is not a valid cairn root request")
	}
	return req, nil
}

// BuildRoot calls RootBuilder and checks every action is a root action.
func BuildRoot(mode string, host config.Host, sys detect.System, self string) ([]Action, error) {
	acts, err := RootBuilder(mode, host, sys, self)
	if err != nil {
		return nil, fmt.Errorf("build root actions: %w", err)
	}
	for _, a := range acts {
		if !a.NeedsRoot {
			return nil, fmt.Errorf("build root actions: %s %s is not a root action", a.Kind, a.target())
		}
	}
	return acts, nil
}

// ErrRootDeclined is returned when the user answers no at the root prompt.
var ErrRootDeclined = errors.New("root changes declined")

// RootOptions are the argv-level inputs of apply-root plus its terminal.
type RootOptions struct {
	Force bool
	Yes   bool          // skip the confirmation (user ran apply --yes)
	Sys   detect.System // detected by apply-root itself
	Self  string
	Out   io.Writer // where the root plan and prompt are printed
	TTY   io.Reader // where the answer is read (never the request's stdin)
}

// ApplyRoot is the root side of apply: validate the request, regenerate the
// root actions from root's own view of the system, validate all of them,
// resolve against the system inventory and check every target directory
// chain; then show this plan and ask on the TTY (unless Yes); only then write
// the host config snapshot and execute. Nothing is written before the user
// has confirmed the plan root will actually run.
func ApplyRoot(ctx context.Context, r RootRequest, env Env, opts RootOptions) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("rejected request: %w", err)
	}
	if env.SysInv == nil {
		return errors.New("plan: system inventory not loaded")
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	acts, err := BuildRoot(r.Mode, r.Host, opts.Sys, opts.Self)
	if err != nil {
		return err
	}
	extra := ExtraPaths(r.Host)
	var errs []error
	for _, a := range acts {
		errs = append(errs, CheckRootAction(a, extra, r.Mode))
		if r.Mode == ModeUninstall && a.Kind == RemoveFile {
			if _, ok := env.SysInv.Get(a.Path); !ok {
				errs = append(errs, fmt.Errorf("RemoveFile %q: not recorded in the system inventory", a.Path))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("rejected plan: %w", err)
	}
	p := Resolve(acts, env.Root, env.SysInv)
	p.Mode = r.Mode
	if m := p.ModifiedPaths(); len(m) > 0 && !opts.Force {
		return fmt.Errorf("%w: %s", ErrModifiedByUser, strings.Join(m, ", "))
	}
	if err := env.precheckDirs(p); err != nil {
		return err
	}
	for _, n := range RootNotes(r.Mode, r.Host, opts.Sys) {
		_, _ = fmt.Fprintf(out, "note: %s\n", n)
	}
	// Uninstall leaves the snapshot alone; otherwise a changed snapshot is a
	// root change like any other and is shown and confirmed.
	stale := r.Mode != ModeUninstall && SnapshotStale(env.Root, r.Host)
	renderRoot(out, p, stale)
	if len(p.Pending()) == 0 && !stale {
		return nil
	}
	if !opts.Yes {
		if opts.TTY == nil {
			return errors.New("no terminal to confirm root changes; re-run cairn host apply with --yes")
		}
		_, _ = fmt.Fprint(out, "Apply these root changes? [y/N] ")
		line, _ := bufio.NewReader(opts.TTY).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			return ErrRootDeclined
		}
	}
	if stale {
		snap, err := HostSnapshot(r.Host)
		if err != nil {
			return err
		}
		if err := env.secureDir(SysStateDir, true); err != nil {
			return err
		}
		if err := env.Root.WriteAtomic(HostConfigPath, snap, 0o644); err != nil {
			return err
		}
	}
	if err := Apply(ctx, p, env, Options{Force: opts.Force, RootOnly: true, Extra: extra}); err != nil {
		return err
	}
	bootChanged, logindChanged := false, false
	for _, a := range p.Pending() {
		if a.Kind != WriteFile {
			continue
		}
		c := path.Clean(a.Path)
		bootChanged = bootChanged || bootEntry(c)
		logindChanged = logindChanged || under(c, "/etc/systemd/logind.conf.d") || under(c, "/etc/UPower/UPower.conf.d")
	}
	if bootChanged {
		_, _ = fmt.Fprintln(out, "boot entries changed: requires reboot")
	}
	if logindChanged {
		_, _ = fmt.Fprintln(out, "Lid and critical-battery settings take effect after a reboot "+
			"(or: sudo systemctl restart systemd-logind — this ends the graphical session).")
	}
	return nil
}

// renderRoot prints root's plan; a stale host config snapshot is shown as one
// more pending change.
func renderRoot(w io.Writer, p Plan, stale bool) {
	if !stale {
		Render(w, p)
		return
	}
	if len(p.Pending()) == 0 {
		_, _ = fmt.Fprintln(w, "needs root:")
	} else {
		Render(w, p)
	}
	_, _ = fmt.Fprintf(w, "  update  config snapshot  %s\n", HostConfigPath)
}

// precheckDirs verifies, without creating anything, the existing part of
// every directory chain root will write into, so an unsafe chain is refused
// before the prompt and before any write.
func (env Env) precheckDirs(p Plan) error {
	dirs := []string{SysStateDir, BackupDir}
	for _, a := range p.Pending() {
		switch a.Kind {
		case WriteFile, RemoveFile, MakeDir:
			dirs = append(dirs, path.Dir(path.Clean(a.Path)))
		}
	}
	for _, d := range dirs {
		if err := env.secureDir(d, false); err != nil {
			return err
		}
	}
	return nil
}

// HostSnapshot is the content of HostConfigPath for host.
func HostSnapshot(host config.Host) ([]byte, error) {
	data, err := json.MarshalIndent(host, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// SnapshotStale reports whether HostConfigPath differs from host.
func SnapshotStale(root fsx.Root, host config.Host) bool {
	want, err := HostSnapshot(host)
	if err != nil {
		return true
	}
	got, err := root.ReadFile(HostConfigPath)
	return err != nil || !bytes.Equal(got, want)
}
