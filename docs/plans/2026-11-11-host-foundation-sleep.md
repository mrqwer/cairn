# host, part 1: foundation + sleep — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the `cairn` binary with the shared foundation (XDG paths, layered config, events, inventory, doctor, plan/apply) and the sleep part of `host`: suspend-then-hibernate policy, pre-flight checks at sleep entry, NVIDIA sleep settings, silent-failure detection, uninstall and repair.

**Architecture:** One Go module, one binary. Every filesystem access goes through `fsx.Root` and every external command through `execx.Runner`, so all logic is tested against temp directories and fake commands; the real system is `Root{Dir:"/"}` + `OSRunner`. Policies turn config + detected system into a `plan.Plan` of actions; `apply` executes user-level actions directly and root-level actions through `sudo cairn host apply-root`, which re-validates every path against an allowlist.

**Tech Stack:** Go (go directive 1.25; built with the installed 1.27), `github.com/spf13/cobra` (CLI), `github.com/pelletier/go-toml/v2` (strict TOML), `golang.org/x/sys/unix` (FIEMAP ioctl), stdlib `testing`. Task runner: `just`.

**Spec:** `docs/specs/host.md` (sections 4, 4.1–4.3, 5, 9, 10, 11, 12). Principles: `docs/principles.md`. Disk hygiene (§6), backups (§7) and WIP snapshots (§8) are separate plans that reuse this foundation.

## Global Constraints

- Module path `github.com/mrqwer/cairn`; binary `cairn`; source under `cmd/cairn` and `internal/`.
- **Public vs personal:** code, defaults, tests and docs in this repo are generic. Test fixtures modelled on the reference machine are anonymised (no UUIDs, serials, usernames, hostnames, home paths). The owner's real values (swap path, NVIDIA temp dir, delays) go only into `~/.config/cairn/cairn.toml`, written by `cairn init` or by hand during Task 14.
- No hardcoded user, host or project paths in code; user paths come from `xdg.Dirs`, system paths are the constants listed in Task 7's allowlist.
- Every generated file begins with exactly: `# Managed by cairn host (<policy>). Do not edit; run "cairn host ..."` (shell scripts: same text on line 2, after the shebang).
- System state directory: `/var/lib/cairn/host/` (events `events.jsonl`, inventory `managed.json`, last check `sleep-check.json`, backups `backup/`). User state: `$XDG_STATE_HOME/cairn/host/`.
- Config file: `$XDG_CONFIG_HOME/cairn/cairn.toml` (system layer `/etc/cairn/cairn.toml`); host settings live under `[host.*]` sections. (Spec §10 example shows `host.toml`; one file with sections is the decision for all modules.)
- Sleep defaults: lid (battery and AC) `suspend-then-hibernate`, `HibernateDelaySec=15min`, `SuspendEstimationSec=15min`, `HibernateOnACPower=no`, critical battery `Hibernate`, GNOME idle on battery `hibernate`.
- Root-run units must execute a root-owned binary: `apply` refuses if the resolved `cairn` path or any parent directory is writable by a non-root user.
- Nothing privileged runs without the plan being printed first; `apply` asks for confirmation unless `--yes`.
- Commit messages describe the change only — no AI attribution lines. Do not push. Ask the owner before the first commit.

## Review Focus

1. **Second `apply` on an unchanged system** must print "nothing to do" and change nothing (idempotence) — test in Task 8.
2. **A managed file edited by hand** (checksum differs from inventory) must not be overwritten silently: plan marks it `modified-by-user` and apply needs `--force` — test in Task 8.
3. **A pre-existing non-cairn file at a target path** (e.g. `/etc/modprobe.d/nvidia-utils.conf` already present) is backed up before overwrite and restored by `uninstall` — test in Task 12.
4. **`events.jsonl` with a truncated last line** (power loss mid-write) must be read without error, skipping only the broken line — test in Task 4.
5. **`cairn` installed in a user-writable location** (e.g. `~/.local/bin`) must make `apply` refuse with the exact install command to fix it — test in Task 8.

---

## File Structure

```
go.mod, go.sum, justfile, .golangci.yml
cmd/cairn/main.go                 entry; builds the cobra root
internal/cli/root.go              root command, version, global flags (--root for tests)
internal/cli/config.go            cairn config validate|explain
internal/cli/doctor.go            cairn doctor [host]
internal/cli/host.go              cairn host plan|apply|apply-root|uninstall|repair|sleep-check|record-sleep|boot-check|notify
internal/fsx/root.go              Root: path joining, atomic writes, managed header, sha256
internal/execx/runner.go          Runner interface, OSRunner, FakeRunner
internal/xdg/xdg.go               XDG directory resolution
internal/config/{types,defaults.toml,load,validate,explain}.go
internal/events/log.go            append-only JSONL event log
internal/inventory/inventory.go   managed-file inventory
internal/doctor/doctor.go         checks, results, rendering, exit code
internal/plan/{plan,render,apply,allowlist}.go
internal/detect/detect.go         system detection
internal/host/sleep/policy.go     sleep policy → actions
internal/host/sleep/check.go      pre-flight checks (sleep-check)
internal/host/sleep/fiemap.go     first physical offset of a file
internal/host/sleep/record.go     record-sleep, boot-check, notify
internal/host/gpu/nvidia.go       NVIDIA masking copy + checks
internal/host/repair/repair.go    conflicting units, systemd-boot resume_offset
internal/host/uninstall.go        uninstall plan from inventory
testdata/                         fixture system trees per task
```

---

### Task 1: Module, CLI skeleton, `fsx.Root`, `execx.Runner`

