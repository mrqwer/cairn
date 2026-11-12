# Spec: host — machine hardening

Status: **design** · Module dir: `host/` · Date: 2026-11-11

## 1. Intent

A developer's work must not depend on a laptop sleeping and waking correctly. Today, on the
reference machine, closing the lid can end in a dead battery and a cold boot; a failed
hibernate silently turns into an indefinite suspend; the root disk fills up until logs and
GPU sleep data have nowhere to go; nothing is backed up; uncommitted work exists only in
working trees. `host` makes the machine underneath cairn dependable and self-maintaining,
and makes every one of those failure modes either impossible or loud.

It is generic (any systemd-based Linux) with machine-specific values kept in a profile
(principle 13).

## 2. Goals

1. **Sleep never loses state silently.** Lid close and idle lead to a sleep that ends in
   hibernation before the battery can die. Any failed hibernate or resume is reported.
2. **Disks stay healthy without attention.** Every growing thing (journals, package caches,
   container build caches and images, language caches, coredumps) has a limit and a cleanup
   that runs on its own; low space is reported before it causes failures.
3. **Work is backed up.** Code, configs, editor state and AI agent histories are backed up
   encrypted, deduplicated and on a schedule, and restores are tested automatically.
4. **Uncommitted work survives anything.** Every configured repository's working tree is
   snapshotted every few minutes without touching branches, the index or the working tree.
5. **One place to see it all.** `cairn doctor host` shows the state of all of the above with
   the exact command to fix each problem.

## 3. Non-goals and anti-goals

- Non-goal: managing the whole OS (packages, users, network). Only the areas above.
- Non-goal: process checkpointing (CRIU) — ruled out in D1.
- Anti-goal: changing system files the user owns. Changes are cairn-owned drop-ins and
  managed copies that `cairn host uninstall` removes completely (principle 11); the few
  one-time repairs that cannot be drop-ins are separate, explicit and backed up (4.1).
- Anti-goal: anything privileged happening without the user seeing the plan first.
- Anti-goal: cleanup that can destroy user data (container **volumes**, git objects that are
  still referenced, files outside declared cache paths) — never by default.

## 4. Architecture

```
host config (TOML, layered)      providers (detected)              generated artifacts
  sleep   ─┐                      init: systemd (logind, sleep,     drop-ins under *.d/,
  disk    ─┤   cairn host plan →  journald, timers)                 user/system units,
  backup  ─┤   (diff, needs-root  pkg: pacman | apt | dnf           sleep hooks,
  wip     ─┤    marked)           containers: docker | podman        state + event log
  gpu     ─┘   cairn host apply → gpu: nvidia | amd | intel | none
                                  backup: restic
```

- **Policies** are declarative config (what should be true). **Providers** know how to
  make it true on a given system and are chosen by detection, overridable in config.
- `cairn host plan` prints every file it would create/change/remove and every command it
  would run, grouped into *user-level* and *needs root*. `cairn host apply` executes the
  plan; root parts run through `sudo` once.
- **Privilege model (v1).** The unprivileged side never hands actions, file content or a
  plan file to root. `cairn host apply|uninstall|repair` sends only a small request on
  stdin (format version, mode, host config) to `sudo <cairn> host apply-root --mode <m>`.
  The root side validates that config, regenerates its own actions from it with its own
  detection, checks every action against the root allowlist and every target directory
  chain (root-owned, not group/world writable, no symlinks), prints its plan and asks on
  `/dev/tty` (or `--yes`); only then does it write. The user's `plan` shows root's part as
  a labelled preview. On `apply` the privileged step runs first and user-level changes
  (GNOME idle settings, user units) are applied only after it succeeded, so idle
  hibernation never takes effect without the sleep-entry guard. Root-side commands
  (`apply-root`, `sleep-check`, `record-sleep`) never read the user's XDG config; they use
  the host config snapshot `/var/lib/cairn/host/config.json` written by `apply-root`.
- Every generated file starts with a header: `# Managed by cairn host (<policy>). Do not edit; run "cairn host ..."`.
  An inventory of managed files is kept in `$XDG_STATE_HOME/cairn/host/managed.json`;
  `uninstall` uses it and restores any file it had to replace from a saved copy.
