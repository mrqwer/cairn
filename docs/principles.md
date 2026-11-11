# Principles

These are the rules every module is designed and reviewed against. When a design choice
conflicts with one of them, the design changes or the principle is amended explicitly here.

## 1. Crash-only

There is no clean-shutdown path that recovery depends on. Starting up *is* recovery, so a
dead battery, a failed hibernate and a normal boot all lead to the same result.
(Candea & Fox, "Crash-Only Software", HotOS 2003.)

Consequences: no "save on exit" hooks as the source of truth; all state is written
continuously and atomically (write-to-temp + rename, fsync where it matters).

## 2. Declare the desired state; reconcile continuously

The user declares what should exist (projects, windows, agents, settings). A controller
compares desired with actual state and changes only the difference. Reconciliation is
level-triggered and idempotent: running it twice, or after a crash mid-way, is safe.
(The controller pattern behind Borg/Kubernetes.)

Consequences: "restore" is just reconciling against an empty tmux; editing the structure
applies live without restarts; `capture` imports manual changes back into desired state.

## 3. Each tool owns its own state (end-to-end)

Neovim restores its own sessions, undo and swap; AI CLIs resume their own conversations;
git owns the code. cairn orchestrates and never reconstructs a tool's state by parsing
its private files when a supported command exists.
(Saltzer, Reed & Clark, "End-to-End Arguments in System Design", 1984.)

## 4. Every change is an event

Changes to desired state are appended to a history log: who/what changed it, the diff, and
how to revert it. Nothing is overwritten silently, so deleting is never scary.

## 5. Validate before apply

All configuration has a schema. Invalid config is rejected with a precise error before it
touches the running system. Every apply shows a plan (diff) first, like `terraform plan`.

## 6. Contracts at the edges

Integrations with outside tools (tmux, AI CLIs, Neovim, systemd) go through adapters with a
written contract and a conformance test suite. A tool update that breaks an adapter shows
up as a failing check, not as silent misbehaviour.

## 7. Config-driven, not author-specific

Behaviour comes from configuration files, not from hardcoded paths, names or habits. A new
AI tool, layout or integration is a config entry. Defaults are sensible; nothing assumes
the author's machine.

## 8. Observable

Every module reports its health in one place (`cairn doctor`): what is running, what is
stale, what failed and why, with the command to fix it.

## 9. Safe by default

Destructive actions need confirmation and are reversible where possible (archive before
delete; never discard unpushed commits). AI agents run sandboxed by default; secrets are
never indexed or logged.

## 10. Bounded resources

Nothing grows without limit: logs, caches, indexes, snapshots and histories all have size
or age limits and cleanup that runs on its own.

## 11. Non-invasive install, no hardcoded paths

cairn follows the XDG Base Directory specification: config in `$XDG_CONFIG_HOME/cairn`,
state in `$XDG_STATE_HOME/cairn`, data in `$XDG_DATA_HOME/cairn`, cache in
`$XDG_CACHE_HOME/cairn`, runtime in `$XDG_RUNTIME_DIR`. Every path is derived from these,
from `$HOME`, or from config; nothing in code names a user, machine or project.

It never overwrites files it does not own. It integrates by *adding* one documented line
where a tool supports includes (`source-file` in tmux, `@AGENTS.md`-style imports, `include`
in git config), by its own separate Neovim app (`NVIM_APPNAME=cairn`), or by generating files
in its own directories. System-level changes (sleep, journald, timers) are drop-in files in
`*.d/` directories, each marked as managed by cairn, removable with one command.
`cairn uninstall` restores the system exactly; `cairn install --dry-run` shows every file it
would touch.

## 12. Layered configuration with an easy start

Configuration resolves in layers, later overriding earlier, each layer optional:
built-in defaults → system (`/etc/cairn`) → user (`$XDG_CONFIG_HOME/cairn`) → profile →
project (`.cairn.toml` in a repo) → environment variables → command-line flags.
`cairn config explain <key>` shows the effective value and which layer set it.

A new user runs `cairn init`: it detects installed tools (tmux, Neovim, AI CLIs, git
repositories), proposes a minimal config, and works with zero lines written by hand.
Everything beyond that is opt-in.

## 13. Product vs profile

The product (`engine/`, `agents/`, `confighub/`, `ui/`, `nvim/`, `host/` modules) contains
only generic behaviour. Everything specific to one person or machine (project list, paths,
tool choices, keymaps, hardware quirks such as a particular GPU) lives in a profile under
`profiles/` or in the user's own config directory. A rule for reviews: if a change mentions
a specific user, host, project or path, it belongs in a profile.

## 14. Simple to learn, deep when needed

The everyday mental model fits on one screen: three nouns (**project**, **task**,
**agent**) and a handful of verbs (`open`, `new`, `find`, `finish`, `undo`). `cairn` with no
arguments opens the terminal UI on what matters now (what is running, what is waiting for
you). Everything else — templates, policies, profiles, adapters, layers — is progressive
disclosure: present, documented, but never required to start. Defaults are chosen so a
developer can use cairn for a week without reading anything beyond the README's first
screen. Every new feature is reviewed for "does this add a concept the user must learn?".

## 15. Designed, not generated

The terminal UI and the web dashboard follow one written design system: typography scale,
spacing grid, colour tokens (light and dark, readable contrast), states (empty, loading,
error, success), motion rules and keyboard model. UX is task-first: the most common actions
are one key or one click away, destructive ones are clearly separated, and every screen
answers "what is happening, what needs me, what can I do". Interfaces are reviewed against
real tasks before they are built.

## 16. Instructions, not essays

All guidance — docs, playbooks, UI hints, generated records — is short and imperative:
when to do what, and the exact command. No filler, no restating the obvious, no marketing
tone. A playbook entry fits on a few lines; if it needs more, it is split. Generated text
(task records, summaries) is checked by the same rules before it is saved.
