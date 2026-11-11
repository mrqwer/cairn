package plan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/mrqwer/cairn/internal/config"
	"github.com/mrqwer/cairn/internal/detect"
	"github.com/mrqwer/cairn/internal/fsx"
)

// RootBuilder generates the root-level actions for mode from the host config
// and the detected system. Both sides call it: the user side to show the
// plan, apply-root to regenerate what it executes, so the unprivileged side
// never hands actions or file content to root. Later tasks register the
// sleep, GPU and repair policies here.
var RootBuilder = func(mode string, host config.Host, sys detect.System, self string) ([]Action, error) {
	return nil, nil
}

// ErrPlanChanged is returned by apply-root when the regenerated plan is not
// the one the user approved.
var ErrPlanChanged = errors.New("plan changed since you approved it; run cairn host apply again")

// MaxRootRequest bounds the request apply-root reads from stdin.
const MaxRootRequest = 64 << 10

const requestVersion = 1

// RootRequest is the only input apply-root takes from the unprivileged side
// (on stdin). It carries no actions or file content.
type RootRequest struct {
	Version int         `json:"version"`
	Mode    string      `json:"mode"`
	Host    config.Host `json:"host"`
	Extra   []string    `json:"extra,omitempty"`
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
	return RootRequest{Version: requestVersion, Mode: mode, Host: host, Extra: ExtraPaths(host)}
}

// Validate checks every field; any failure rejects the whole request.
func (r RootRequest) Validate() error {
	if r.Version != requestVersion {
		return fmt.Errorf("request version %d, want %d", r.Version, requestVersion)
	}
	if r.Mode != ModeApply && r.Mode != ModeRepair {
		return fmt.Errorf("request mode %q is not %q or %q", r.Mode, ModeApply, ModeRepair)
	}
	var errs []error
	cfg := config.Config{Host: r.Host}
	for _, e := range cfg.Validate() {
		errs = append(errs, e)
	}
	if want := ExtraPaths(r.Host); !slices.Equal(r.Extra, want) {
		errs = append(errs, fmt.Errorf("extra paths %q do not match the host config (%q)", r.Extra, want))
	}
	errs = append(errs, checkExtra(r.Extra))
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

// Hash fingerprints the pending root actions of a resolved plan (kind, path,
// content, mode, unit, command, in order). The user side passes the hash of
// what it showed; apply-root refuses to run anything else.
func Hash(actions []Action) string {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, a := range actions {
		if !a.NeedsRoot || a.State == Unchanged {
			continue
		}
		field([]byte(a.Kind))
		field([]byte(a.Path))
		field(a.Content)
		field(fmt.Appendf(nil, "%d", uint32(a.Mode)))
		field([]byte(a.Unit))
		field(fmt.Appendf(nil, "%d", len(a.Cmd)))
		for _, c := range a.Cmd {
			field([]byte(c))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RootOptions are the argv-level inputs of apply-root.
type RootOptions struct {
	Expect string // Hash of the plan the user approved
	Force  bool
	Sys    detect.System // detected by apply-root itself
	Self   string
}

// ApplyRoot is the root side of apply: validate the request, regenerate the
// root actions, validate all of them, resolve against the system inventory,
// require the plan hash to equal the approved one, then write the host config
// snapshot and execute. Nothing is written before every check has passed.
func ApplyRoot(ctx context.Context, r RootRequest, env Env, opts RootOptions) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("rejected request: %w", err)
	}
	if env.SysInv == nil {
		return errors.New("plan: system inventory not loaded")
	}
	acts, err := BuildRoot(r.Mode, r.Host, opts.Sys, opts.Self)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range acts {
		errs = append(errs, CheckRootAction(a, r.Extra, r.Mode))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("rejected plan: %w", err)
	}
	p := Resolve(acts, env.Root, env.SysInv)
	p.Mode = r.Mode
	if Hash(p.Actions) != opts.Expect {
		return ErrPlanChanged
	}
	if m := p.ModifiedPaths(); len(m) > 0 && !opts.Force {
		return fmt.Errorf("%w: %s", ErrModifiedByUser, strings.Join(m, ", "))
	}
	snap, err := HostSnapshot(r.Host)
	if err != nil {
		return err
	}
	if err := env.Root.MkdirAll(SysStateDir, 0o755); err != nil {
		return err
	}
	if err := env.Root.WriteAtomic(HostConfigPath, snap, 0o644); err != nil {
		return err
	}
	return Apply(ctx, p, env, Options{Force: opts.Force, RootOnly: true, Extra: r.Extra})
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
