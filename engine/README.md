# engine — core

**Status: idea.** Design: `docs/specs/engine.md` (not written yet).

The desired-state model and everything that applies it.

- Schema for projects, workspaces, windows, panes and tools; validation before apply.
- Reconciler: compares desired state with running tmux and changes only the difference;
  idempotent and safe after a crash mid-way. Restore after reboot is just reconcile.
- History: every change is an event with a diff and an undo.
- `plan` (preview) / `apply` (live) / `capture` (import manual tmux changes back).
- CLI, and the API the terminal UI and web dashboard use.
- tmux server supervised as a `systemd --user` service.
