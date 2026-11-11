# host — machine hardening

**Status: idea.** Design: `docs/specs/host.md` (not written yet).

Makes the machine underneath the workspace dependable and self-maintaining. Reusable
modules for any systemd-based Linux, plus a profile for a concrete machine.

- **Sleep:** suspend-then-hibernate on lid close, one dedicated hibernation swap, GPU
  driver (NVIDIA) sleep settings verified, critical-battery hibernate, alert when a
  resume failed or the battery died during suspend.
- **Disk hygiene:** bounded journals and logs, package-cache rotation, container
  image/build-cache policy, per-directory cache rules; nothing grows without limit.
- **Backups:** encrypted, deduplicated, scheduled (restic or kopia), including AI agent
  transcripts; restore is tested, not assumed.
- **Uncommitted-work safety net:** periodic snapshots of every repo working tree that never
  touch branches or the index.
- **Health:** everything above reports into `cairn doctor`.
