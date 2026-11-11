# nvim — IDE-grade editor

**Status: idea.** Design: `docs/specs/nvim.md` (not written yet).

A Neovim distribution with the capabilities people expect from JetBrains IDEs and
VS Code, built with a clean, layered architecture and driven by configuration files
instead of hardcoded, author-specific code.

## Goals

- **IDE features:** LSP (navigation, refactoring, code actions, inlay hints), debugging
  (DAP), test explorer, task runner, symbol/outline views, search and replace across the
  project, git integration (blame, history, diff, conflicts, PR review), database client,
  HTTP client, notebooks/REPL where useful, AI completion and agent integration.
- **Config-driven integrations:** project and tool settings come from declarative files.
  Where a standard exists, it is read directly (for example `.vscode/launch.json`,
  `.vscode/tasks.json`, `.editorconfig`, `devcontainer.json`) so projects stay portable
  between editors; anything else uses cairn's own schema-validated config.
- **Remote and server work as a first-class mode:** the equivalent of VS Code Remote-SSH
  and JetBrains Gateway — open a project on a server, run LSP/DAP/terminals/tests there,
  browse and edit remote files, port forwarding, logs, containers and databases on the
  server, SSH host management. (Approach to be researched: remote Neovim server vs SSH
  filesystem vs both.)
- **Reliability:** autosave, swap, persistent undo and sessions tuned so a power loss
  loses nothing; no plugin windows or terminals that break session restore.
- **Architecture:** a small core, feature modules with clear boundaries, one place for
  keymaps and options, no hardcoded paths or project names; every module can be turned
  off in config.

## Non-goals

- Replacing the user's config wholesale: the existing config is migrated module by module
  into a profile, keeping what works.