- Re-applying is idempotent. Applying after a crash mid-way converges (principle 2).
- **Drift is detected, not auto-fixed, on the root side.** Package updates rewrite vendor
  files, a recreated swapfile changes its offset, an initramfs rebuild can drop a hook.
  `doctor`, the sleep-entry checks and the package hooks detect it; fixing it is always a
  visible `cairn host apply`. No root timer changes the system on its own.

### 4.1 Classes of change

| Class | Examples | Apply | Uninstall |
|---|---|---|---|
| Drop-ins (preferred) | `logind.conf.d`, `sleep.conf.d`, `journald.conf.d`, `coredump.conf.d`, unit drop-ins, tmpfiles rules, cairn's own units/timers | write managed file | delete it |
| Masking copies | NVIDIA modprobe file with the vendor's name | copy vendor content + cairn options; record vendor checksum; re-sync when the vendor file changes (package hook + doctor) | delete it (vendor file applies again) |
| User-level settings | GNOME gsettings for idle/lid actions | record previous values | restore them |
| Enabling existing units | `paccache.timer`, vendor GPU sleep units | record previous state | restore previous state |
| One-time repairs (`cairn host repair`) | boot entries (`resume=`, `resume_offset`), boot-loader default, broken fallback entries, initramfs `resume` hook + regeneration, swapfile creation/resizing, removing a conflicting pre-existing unit | explicit command, separate plan, each file backed up to `/var/lib/cairn/host/backup/` | **not** reverted automatically (could leave the machine unbootable); `uninstall` lists them and how to restore from the backups |

Supported boot loaders for repairs in v1: systemd-boot (the reference machine), with Type
#1 entries for the running kernel. GRUB, UKIs and other boot loaders are report-only in
v1: repair prints the exact `resume=` / `resume_offset=` values to set by hand. Repairs
are recorded in the system inventory (class `repaired`, with their backup) so `uninstall`
can list them.

### 4.2 Shared foundation

`host` is built first, so it also builds the foundation every module reuses — not a
throwaway: layered config loading with schema validation and `config explain`, the
managed-file inventory, the `doctor` framework (checks with status, evidence, fix), the
event log format, and provider detection. The engine spec extends these; it does not
replace them. `cairn init` in v1 covers host detection only.

### 4.3 Platform scope for v1

- Arch Linux with systemd ≥ 256 is the tested target; Debian/Ubuntu/Fedora providers are
  designed for (package-manager and boot-loader abstractions) but not tested in v1.
- GNOME is the supported desktop for idle settings; other desktops get documented manual
  steps and a doctor check.
- Hibernation swap: a swap file on ext4/xfs or a swap partition (zram is never used).
  Swap files on btrfs are not supported in v1 (FIEMAP does not give their resume
  offset); the resume check reports them as critical.
- Secure Boot with kernel lockdown disables hibernation: detected and reported; sleep
  policy falls back to suspend with the warning from 5.2.
- Single primary user per machine (the one who ran `cairn host apply`); multi-user is out
  of scope for v1.
- Implementation lives in the `cairn` binary (Go, single static binary — builder's
  language for the whole project, see engine spec; for `host` it only shells out to
  `systemctl`, `journalctl`, `restic`, `git`, package managers).

## 5. Sleep

### 5.1 Policy (generic defaults)

| Setting | Default | Mechanism |
|---|---|---|
| Lid close (battery) | suspend-then-hibernate | `logind.conf.d/50-cairn.conf`: `HandleLidSwitch=` |
| Lid close (AC) | suspend-then-hibernate (stays suspended on AC) | `HandleLidSwitchExternalPower=`, `HibernateOnACPower=no` |
| Lid close (docked) | ignore | unchanged default |
| Hibernate after | 15 min (configurable), or earlier when battery < 5% | `sleep.conf.d/50-cairn.conf`: `HibernateDelaySec=15min`, `SuspendEstimationSec=15min` |
| Idle (desktop) | same action as lid | GNOME: set idle action consistently; other desktops: document |
| Critical battery | hibernate | UPower `CriticalPowerAction=Hibernate` via drop-in where supported |
| Failed hibernate | suspend instead + alert (see 5.2) | `OnFailure=` fallback unit: logs the event, then suspends |

### 5.2 Pre-flight checks (run by `plan`, `doctor`, and at sleep entry)