**Files:**
- Create: `go.mod`, `justfile`, `.golangci.yml`, `cmd/cairn/main.go`, `internal/cli/root.go`, `internal/fsx/root.go`, `internal/execx/runner.go`
- Test: `internal/fsx/root_test.go`, `internal/execx/runner_test.go`

**Interfaces:**
- Produces:
  - `type fsx.Root struct{ Dir string }` with `Path(p string) string`, `ReadFile(p string) ([]byte, error)`, `WriteAtomic(p string, data []byte, perm fs.FileMode) error` (temp file in same dir + fsync + rename + fsync dir), `Remove(p string) error` (missing is not an error), `Exists(p string) bool`, `MkdirAll(p string, perm fs.FileMode) error`, `Stat(p string) (fs.FileInfo, error)`.
  - `func fsx.Header(policy string) string` → the Global Constraints header line + `\n`.
  - `func fsx.SHA256(data []byte) string` (hex).
  - `type execx.Runner interface{ Run(ctx context.Context, name string, args ...string) ([]byte, error) }`; `execx.OSRunner{}`; `execx.FakeRunner{ Responses map[string]execx.Response; Calls [][]string }` where key is the space-joined command line and `Response{Out []byte; Err error}`; unknown commands return an error naming the command.
  - `cairn version` prints `cairn <version>` (version injected with `-ldflags -X`).

- [ ] **Step 1: Write failing tests**
  - `TestWriteAtomicReplacesContent`: write "a" then "b" to `etc/x.conf` under `t.TempDir()`; read back "b"; no `*.tmp*` files left in the directory.
  - `TestRemoveMissingIsNotError`.
  - `TestHeader`: `fsx.Header("sleep") == "# Managed by cairn host (sleep). Do not edit; run \"cairn host ...\"\n"`.
  - `TestFakeRunnerRecordsCalls` and `TestFakeRunnerUnknownCommandErrors`.
- [ ] **Step 2: Run** `go test ./internal/...` — Expected: FAIL (packages missing).
- [ ] **Step 3: Implement** the files above; `justfile` recipes: `build` (`go build -trimpath -ldflags "-X github.com/mrqwer/cairn/internal/cli.Version=$(git describe --always --dirty)" -o bin/cairn ./cmd/cairn`), `test` (`go test ./...`), `lint` (`golangci-lint run`), `install` (`sudo install -m 0755 -o root -g root bin/cairn /usr/local/bin/cairn`).
- [ ] **Step 4: Run** `just test && just build && ./bin/cairn version` — Expected: tests PASS, prints `cairn <hash>`.
- [ ] **Step 5: Commit** `feat: module skeleton, atomic file root and command runner`

---

### Task 2: XDG directories

**Files:**
- Create: `internal/xdg/xdg.go`
- Test: `internal/xdg/xdg_test.go`

**Interfaces:**
- Produces: `type xdg.Dirs struct{ Home, Config, State, Data, Cache, Runtime string }` (each module path is `<dir>/cairn`); `func xdg.Resolve(getenv func(string) string) (xdg.Dirs, error)`.

- [ ] **Step 1: Write failing tests**
  - `TestDefaults`: env `HOME=/h` only → Config `/h/.config/cairn`, State `/h/.local/state/cairn`, Data `/h/.local/share/cairn`, Cache `/h/.cache/cairn`; Runtime from `XDG_RUNTIME_DIR`, error if unset.
  - `TestRelativeXDGIgnored`: `XDG_CONFIG_HOME=rel/path` is ignored per the XDG spec (falls back to default).
  - `TestMissingHomeErrors`.
