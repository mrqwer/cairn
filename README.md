# cairn

> Working name. A cairn is a stack of stones that marks the path and survives the weather.

A crash-only developer workspace system for Linux: terminal workspaces, AI coding agents,
an IDE-grade Neovim and the machine underneath them, all declared, versioned and editable
live from a CLI, a terminal UI and a web dashboard.

It is built to be used by anyone: it adopts *your* tmux, Neovim and AI-tool configs instead
of imposing a fixed setup. The author's own setup is just one profile in `profiles/`.

**Status:** the shared foundation, the optional Linux-laptop module (`cairn host`: sleep,
hibernation, NVIDIA) and the workspace engine (M1; end-to-end verification pending) are
implemented. AI agent sessions are the next milestone; the rest is in design.
Every module gets a reviewed design document in `docs/specs/` before any code is written.

## Quickstart

Needs Go, tmux 3.2+ and a Linux user session. Full guide: [docs/guide/workspace.md](docs/guide/workspace.md).

```
go build -o cairn ./cmd/cairn          # then put it on PATH
mkdir -p ~/code/api                    # the project root must exist
cairn project add api --root ~/code/api --template dev
cairn up                               # create every project's session
cairn open api                         # switch to it
cairn doctor workspace                 # read-only health check
```

Try it without touching your tmux: add `--socket try` to every command.

## Why

Snapshot-and-replay session restore (tmux-resurrect and friends) photographs running
processes and tries to re-run them. It breaks on power loss, failed hibernation, renamed
processes and corrupted snapshots, and every workaround adds another layer of glue.
cairn starts from the opposite assumption: the machine *will* die at the worst moment,
so recovering must be the same code path as starting.

## Modules

The workspace is the product. Everything else is built on it or optional.

| Dir | Module | What it owns |
|---|---|---|
| [`engine/`](engine/) | Workspace engine | projects, sessions and windows declared in config, applied live to tmux, history and undo, one CLI |
| [`agents/`](agents/) | AI agents | task-centric agent sessions, tool adapters, live state, search, notifications |
| [`knowledge/`](knowledge/) | Knowledge | project knowledge base anchored to code and tasks, playbooks, artifacts |
| [`confighub/`](confighub/) | Config hub | every config (tmux, Neovim, AI tools, MCP) visible, validated, versioned |
| [`ui/`](ui/) | Surfaces | terminal popup UI and local web dashboard on top of the engine |
| [`nvim/`](nvim/) | Editor | IDE-grade Neovim distribution, config-driven integrations, remote/server work |
| [`host/`](host/) | *Optional:* Linux laptop hardening | sleep and hibernation that never silently lose work, disk hygiene, backups, uncommitted-work snapshots |
| [`profiles/`](profiles/) | Example profiles | anonymised examples to copy; a person's own profile lives in `~/.config/cairn/` |

Every module is usable on its own. Modules talk only through the engine's public model and
CLI, never through each other's files. `host` is optional and machine-aware (Arch, systemd,
NVIDIA today); nothing else depends on it.

## Docs

- [Principles](docs/principles.md): the ideas the whole system rests on.
- [Program](docs/program.md): the projects, their order, dependencies and decisions so far.
- `docs/specs/`: one design document per module, written and reviewed before building.