The checks run as `ExecStartPre=` in cairn drop-ins for `systemd-hibernate.service` and
`systemd-suspend-then-hibernate.service` (a failing `ExecStartPre` stops the unit; a
`system-sleep` hook could not). They run **at sleep entry** (lid close / menu / idle), while
the user can still see a notification — not at the delayed hibernate step in a bag.

When a check fails at entry (decided, same on battery and AC):

1. A notification names the reason and the fix ("kernel was upgraded — reboot to make
   hibernation safe"), and the event is logged.
2. The system **suspends instead** (staying awake in a bag is worse), and a persistent
   "hibernation unavailable" warning is shown in `doctor` and the status bar until fixed.
3. On resume, the warning is repeated if the condition still holds.

If the delayed hibernate inside suspend-then-hibernate fails anyway, systemd returns to
suspend; the resume check (5.4) reports it. Kernel upgrades are the common case, so a
package-manager hook (pacman hook / apt/dnf equivalents) shows "reboot required for safe
sleep" right after a kernel or GPU-driver upgrade.

Checks:

1. A swap device/file large enough for the image exists and is the one the kernel resumes
   from: `/sys/power/resume` + `resume_offset` match a configured hibernation swap
   (offset computed from `filefrag -v` physical offset for files).
2. Free space in that swap ≥ estimated image size (`/sys/power/image_size` or used RAM).
3. The running kernel equals the installed default kernel (no hibernating after a kernel
   upgrade without reboot — the resume would boot a different kernel and discard the image).
4. GPU provider checks (5.3).
5. The resume hook/generator is present in the initramfs config (mkinitcpio `resume` hook,
   or systemd-based initramfs).

Every sleep transition is written by a small root-side recorder (a `system-sleep` hook used
only for logging, plus the `ExecStartPre` results) to `/var/lib/cairn/host/events.jsonl`
(time, type, kernel, battery %, result), readable by the primary user; the user daemon reads
it for notifications and `doctor`.

### 5.3 GPU providers

- **nvidia:** VRAM preservation enabled; `NVreg_TemporaryFilePath` points to a directory on
  a filesystem with free space ≥ VRAM size + 5%; the effective value is verified in
  `/proc/driver/nvidia/params` (because distro files in `/usr/lib/modprobe.d` can override
  `/etc/modprobe.d`, cairn writes its options under the **same filename** as the distro
  file so it masks it, copying the distro file's other lines). Vendor sleep units enabled.
  Doctor flags a mismatch between configured and effective values.
- **amd / intel / none:** no special handling initially; providers can add checks later.

### 5.4 Detecting silent failures

At boot, a user unit compares the last event of the previous boot with the current boot:

- previous boot ended in `suspend` with no resume, and this is a cold boot → **"battery died
  during suspend"** notification;
- previous boot logged `hibernate` but the kernel reports no image restored → **"resume
  failed"** notification with the relevant journal lines;
- any `OnFailure` from sleep units → notification.

Notifications use the desktop notification bus when available and are always recorded in
the event log so `doctor` can show them later.

### 5.5 Removed by `host` (reference machine)

The custom "fall back to suspend when hibernate fails" unit is replaced by 5.1/5.4; it is
removed by `cairn host repair` (backed up first), not by `apply`.

## 6. Disk hygiene

Each rule has: target, limit (size and/or age), schedule, and whether it may delete user
data (never by default).

| Area | Default rule | Mechanism |
|---|---|---|
| systemd journal | `SystemMaxUse=1G`, `SystemKeepFree=5G` | `journald.conf.d/50-cairn.conf` |
| coredumps | `MaxUse=2G` | `coredump.conf.d/50-cairn.conf` |
| package cache | keep last 2 versions, remove uninstalled | pacman: `paccache.timer` + hook; apt: `apt-get autoclean` timer; dnf: `clean_requirements_on_remove`/`dnf clean` timer |
| container build cache | prune unused older than 14 days | `docker builder prune --filter until=336h` (podman equivalent) weekly |
| container images | prune dangling weekly; unused older than 30 days **opt-in** | `docker image prune` |
| container volumes | **never** automatically; report unused volumes and sizes | doctor only |
| user caches | per-directory caps for known caches (`go-build`, `pip`, `npm`, `yarn`, `pnpm`, `cargo` registry, `~/.cache/*` generic) | user timer; only inside declared cache paths |
| tmp | age-based, only cairn-declared temp dirs | systemd-tmpfiles rules |
| trash | **opt-in** age-based emptying (it is user data) | user timer |
| AI tool transcripts | **never** deleted by cairn; Claude `cleanupPeriodDays` raised (agents spec) | — |

**Watch:** each mounted filesystem has warn/critical thresholds (default 85% / 95%);
crossing one sends a notification naming the largest growing directories (sampled with
`du` on declared roots, never a full-disk scan on a schedule).

## 7. Backups

- **Two backup kinds, chosen per target in config:**
  - `snapshot` (default): restic — versioned, encrypted, deduplicated. cairn generates the
    restic config, units and timers; it does not wrap restic's data format.
  - `mirror`: a plain, browsable file copy (rsync, with `--delete` off by default) for people
    who want "just the files" on a disk or server. No history unless the target keeps it.
- **What (default set, configurable):** declared project roots; `$XDG_CONFIG_HOME`
  (excluding caches); `$XDG_STATE_HOME/nvim`; AI tool histories (`~/.claude`, `~/.codex`,
  `~/.gemini` — excluding credentials files); cairn's own state.
- **Excludes (default):** regenerable directories only — `node_modules`, `target`,
  `.venv`, `dist`, `build`, `__pycache__`, `.cache`. Large files are **not** excluded by
  default; doctor reports files over a size threshold so the user can decide.
  `.gitignore`-aware exclusion is an option.
- **Credentials are never backed up in plain form:** `auth.json`, `.credentials.json`,
  `oauth_creds.json`, `*.key` and similar are excluded by default.
- **Schedule and retention:** hourly; `--keep-hourly 24 --keep-daily 7 --keep-weekly 4
  --keep-monthly 12`. Runs on AC or above 30% battery; skipped runs are caught up.
- **Repository password:** stored in the desktop secret service (keyring). By default
  backups run only while the user is logged in and the keyring is unlocked; runs missed
  while locked are caught up after login. Headless backups (password in a 0600 file under
  `$XDG_CONFIG_HOME/cairn/secrets/`) are an explicit opt-in. Never in config files.
- **Removable or sometimes-offline targets** (external disk, a server on another network):
  the job checks reachability first; "unreachable" is recorded as *skipped*, not failed, and
  doctor only warns when a target has not received a successful backup for longer than its
  configured `max_age` (default 3 days). External disks also trigger a backup when mounted.
- **Verification:** weekly `restic check --read-data-subset=5%`; monthly automatic restore
  of a random sample of files into a temp directory and byte comparison. Doctor shows the
  age of the last successful backup and last successful verification.
- **Targets (any number, each with its own kind and schedule):** a local directory or
  external disk (`path:`), a server over SFTP/SSH (`sftp:` / rsync over SSH), a restic REST
  server (`rest:`), S3-compatible storage (`s3:`), or any rclone remote (`rclone:`).
  No target is configured by default; `cairn init` asks and offers to test it.

## 8. Uncommitted-work safety net (WIP snapshots)

For every repository under declared roots, every 5 minutes (user timer):

1. Skip if the working tree and untracked files are unchanged since the last snapshot
   (compare `git status --porcelain=v2` hash + tree hash).
2. Build a tree from the working tree **using a temporary index** (`GIT_INDEX_FILE`), so
   the real index and working tree are never touched; include untracked files, honour
   `.gitignore`, skip files over a size cap (default 20 MB).
3. `git commit-tree` with parent = current `HEAD` only (or none on an unborn branch) —
   snapshots are **not chained**, so old ones become unreachable and collectable. Update
   `refs/wip/<worktree>/<branch>` with a reflog entry; the reflog *is* the snapshot history.
   `<worktree>` is a stable short id of the worktree path (main checkout = `main`); a
   detached HEAD uses `detached-<short-sha>`.
4. Retention: the job runs `git reflog expire --expire=14.days refs/wip/...` for its refs;
   expired snapshots are removed by normal `git gc`. A per-repo cap on total snapshot size
   (default 500 MB of new blobs per 14 days) stops snapshotting with a doctor warning
   instead of growing without limit.
5. Never pushes; `refs/wip/*` are local only (doctor warns if a remote fetch/push refspec
   would include them).

Restore: `cairn wip list [repo]`, `cairn wip show <snapshot>`, `cairn wip restore <snapshot>
[path...]` (restores into the working tree after confirmation, or into a new branch).

Locking: skip a repository if a git operation is in progress (`index.lock`, rebase/merge
state). Performance: run with low CPU/IO priority; cap the number of repos per run.

## 9. Health (`cairn doctor host`)

One table, every row with status, evidence and fix command:

- sleep policy applied; lid/idle/critical actions consistent;
- hibernation swap: size, free, resume target matches, initramfs resume support;
- running kernel vs installed kernel;
- GPU effective sleep parameters vs configured (nvidia);
- last 5 sleep events and their results; any silent-failure findings;
- filesystems usage vs thresholds; top growth since last week;
- each cleanup rule: last run, bytes freed;
- backup: last success, last verification, repository size, targets reachable;
- WIP: repos covered, last snapshot age, failures.

Exit code is non-zero if anything is critical, so it can run in CI-like checks or a
status bar.

## 10. Configuration surface (example)

```toml
# $XDG_CONFIG_HOME/cairn/host.toml  (or the profile)
[sleep]
lid = "suspend-then-hibernate"
hibernate_after = "15min"
critical_battery = "hibernate"

[sleep.hibernation_swap]
path = "/home/.swap/swapfile"      # profile value; detected by `cairn init` if absent

[gpu.nvidia]
temporary_file_path = "/home/.nvidia-suspend"

[disk.watch]
warn = "85%"
critical = "95%"

[backup]
[[backup.target]]
name = "server"
kind = "snapshot"
url = "sftp:backup@server:/srv/restic/laptop"

[[backup.target]]
name = "usb-disk"
kind = "mirror"
url = "path:/run/media/$USER/backup/laptop"
roots = ["~/projects", "~/DevLog"]

[wip]
roots = ["~/projects", "~/DevLog"]
interval = "5min"
```

Everything has a default; `cairn init` writes only what it detected and what differs from
defaults.

## 11. Reference machine profile (`profiles/<author>/host.toml`)

Findings that drive the profile values (from the investigation on 2026-11-11):

- Lid close is plain `suspend`; hibernate failures fall back to plain suspend → replaced.
- NVIDIA (`nvidia-open` 615.x) options in `/etc/modprobe.d/` are overridden by
  `/usr/lib/modprobe.d/nvidia-utils.conf`; VRAM is dumped to `/var/tmp` on a small root fs.
- Two 24 GB swapfiles (`runtime-swapfile`, pri 20; `swapfile`, pri -1/-10) plus zram;
  which file `resume_offset` points to is unverified → the profile names `swapfile` as the
  hibernation swap and `apply` recomputes the offset and updates boot entries if needed.
- Boot loader: fallback entries reference missing initramfs images; no default entry set.
- Root fs reached 97%; journal retained only ~3 days; no backups; no WIP snapshots.

## 12. Verification plan

Run on the reference machine after `apply`, each step logged:

1. `cairn doctor host` all green (or every red explained).
2. Hibernate dry runs: `pm_test` levels `freezer`, `devices`, `platform`; then
   `test_resume` (writes and reads the image without powering off).
3. Real `systemctl hibernate`; confirm resume and event log entries.
4. Suspend-then-hibernate with `HibernateDelaySec=2min` on battery; confirm transition to
   hibernate and resume.
5. Simulated failure: point the hibernation swap check at a too-small file → hibernate is
   refused with a notification; the machine suspends instead + alert (5.2).
6. Disk: fill a test cache dir over its cap → cleanup trims it; cross a watch threshold on a
   loop-mounted test filesystem → notification.
7. Backup: initial backup, `restic check`, random-sample restore matches.
8. WIP: modify + add untracked file in a test repo; snapshot appears under `refs/wip/`;
   index and working tree unchanged (checksums); restore into a new branch works.
9. `cairn host uninstall` → every managed file removed or restored; `plan` shows a clean
   system.

Automated tests: providers are tested against fixture system states (fake `/sys`, fake
`systemctl`/`restic`/`git` outputs); the WIP snapshot logic is tested on real temporary
git repositories; plan/apply/uninstall round-trip is tested in a container.

## 13. Owner decisions

- Backups: configurable per target — `snapshot` (restic) or `mirror` (plain files), local or remote over standard protocols.
- Hibernate 15 minutes after suspend (configurable).

## 14. Builder's calls

Exact unit names, file layout under `host/`, notification text, and the list of known
language caches are left to the builder, within the principles.