- [ ] **Step 2: Run** `go test ./internal/xdg/` — Expected: FAIL.
- [ ] **Step 3: Implement** `Resolve`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: XDG directory resolution`

---

### Task 3: Layered config, validation, `config explain`

**Files:**
- Create: `internal/config/types.go`, `internal/config/defaults.toml` (embedded), `internal/config/load.go`, `internal/config/validate.go`, `internal/config/explain.go`, `internal/cli/config.go`
- Test: `internal/config/load_test.go`, `internal/config/validate_test.go`, `testdata/config/`

**Interfaces:**
- Consumes: `fsx.Root`, `xdg.Dirs`.
- Produces:
  - `type config.Config struct{ Profile string; Host config.Host }`; `type config.Host struct{ Sleep config.Sleep; GPU config.GPU }`;
    `type config.Sleep struct{ Lid, LidAC, CriticalBattery, IdleBattery string; HibernateAfter config.Duration; HibernationSwap config.Swap }`;
    `type config.Swap struct{ Path string }`; `type config.GPU struct{ Nvidia config.Nvidia }`; `type config.Nvidia struct{ Manage bool; TemporaryFilePath string }`; `type config.Duration struct{ time.Duration }` (TOML string like `"15min"`, `"60min"`, `"2h"`; systemd spelling `min` accepted).
    TOML keys: `host.sleep.lid`, `host.sleep.lid_ac`, `host.sleep.critical_battery`, `host.sleep.idle_battery`, `host.sleep.hibernate_after`, `host.sleep.hibernation_swap.path`, `host.gpu.nvidia.manage`, `host.gpu.nvidia.temporary_file_path`, top-level `profile`.
  - `func config.Load(root fsx.Root, dirs xdg.Dirs, getenv func(string) string) (*config.Config, config.Provenance, error)`. Layers in order: `defaults` (embedded) → `system` (`/etc/cairn/cairn.toml`) → `user` (`<Config>/cairn.toml`) → `profile` (`<Config>/profiles/<profile>.toml`, profile name from the merged `profile` key or `CAIRN_PROFILE`) → `env` (`CAIRN_HOST_SLEEP_LID` style: `CAIRN_` + upper-snake key path). Missing layer files are skipped.
  - `type config.Provenance map[string]config.Origin` keyed by dotted key; `Origin{ Layer, File string }`.
  - `func (c *config.Config) Validate() []config.FieldError`; `FieldError{ Key, Msg string }`. Allowed values: lid/lid_ac `suspend-then-hibernate|suspend|hibernate|ignore`; critical_battery `hibernate|hybrid-sleep|poweroff`; idle_battery `hibernate|suspend|nothing`; hibernate_after between `1min` and `24h`; paths absolute.
  - CLI: `cairn config validate` (exit 1 and one line per FieldError), `cairn config explain <key>` (prints `value`, `layer`, `file`).
- defaults.toml values: the Sleep defaults in Global Constraints; `host.gpu.nvidia.manage = true`, `temporary_file_path = "/var/tmp"`.

- [ ] **Step 1: Write failing tests**
  - `TestDefaultsOnly`: empty root → `Host.Sleep.HibernateAfter == 15*time.Minute`, `Lid == "suspend-then-hibernate"`, provenance of `host.sleep.lid` is layer `defaults`.
  - `TestLayerOrder`: system sets lid `suspend`, user sets `hibernate`, env `CAIRN_HOST_SLEEP_LID=ignore` → value `ignore`, origin `env`.
  - `TestProfileLayer`: user `profile = "laptop"`, `profiles/laptop.toml` sets `hibernation_swap.path` → applied, origin `profile`.
  - `TestUnknownKeyIsError`: user file with `host.sleep.lidd` → error containing the file path and `host.sleep.lidd`.
  - `TestValidate`: lid `"sleep"` → FieldError on `host.sleep.lid`; hibernate_after `"30s"` → FieldError; relative swap path → FieldError.
- [ ] **Step 2: Run** `go test ./internal/config/` — Expected: FAIL.
- [ ] **Step 3: Implement**: decode each layer into `map[string]any`, deep-merge maps recording origin per leaf key, then strict-decode the merged map into `Config` (`DisallowUnknownFields`), reporting unknown keys with the layer file that introduced them.
- [ ] **Step 4: Run** `go test ./internal/config/` — Expected: PASS. Run `./bin/cairn config explain host.sleep.hibernate_after` → `15min  defaults  (embedded)`.
- [ ] **Step 5: Commit** `feat: layered config with provenance and validation`

---

### Task 4: Event log

**Files:**
- Create: `internal/events/log.go`
- Test: `internal/events/log_test.go`

**Interfaces:**
- Consumes: `fsx.Root`.
- Produces: `type events.Event struct{ TS time.Time "json:\"ts\""; Boot string "json:\"boot\""; Source string "json:\"source\""; Kind string "json:\"kind\""; Data map[string]any "json:\"data,omitempty\"" }`; `type events.Log struct{ Root fsx.Root; Path string }`; `func (l events.Log) Append(e events.Event) error` (open `O_APPEND|O_CREATE`, mode 0644, one JSON object + `\n`, fsync); `func (l events.Log) ReadFrom(offset int64) (evs []events.Event, next int64, err error)` (skips unparsable lines, never errors on them); `func events.CurrentBoot(root fsx.Root) (string, error)` reads `/proc/sys/kernel/random/boot_id`.
- Event kinds used in this plan (constants in this package): `sleep.pre`, `sleep.post`, `sleep.check.failed`, `sleep.fallback`, `boot.lost-during-sleep`, `kernel.reboot-required`.

- [ ] **Step 1: Write failing tests**
  - `TestAppendAndRead`: two appends → `ReadFrom(0)` returns both in order; `next` equals file size.
  - `TestReadFromOffset`: read, append one more, `ReadFrom(next)` returns only the new one.
  - `TestTruncatedLastLine` (Review Focus 4): file content = valid line + `{"ts":"2026-` (no newline) → returns 1 event, no error, and `next` points to the start of the broken line so a later complete append is still read.
- [ ] **Step 2: Run** `go test ./internal/events/` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: append-only event log`

---

### Task 5: Managed-file inventory

**Files:**
- Create: `internal/inventory/inventory.go`
- Test: `internal/inventory/inventory_test.go`

**Interfaces:**
- Consumes: `fsx.Root`.
- Produces: `type inventory.Class string` with `DropIn="dropin"`, `Mask="mask"`, `ManagedDir="dir"`, `UserSetting="user-setting"`, `UnitEnable="unit-enable"`; `type inventory.Entry struct{ Path, Policy string; Class inventory.Class; SHA256 string; Backup string; Previous string; Upstream string; UpstreamSHA256 string }` (`Backup` = path of the saved pre-existing file, `Previous` = previous value of a setting or unit state, `Upstream*` = vendor file for masks); `type inventory.Inventory struct{ Entries []inventory.Entry }`; `func inventory.Load(root fsx.Root, path string) (*inventory.Inventory, error)` (missing → empty); `func (i *inventory.Inventory) Save(root fsx.Root, path string) error` (atomic, 0644); `Get(path string) (inventory.Entry, bool)`; `Upsert(e inventory.Entry)`; `Delete(path string)`.

- [ ] **Step 1: Write failing tests** `TestRoundTrip` (save, load, equal), `TestUpsertReplacesByPath`, `TestLoadMissingIsEmpty`.
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement** (JSON, entries sorted by path for stable diffs).
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: managed-file inventory`

---

### Task 6: Doctor framework

**Files:**
- Create: `internal/doctor/doctor.go`, `internal/cli/doctor.go`
- Test: `internal/doctor/doctor_test.go`

**Interfaces:**
- Produces: `type doctor.Status int` with `OK`, `Warn`, `Crit`, `Unknown` (String: `ok`, `warn`, `CRIT`, `?`); `type doctor.Result struct{ ID, Title string; Status doctor.Status; Evidence, Fix string }`; `type doctor.Check interface{ ID() string; Title() string; Run(ctx context.Context) doctor.Result }`; `func doctor.Run(ctx context.Context, checks []doctor.Check) []doctor.Result` (a panicking check yields `Unknown` with the panic text as evidence); `func doctor.Render(w io.Writer, rs []doctor.Result)` (columns: status, title, evidence; `fix:` line under non-OK rows); `func doctor.ExitCode(rs []doctor.Result) int` (2 if any Crit, else 0). `cairn doctor` and `cairn doctor host` both run the host checks registered by later tasks (`--json` prints results as JSON).

- [ ] **Step 1: Write failing tests** `TestExitCode` (OK+Warn → 0; any Crit → 2), `TestPanicBecomesUnknown`, `TestRenderShowsFixOnlyForNonOK` (golden string).
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: doctor framework`

---

### Task 7: System detection

**Files:**
- Create: `internal/detect/detect.go`
- Test: `internal/detect/detect_test.go`, `testdata/detect/arch-nvidia/` (fixture tree copied from the reference machine's relevant files: `etc/os-release`, `proc/swaps`, `proc/cmdline`, `sys/power/{resume,resume_offset,mem_sleep,image_size}`, `etc/mkinitcpio.conf`, `usr/lib/modprobe.d/nvidia-utils.conf`, `sys/module/nvidia/` marker, `proc/driver/nvidia/params`, `etc/UPower/UPower.conf.d/`, `boot/loader/entries/*.conf`)

**Interfaces:**
- Consumes: `fsx.Root`, `execx.Runner`.
- Produces: `type detect.System struct{ Distro string; SystemdVersion int; PkgManager string; GPU string; BootLoader string; Desktop string; KernelRunning string; KernelModulesPresent bool; Swaps []detect.Swap; Resume detect.Resume; MemSleepDeep bool; InitramfsResume detect.Tri; NvidiaParams map[string]string; UPowerDropIns bool }`;
  `type detect.Swap struct{ Path, Type string; SizeKiB, UsedKiB int64; Priority int }`; `type detect.Resume struct{ Device string; Offset uint64 }` (from `/sys/power/resume` "MAJ:MIN" and `/sys/power/resume_offset`); `type detect.Tri int` (`Yes`, `No`, `Unknown`);
  `func detect.Detect(ctx context.Context, root fsx.Root, r execx.Runner, getenv func(string) string) (detect.System, error)`.
  Sources: `systemctl --version` (first number), `/etc/os-release` `ID`, package manager by binary presence (`pacman`, `apt-get`, `dnf`), GPU `nvidia` if `/sys/module/nvidia` exists, boot loader `systemd-boot` if `/boot/loader/loader.conf` exists else `grub` if `/boot/grub/grub.cfg` else `unknown`, desktop from `XDG_CURRENT_DESKTOP` (lowercased, `gnome` if it contains GNOME), kernel `uname -r`, modules present = `/usr/lib/modules/<kernel>` exists, initramfs resume: mkinitcpio `HOOKS` contains `resume` or `systemd` → Yes, `/etc/dracut.conf` present → Yes, else Unknown.

- [ ] **Step 1: Write failing tests**
  - `TestDetectArchNvidia` on the fixture with FakeRunner (`systemctl --version` → `systemd 262 (262.1-1-arch)`, `uname -r` → `7.2.11-arch1-1`): expect Distro `arch`, SystemdVersion 262, PkgManager `pacman`, GPU `nvidia`, BootLoader `systemd-boot`, 3 swaps, Resume `{259:3 61112320}`, MemSleepDeep true, InitramfsResume Yes, `NvidiaParams["TemporaryFilePath"] == "\"/var/tmp\""`, UPowerDropIns true.
  - `TestKernelModulesMissing`: no `/usr/lib/modules/7.2.11-arch1-1` in fixture → `KernelModulesPresent == false`.
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: system detection`

---

### Task 8: Plan, render, apply (user and root), allowlist

**Files:**
- Create: `internal/plan/plan.go`, `internal/plan/render.go`, `internal/plan/apply.go`, `internal/plan/allowlist.go`, `internal/cli/host.go` (plan/apply/apply-root)
- Test: `internal/plan/plan_test.go`, `internal/plan/apply_test.go`

**Interfaces:**
- Consumes: `fsx.Root`, `execx.Runner`, `inventory`, `events`.
- Produces:
  - `type plan.Kind string`: `WriteFile`, `RemoveFile`, `MakeDir`, `EnableUnit`, `DisableUnit`, `SetGSetting`, `RunCommand`.
  - `type plan.Action struct{ Kind plan.Kind; Path string; Content []byte; Mode fs.FileMode; Unit string; UserUnit bool; Schema, Key, Value string; Cmd []string; NeedsRoot bool; Class inventory.Class; Policy, Reason string; State plan.State }`; `type plan.State string`: `create`, `update`, `unchanged`, `modified-by-user`, `foreign` (exists, not in inventory).
  - `type plan.Plan struct{ Actions []plan.Action }`; `func plan.Resolve(desired []plan.Action, root fsx.Root, inv *inventory.Inventory) plan.Plan` — sets `State` by comparing with disk and inventory (`unchanged` actions are kept for display but skipped by apply); `func (p plan.Plan) Pending() []plan.Action`.
  - `func plan.Render(w io.Writer, p plan.Plan)` — groups "user-level" and "needs root", one line per action: state, kind, path/unit/key, reason; prints `nothing to do` when no pending actions.
  - `func plan.Apply(ctx context.Context, p plan.Plan, env plan.Env, opts plan.Options) error`; `type plan.Env struct{ Root fsx.Root; Runner execx.Runner; UserInv, SysInv *inventory.Inventory; UserInvPath, SysInvPath string; Events events.Log }`; `type plan.Options struct{ Force bool; RootOnly, UserOnly bool }`. Rules: `foreign` files are copied to `/var/lib/cairn/host/backup/<path with / → %>` before overwrite and recorded in `Entry.Backup`; `modified-by-user` without `Force` returns `plan.ErrModifiedByUser` listing paths; `SetGSetting` records the previous value (`gsettings get`) in `Entry.Previous`; `EnableUnit` records previous `systemctl is-enabled` output.
  - `func plan.AllowedRootPath(p string, extra []string) bool` — `extra` holds config-derived paths (the NVIDIA temporary directory); true only under: `/etc/systemd/logind.conf.d/`, `/etc/systemd/sleep.conf.d/`, `/etc/systemd/system/` (files named `cairn-*` or inside `*.service.d/` with basename `50-cairn.conf`), `/usr/lib/systemd/system-sleep/50-cairn`, `/etc/UPower/UPower.conf.d/50-cairn.conf`, `/etc/modprobe.d/nvidia-utils.conf`, `/var/lib/cairn/`, paths in `extra`, and `/boot/loader/entries/` (repair only). Paths are cleaned first; `..` is rejected.
  - `func plan.CheckSelfOwnership(root fsx.Root, self string) error` — error if `self` or any parent dir is not owned by root or is group/world writable; message includes `just install` / `sudo install -m 0755 -o root -g root bin/cairn /usr/local/bin/cairn`.
  - CLI: `cairn host plan` (prints), `cairn host apply [--yes] [--force]` (prints plan, confirms, runs `UserOnly`, writes the root sub-plan as JSON to `<Runtime>/host-plan.json` mode 0600, runs `sudo <self> host apply-root --plan <file>`), hidden `cairn host apply-root --plan <file>` (requires euid 0; rejects any action failing `AllowedRootPath`; runs `RootOnly`; updates `/var/lib/cairn/host/managed.json`).

- [ ] **Step 1: Write failing tests**
  - `TestResolveStates`: desired WriteFile for missing path → `create`; same content present and in inventory with matching sha → `unchanged`; present not in inventory → `foreign`; in inventory but sha differs → `modified-by-user`.
  - `TestApplyIsIdempotent` (Review Focus 1): apply a plan, re-resolve the same desired actions → `Pending()` empty; `Render` prints `nothing to do`.
  - `TestApplyRefusesModifiedByUser` (Review Focus 2): returns `ErrModifiedByUser`; with `Force` overwrites and updates sha.
  - `TestForeignFileBackedUp`: original content saved under `var/lib/cairn/host/backup/` and `Entry.Backup` set.
  - `TestAllowedRootPath`: allows `/etc/systemd/sleep.conf.d/50-cairn.conf` and `/home/.nvidia-suspend` when passed in `extra`; rejects `/etc/passwd`, `/etc/systemd/sleep.conf.d/../../passwd`, `/etc/systemd/system/sshd.service`.
  - `TestCheckSelfOwnership` (Review Focus 5): binary in a temp dir owned by the test user → error mentioning `sudo install`; skip the positive case unless running as root.
- [ ] **Step 2: Run** `go test ./internal/plan/` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: plan/apply with root allowlist and inventory`

---

### Task 9: Sleep policy

**Files:**
- Create: `internal/host/sleep/policy.go`
- Test: `internal/host/sleep/policy_test.go`, `testdata/sleep/golden/*.conf`

**Interfaces:**
- Consumes: `config.Sleep`, `detect.System`, `plan.Action`.
- Produces: `func sleep.Actions(cfg config.Sleep, sys detect.System, self string) []plan.Action` with policy name `sleep`, generating exactly:
  - `/etc/systemd/logind.conf.d/50-cairn.conf`: `[Login]` `HandleLidSwitch=<lid>` `HandleLidSwitchExternalPower=<lid_ac>`.
  - `/etc/systemd/sleep.conf.d/50-cairn.conf`: `[Sleep]` `AllowSuspendThenHibernate=yes` `HibernateDelaySec=<hibernate_after as systemd span, e.g. 15min>` `SuspendEstimationSec=15min` `HibernateOnACPower=no`, plus `MemorySleepMode=deep` only when `sys.MemSleepDeep`.
  - `/etc/systemd/system/systemd-hibernate.service.d/50-cairn.conf` and `/etc/systemd/system/systemd-suspend-then-hibernate.service.d/50-cairn.conf`: `[Unit]` `OnFailure=cairn-sleep-fallback.service` `[Service]` `ExecStartPre=<self> host sleep-check --record`.
  - `/etc/systemd/system/cairn-sleep-fallback.service`: `Type=oneshot`, `ExecStart=<self> host record-sleep fallback`, `ExecStart=/usr/bin/systemctl suspend`.
  - `/usr/lib/systemd/system-sleep/50-cairn` (mode 0755): `#!/bin/sh`, header line, `exec <self> host record-sleep "$1" "$2"`.
  - `/etc/UPower/UPower.conf.d/50-cairn.conf` when `sys.UPowerDropIns`: `[UPower]` `CriticalPowerAction=<Hibernate|HybridSleep|PowerOff>`.
  - `MakeDir /var/lib/cairn/host` (0755, root).
  - When `sys.Desktop == "gnome"`: `SetGSetting org.gnome.settings-daemon.plugins.power sleep-inactive-battery-type '<idle_battery>'` (user-level).
  - User units in `$XDG_CONFIG_HOME/systemd/user/` (user-level): `cairn-host-notify.path` (`PathChanged=/var/lib/cairn/host/events.jsonl`), `cairn-host-notify.service` (`ExecStart=<self> host notify`), `cairn-host-boot-check.service` (`Type=oneshot`, `ExecStart=<self> host boot-check`, `WantedBy=default.target`); `EnableUnit` for the `.path` and boot-check units with `UserUnit=true`.
  - `RunCommand systemctl daemon-reload` (root) and `systemctl --user daemon-reload` (user) after unit writes.

- [ ] **Step 1: Write failing tests** `TestSleepActionsGolden` (defaults + fixture system → each generated file equals its golden file in `testdata/sleep/golden/`), `TestNoUPowerDropInWhenUnsupported`, `TestNoGSettingOutsideGnome`, `TestMemorySleepModeOnlyWhenDeep`.
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement** (text templates; each file starts with `fsx.Header("sleep")`).
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: sleep policy drop-ins and units`

---

### Task 10: Pre-flight checks (`cairn host sleep-check`) and resume offset

**Files:**
- Create: `internal/host/sleep/check.go`, `internal/host/sleep/fiemap.go`
- Test: `internal/host/sleep/check_test.go`, `internal/host/sleep/fiemap_test.go`

**Interfaces:**
- Consumes: `config.Sleep`, `detect.System`, `doctor`, `events`.
- Produces:
  - `type sleep.OffsetFunc func(path string) (uint64, error)`; `func sleep.FirstPhysicalOffset(path string) (uint64, error)` — first extent's physical byte offset via the `FS_IOC_FIEMAP` ioctl, divided by the page size (4096), which is what `resume_offset` expects.
  - `func sleep.Checks(cfg config.Sleep, sys detect.System, offset sleep.OffsetFunc, devOf func(path string) (string, error)) []doctor.Check` with IDs and Crit conditions:
    - `sleep.swap.present`: configured `hibernation_swap.path` (or, if empty, the single file swap with lowest priority) is active in `sys.Swaps`. Crit if not.
    - `sleep.resume.match`: `devOf(path)` (`MAJ:MIN` of the file's filesystem) equals `sys.Resume.Device` and `offset(path)` equals `sys.Resume.Offset`. Crit with evidence `expected 259:3/<n>, kernel has <dev>/<off>`, fix `cairn host repair`.
    - `sleep.swap.free`: free KiB of that swap ≥ image estimate (`/sys/power/image_size` bytes, else MemTotal−MemAvailable). Crit if smaller.
    - `sleep.kernel.match`: `sys.KernelModulesPresent`. Crit "kernel upgraded since boot — reboot before hibernating".
    - `sleep.initramfs.resume`: Yes→OK, No→Crit, Unknown→Unknown.
  - CLI `cairn host sleep-check [--record]`: runs checks, prints doctor table; with `--record` writes `/var/lib/cairn/host/sleep-check.json` and, on any Crit, appends `sleep.check.failed` (data: failed IDs) and exits 1 (makes `ExecStartPre` fail → `OnFailure` fallback). The same checks are registered with `cairn doctor host`; when not root, `sleep.resume.match` reads the result from `sleep-check.json` (age shown in evidence) instead of calling FIEMAP.

- [ ] **Step 1: Write failing tests**
  - `TestChecksAllGreen`: fixture system, offset func returns 61112320, devOf returns `259:3` → all OK.
  - `TestResumeOffsetMismatch`: offset returns 999 → `sleep.resume.match` Crit, evidence contains `61112320` and `999`.
  - `TestKernelMismatchCrit`.
  - `TestSwapTooSmall`: image_size 8 GiB, swap free 4 GiB → Crit.
  - `TestFirstPhysicalOffsetOnRealFile`: create a 1 MiB file in `t.TempDir()`, `fsync`; `FirstPhysicalOffset` returns > 0 or `t.Skip` if the filesystem returns `ENOTSUP`/`EOPNOTSUPP` (e.g. tmpfs).
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement `FirstPhysicalOffset`** with this algorithm (ioctl struct layout is not obvious):

```go
// struct fiemap { u64 fm_start, fm_length; u32 fm_flags, fm_mapped_extents, fm_extent_count, fm_reserved; struct fiemap_extent fm_extents[]; }
// struct fiemap_extent { u64 fe_logical, fe_physical, fe_length, fe_reserved64[2]; u32 fe_flags, fe_reserved[3]; }  // 56 bytes
const fsIocFiemap = 0xC020660B
buf := make([]byte, 32+56)            // header + 1 extent
binary.LittleEndian.PutUint64(buf[8:], math.MaxUint64) // fm_length = whole file
binary.LittleEndian.PutUint32(buf[16:], 1)             // FIEMAP_FLAG_SYNC
binary.LittleEndian.PutUint32(buf[24:], 1)             // fm_extent_count = 1
// unix.Syscall(SYS_IOCTL, fd, fsIocFiemap, uintptr(unsafe.Pointer(&buf[0])))
// mapped := LE.Uint32(buf[20:]); if mapped == 0 → error "file has no extents"
// physical := LE.Uint64(buf[32+8:]); return physical / uint64(os.Getpagesize())
```

- [ ] **Step 4: Run** `go test ./internal/host/sleep/` — Expected: PASS. On the reference machine: `sudo ./bin/cairn host sleep-check` prints the table (no `--record`, nothing written).
- [ ] **Step 5: Commit** `feat: sleep pre-flight checks and resume offset`

---

### Task 11: Sleep events, boot check, notifications

**Files:**
- Create: `internal/host/sleep/record.go`
- Test: `internal/host/sleep/record_test.go`

**Interfaces:**
- Consumes: `events`, `execx.Runner`, `xdg.Dirs`.
- Produces:
  - CLI hidden `cairn host record-sleep <pre|post|fallback|reboot-required> [type]` → appends `sleep.pre` / `sleep.post` (data: `type`, `kernel`, `battery_percent` from `/sys/class/power_supply/BAT*/capacity` if present), `sleep.fallback`, or `kernel.reboot-required`, source `system-sleep` (or `package-hook` for reboot-required).
  - Doctor check `sleep.events.recent` (always OK/Warn): evidence lists the last 5 `sleep.*` events (time, type, result); Warn if any of them is `sleep.check.failed` or `sleep.fallback` newer than the last successful `sleep.post`.
  - `type sleep.Finding struct{ Kind, SleepType string; At time.Time }`; `func sleep.AnalyzePreviousBoot(evs []events.Event, currentBoot string) (sleep.Finding, bool)` — take the last event whose `Boot != currentBoot`; if its kind is `sleep.pre`, return `Finding{Kind: "boot.lost-during-sleep", SleepType: data.type}`.
  - CLI hidden `cairn host boot-check` (user unit at login): runs Analyze on `/var/lib/cairn/host/events.jsonl`; on a finding, sends the notification (below) and records it in user state `$XDG_STATE_HOME/cairn/host/findings.jsonl` (the system log is root-owned).
  - CLI hidden `cairn host notify` (user path unit): reads system events from the cursor stored in `$XDG_STATE_HOME/cairn/host/notify.cursor`, and for `sleep.check.failed`, `sleep.fallback`, `kernel.reboot-required` runs `notify-send --urgency=critical --app-name=cairn "<title>" "<body>"`; saves the new cursor. Message text: `Hibernation unavailable: <failed check titles>. Suspended instead. Run: cairn doctor host` / `Kernel upgraded: reboot before relying on hibernation`.

- [ ] **Step 1: Write failing tests**
  - `TestLostDuringSuspend`: events `[boot A: sleep.pre suspend]`, current boot B → finding with SleepType `suspend`.
  - `TestCleanResumeNoFinding`: `[A: sleep.pre hibernate, A: sleep.post hibernate]`, current B → no finding.
  - `TestNotifySendsOncePerEvent`: FakeRunner; two runs over the same log → one `notify-send` call.
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: sleep event recording, boot check and notifications`

---

### Task 12: NVIDIA provider

**Files:**
- Create: `internal/host/gpu/nvidia.go`
- Test: `internal/host/gpu/nvidia_test.go`, `testdata/gpu/nvidia-utils.conf` (copy of the vendor file shown in the spec research)

**Interfaces:**
- Consumes: `config.Nvidia`, `detect.System`, `plan`, `doctor`.
- Produces:
  - `func gpu.NvidiaActions(cfg config.Nvidia, sys detect.System, vendor []byte) []plan.Action` (empty unless `sys.GPU == "nvidia" && cfg.Manage`): `WriteFile /etc/modprobe.d/nvidia-utils.conf` class `Mask`, policy `gpu-nvidia`, content = header + vendor content where every `options nvidia` line's `NVreg_TemporaryFilePath=` value is replaced by `cfg.TemporaryFilePath` and `NVreg_PreserveVideoMemoryAllocations=1` is ensured on that line; `MakeDir <temporary_file_path>` (0700 root) unless it is `/var/tmp`. The entry records `Upstream=/usr/lib/modprobe.d/nvidia-utils.conf` and its sha.
  - `func gpu.NvidiaChecks(cfg config.Nvidia, sys detect.System, root fsx.Root, inv *inventory.Inventory, freeBytes func(path string) (uint64, error), vramBytes uint64) []doctor.Check`: `gpu.nvidia.params` (effective `TemporaryFilePath` in `sys.NvidiaParams` equals config; Warn "reboot to apply" if the managed file exists but params differ), `gpu.nvidia.space` (free bytes at the path ≥ VRAM × 1.05; Crit otherwise), `gpu.nvidia.upstream` (vendor sha differs from inventory → Warn "vendor file changed: run cairn host apply").
  - VRAM size from `nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits` (MiB) via the runner; Unknown if unavailable.

- [ ] **Step 1: Write failing tests**
  - `TestMaskReplacesTemporaryPath`: vendor fixture → generated file contains `blacklist nouveau`, `softdep nvidia post: nvidia-uvm nvidia-drm`, and `options nvidia NVreg_TemporaryFilePath=/home/.nvidia-suspend NVreg_PreserveVideoMemoryAllocations=1`, and no `=/var/tmp`.
  - `TestForeignMaskBackedUpAndRestored` (Review Focus 3): pre-existing `/etc/modprobe.d/nvidia-utils.conf` in temp root → after apply, backup exists; after `uninstall` (Task 13) original content is back. (Write the uninstall half in Task 13's tests; here assert backup + inventory entry.)
  - `TestParamsMismatchWarnsReboot`, `TestNotEnoughSpaceCrit`.
- [ ] **Step 2: Run** — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `feat: NVIDIA sleep settings provider`

---

### Task 13: Uninstall, repair, package hook, end-to-end round trip

**Files:**
- Create: `internal/host/uninstall.go`, `internal/host/repair/repair.go`
- Modify: `internal/cli/host.go` (uninstall, repair; wire Tasks 9–12 into `plan`/`apply`/`doctor`)
- Test: `internal/host/uninstall_test.go`, `internal/host/repair/repair_test.go`, `internal/host/e2e_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `func host.DesiredActions(cfg *config.Config, sys detect.System, self string, vendorNvidia []byte) []plan.Action` = `sleep.Actions` + `gpu.NvidiaActions` + a pacman hook (when `sys.PkgManager == "pacman"`): `/etc/pacman.d/hooks/50-cairn-reboot-required.hook` triggering on `linux*`, `nvidia*` upgrades, running `<self> host record-sleep reboot-required` (appends `kernel.reboot-required`). Add `/etc/pacman.d/hooks/50-cairn-reboot-required.hook` to the allowlist.
  - `func host.UninstallPlan(sysInv, userInv *inventory.Inventory) plan.Plan`: `RemoveFile` for DropIn/Mask entries (then restore `Backup` content if present), `SetGSetting` back to `Previous`, `DisableUnit` for UnitEnable entries whose `Previous` was `disabled`, daemon-reloads. `ManagedDir` entries are removed only if empty.
  - `func repair.Plan(root fsx.Root, sys detect.System, cfg config.Sleep, offset sleep.OffsetFunc) plan.Plan`:
    1. Conflicting units: any file in `/etc/systemd/system/systemd-hibernate.service.d/` or `systemd-suspend-then-hibernate.service.d/` not named `50-cairn.conf` that sets `OnFailure=`, and `/etc/systemd/system/sleep-fallback.service` → backed up to `/var/lib/cairn/host/backup/` and removed.
    2. systemd-boot only: for each entry in `/boot/loader/entries/*.conf` whose `linux` line matches the running kernel's image, set `resume=` (UUID of the swap's filesystem from `blkid -s UUID -o value <dev>`) and `resume_offset=<offset>` in the `options` line; backup first.
    3. Report only (no action): fallback entries pointing to missing initramfs images; `loader.conf` without `default`.
  - `cairn host sleep-check` runs `sleep.Checks` **and** `gpu.NvidiaChecks` (spec §5.2 item 4), so a full VRAM target also blocks hibernation at entry.
  - `cairn init --host`: when `<Config>/cairn.toml` does not exist, writes it with only detected values that differ from defaults — `host.sleep.hibernation_swap.path` (the active file swap with the lowest priority) — and prints `cairn config explain` hints; never overwrites an existing file.
  - CLI: `cairn host uninstall [--yes]`, `cairn host repair [--yes]` (separate plan, prints "requires reboot" when boot entries change).

- [ ] **Step 1: Write failing tests**
  - `TestRoundTrip` (e2e in a temp root with FakeRunner): fixture system → `apply` → all generated files present, inventory complete → second plan has no pending actions → `uninstall` → every generated file gone, foreign `nvidia-utils.conf` restored byte-for-byte, gsetting restored to its previous value, `plan` shows only `create` actions again.
  - `TestRepairRemovesConflictingFallback`: fixture with `systemd-hibernate.service.d/fallback.conf` (`OnFailure=sleep-fallback.service`) and `sleep-fallback.service` → both backed up and removed; `50-cairn.conf` untouched.
  - `TestInitWritesOnlyDetectedSwap`: fixture with two file swaps (pri 20 and -10) → written config contains `path = "/hibernate-swap/swapfile"` and nothing else under `[host]`; second run leaves the file untouched.
  - `TestRepairUpdatesResumeOffset`: entry `options ... resume=UUID=x resume_offset=1` with offset func returning 61112320 → options line ends with `resume_offset=61112320`, other options unchanged, backup present.
- [ ] **Step 2: Run** `go test ./...` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./... && just lint` — Expected: PASS, no lint errors.
- [ ] **Step 5: Commit** `feat: host uninstall, repair and pacman reboot hook`

---

### Task 14: Verification on the reference machine (manual, spec §12 steps 1–5, 9)

**Files:**
- Private (not in this repo): the owner's results log and real config in `~/.config/cairn/` (`cairn.toml` with the machine's values).
- Create in repo: `docs/compat.md` — one anonymised line: distro, systemd version, GPU/driver family, boot loader, result.

**Interfaces:**
- Consumes: the built binary installed with `just install`.

- [ ] **Step 1:** `just build && just install`; `cairn doctor host` — record output (expected before apply: lid/sleep policy Crit/Warn, nvidia params Warn).
- [ ] **Step 2:** `cairn host repair` — review plan with the owner, apply; reboot.
- [ ] **Step 3:** `cairn host apply` — review plan with the owner, apply; reboot (NVIDIA option takes effect).
- [ ] **Step 4:** `cairn doctor host` — expected all OK (record any non-OK with explanation).
- [ ] **Step 5:** dry runs as root: `echo platform > /sys/power/pm_test; echo disk > /sys/power/state; echo none > /sys/power/pm_test`; then `echo test_resume > /sys/power/disk; echo disk > /sys/power/state; echo platform > /sys/power/disk` — expected: returns without errors; `journalctl -b -g 'PM: hibernation'` shows success.
- [ ] **Step 6:** real `systemctl hibernate` → resume; `events.jsonl` has `sleep.pre` and `sleep.post` with type `hibernate`.
- [ ] **Step 7:** temporarily set `host.sleep.hibernate_after = "2min"`, apply, close lid on battery 3 minutes → machine hibernated (journal shows `suspend-then-hibernate` then hibernation); restore `15min`, apply.
- [ ] **Step 8:** failure path: set `host.sleep.hibernation_swap.path` to a non-existent file, apply, `systemctl hibernate` → notification "Hibernation unavailable…", machine suspends instead, `sleep.check.failed` and `sleep.fallback` in events; restore config, apply.
- [ ] **Step 9:** `cairn host uninstall` on a copy of the inventory in a temp root only (do not uninstall on the real machine unless the owner asks); record result.
- [ ] **Step 10: Commit** `docs: add tested configuration to compatibility list` (only `docs/compat.md`; personal values stay in `~/.config/cairn/`)
