# cairn

> Working name. A cairn is a stack of stones that marks the path and survives the weather.

A crash-only developer workspace system for Linux: terminal workspaces, AI coding agents,
an IDE-grade Neovim and the machine underneath them, all declared, versioned and editable
live from a CLI, a terminal UI and a web dashboard.

It is built to be used by anyone: it adopts *your* tmux, Neovim and AI-tool configs instead
of imposing a fixed setup. The author's own setup is just one profile in `profiles/`.

**Status: design.** Nothing in this repository is implemented yet. Each module gets a
reviewed design document in `docs/specs/` before any code is written.

## Why

Snapshot-and-replay session restore (tmux-resurrect and friends) photographs running
processes and tries to re-run them. It breaks on power loss, failed hibernation, renamed
processes and corrupted snapshots, and every workaround adds another layer of glue.
cairn starts from the opposite assumption: the machine *will* die at the worst moment,
so recovering must be the same code path as starting.

## Modules

| Dir | Module | What it owns |
|---|---|---|
| [`host/`](host/) | Machine | sleep/hibernate, disk hygiene (logs, caches, journals), backups, health checks |
| [`engine/`](engine/) | Core engine | desired-state model, reconciler for tmux, change history and undo, CLI |
| [`agents/`](agents/) | AI agents | task-centric agent sessions, tool adapters, live state, search, notifications |
| [`knowledge/`](knowledge/) | Knowledge | project knowledge base anchored to code and tasks, playbooks, artifacts |
| [`confighub/`](confighub/) | Config hub | every config (tmux, Neovim, AI tools, MCP) visible, validated, versioned |
| [`ui/`](ui/) | Surfaces | terminal popup UI and local web dashboard on top of the engine |
| [`nvim/`](nvim/) | Editor | IDE-grade Neovim distribution, config-driven integrations, remote/server work |
| [`profiles/`](profiles/) | Profiles | user-specific configuration; the migration target for existing setups |

Every module is usable on its own. Modules talk only through the engine's public model and
CLI, never through each other's files.

## Docs

- [Principles](docs/principles.md): the ideas the whole system rests on.
- [Program](docs/program.md): the projects, their order, dependencies and decisions so far.
- `docs/specs/`: one design document per module, written and reviewed before building.
