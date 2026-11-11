# Program

cairn is built as a sequence of small projects. Each one gets a design document in
`docs/specs/`, reviewed before code is written. Status values: `idea` → `design` →
`approved` → `building` → `done`.

## Projects and order

| # | Project | Dir | Depends on | Status |
|---|---|---|---|---|
| 1 | Machine hardening | `host/` | — | design |
| 2 | Core engine + AI agents | `engine/`, `agents/` | 1 (shared foundation only) | design |
| 3 | Knowledge & playbooks | `knowledge/` | 2 (task records) | idea |
| 4 | Config hub | `confighub/` | 2 | idea |
| 5 | Surfaces (TUI + web) | `ui/` | 2, 3, 4 | idea |
| 6 | Editor (IDE-grade Neovim, remote work) | `nvim/` | 4 for config integration | idea |
| 7 | Migration of existing setups | `profiles/` | 2–6 | idea |

1 is independent and machine-specific, so it can run in parallel with the design of 2.
2 is designed as one unit because agent sessions are the most important workload the
engine manages. 3 builds on the task records of 2. 6 can be designed in parallel with 3–5.

## Decisions so far

- **D1. Restore means rebuild, never replay.** No process snapshots (no tmux-resurrect,
  no CRIU). Layout comes from declared state; programs resume their own state.
- **D2. tmux stays the multiplexer**, run as a `systemd --user` service. Zellij, WezTerm
  and kitty were evaluated; none solves power-loss restore better, and switching would
  cost the existing tmux/Neovim integration.
- **D3. The unit of AI work is a task:** a branch (and, when the user chooses, a git
  worktree) plus one or more agent conversations. Isolation is chosen per task.
  The AI workflow is allowed to differ significantly from the current one.
- **D4. AI tools are adapters described in config:** how to start, resume, fork, list,
  stop and delete, plus which capabilities the tool lacks (the manager fills gaps).
  Native CLI commands are preferred over parsing private history files.
- **D5. Three surfaces on one engine:** CLI, terminal popup UI, local web dashboard.
  Nothing is available only in a UI.
- **D6. Open-source and tool-agnostic.** The author's setup is a profile, not the product.
- **D7. Easy, non-invasive onboarding:** XDG paths, layered config, `cairn init` with tool
  detection, never overwriting the user's dotfiles, full `uninstall` (principles 11–13).
- **D9. Knowledge is a by-product of work:** every finished task can leave a short, strict
  record; knowledge is anchored to code, tasks and conversations, searchable by people and
  by agents, and flagged when the code it describes changes (see `knowledge/`).
- **D8. Real UI/UX design:** one design system shared by the terminal UI and web dashboard,
  validated against real tasks before implementation (principle 15); simple everyday mental model (principle 14).

## What is generic vs. what is specific

| Generic (product, works anywhere) | Specific (profile / user config) |
|---|---|
| Workspace model, reconciler, history, CLI | The list of projects, their paths and layouts |
| AI agent adapters for Claude, Codex, agy, others | Which tools a person uses, their accounts and profiles |
| Config hub, schemas, validation | The user's own tmux.conf, Neovim settings, keymaps |
| Sleep/backup/disk-hygiene modules | Hardware quirks (e.g. NVIDIA settings), swap layout, backup targets |
| Editor distribution and its integrations | Personal plugins, colour scheme, project-specific settings |

## AI architecture scope (applies to 2, 3 and 4)

The agents module and config hub must cover the current agent-harness landscape, not only
"start a chat in a pane":

- **Harness lifecycle:** start, resume, fork, background/foreground, stop, archive, delete;
  supervisor views (Claude `agents`, Codex `agents`) used where they exist.
- **Live state from hooks:** working / waiting for input / done / failed / stopped, with
  notifications on transitions and a "jump to waiting agent" action.
- **Context engineering:** CLAUDE.md / AGENTS.md / GEMINI.md and rule files visible per
  project, drift detection between them, imports and shared sections.
- **Tools and extensions:** MCP servers, skills, plugins, slash commands, subagents and
  hooks — one source of truth, rendered into each tool's native format.
- **Profiles:** model, reasoning effort, permission mode, sandbox, MCP set per session.
- **Parallelism:** worktrees per task, concurrency limits, review → merge/PR gate.
- **Memory and search:** cross-tool full-text index of conversations (local, redacted),
  names, tags, purpose, lineage (forked from, handed off from).
- **Cost and limits:** token/usage and quota view per tool, project and task.
- **Safety:** sandbox by default, no casual "skip permissions", one lock per
  conversation, secrets never indexed.
- **Evaluation hooks:** room to record outcomes (merged? reverted?) so workflows can be
  compared over time.

## Open questions

- Implementation language for engine/CLI/TUI (Go single binary is the leading option).
- Name of the project (cairn is a working name).
- Uncommitted-work safety net: git WIP refs (no workflow change) vs jujutsu.
- Remote development model for the editor: remote Neovim server vs SSH filesystem vs both.

## Evidence

Research done on 2026-11-11 (terminal persistence tools, AI CLI resume capabilities,
agent managers, hibernation on Arch/NVIDIA) is summarised in the design discussion and will
be cited in each spec. Key facts:

- tmux-resurrect/continuum: unmaintained since 2024, known snapshot-corruption bugs.
- Claude Code 2.1.x: `--session-id`, `--name`, `--resume`, `--fork-session`, `--bg`,
  `agents --json`, ~30 hook events. Codex 0.167: `resume`/`fork`/`archive`/`delete` by
  name, hooks, `agents`. agy 1.0.16: `--continue`, `--conversation <id>`; no naming,
  listing or fork.
- Declarative tmux layout tools (tmuxp, tmuxinator, sesh, laio) are active and mature.
