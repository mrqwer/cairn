# Spec: engine + agents — workspaces and AI agent sessions

Status: **design** · Module dirs: `engine/`, `agents/` · Date: 2026-11-11

## 1. Intent

A developer works across many projects at once, each with an editor, shells, services and
several AI coding agents. Today that structure lives only in running processes: when the
machine dies, the structure dies, and getting it back depends on snapshot-and-replay tools
that guess which processes to re-run. Agent conversations multiply without names or
organisation until finding the right one means scrolling a list of "claude 10-05 10:01".

`engine` makes the workspace a **declared, versioned structure** that is applied to tmux
continuously and can be edited live. `agents` makes AI work **task-centric**: every piece of
agent work has a task, a place (branch + worktree), named conversations, a live state, and
a clean end (review, merge or abandon) — searchable and resumable across every tool.

## 2. Goals

1. After any boot — clean, crash, dead battery — one reconcile brings back every active
   project exactly as declared, with editors and agents resumed by their own mechanisms.
2. Any change to the structure (add/remove/rename a project, window, pane, agent) is
   previewed, applied live without restarting anything else, recorded, and undoable.
3. Manual changes made directly in tmux are never lost: they are detected as drift and can
   be captured into the declared structure with one action.
4. Every agent conversation belongs to a project (and usually a task), has a name, and can
   be found by text, tool, project, tag or date, then resumed in the right place.
5. The user always knows which agents are working, which are waiting for them, and which
   are done — without looking at each pane.
6. Supporting a new CLI agent is a config file, not code.
7. Works for anyone: no paths, names or tools assumed (principles 7, 11–13).

## 3. Non-goals and anti-goals

- Non-goal: replacing tmux, Neovim or the AI CLIs. cairn orchestrates them.
- Non-goal: running agents in the cloud; local CLIs only (cloud features of a tool may be
  launched through its adapter).
- Non-goal (this spec): the UIs (`ui` spec) and editing tool configs/MCP (`confighub` spec).
  This spec defines the engine API they use.
- Anti-goal: replaying commands by guessing from process names (D1).
- Anti-goal: parsing a tool's private files when it has a command for the same thing (D4).
- Anti-goal: killing a pane that runs something other than an idle shell without the user
  confirming it.
- Anti-goal: touching tmux sessions cairn does not manage, unless the user adopts them.
- Anti-goal: auto-merging agent work.

## 3a. How it feels to use (the whole user model)

Everything in the rest of this spec exists to make these few actions reliable. A developer
needs nothing else to start (principle 14):

| You want to | You do | What happens |
|---|---|---|
| Start using cairn | `cairn init` | detects tmux, editor, AI tools and repos; adopts current sessions |
| See what matters now | `cairn` (or the popup key) | agents waiting for you, running work, recent projects |
| Open a project | `cairn open <name>` / pick in UI | attach, creating it if missing |
| Give an agent a job | `cairn new "fix login bug"` | task branch + worktree + agent, named, in its own window |
| Find an old conversation | `cairn find <words>` | search across all tools; Enter resumes it in place |
| Wrap up | `cairn finish` | review the diff, then merge, open a PR or abandon |
| Change your mind | `cairn undo` | the last structural change is reverted |

Three nouns: **project**, **task**, **agent**. Layouts, templates, policies, profiles and
adapters exist, but defaults cover them until the user wants to change something.

## 4. Concepts

```
Workspace (one per user; layered config)
 └─ Project            id, name, root, repos[], tags, state: active | paused | archived
     ├─ Session         one tmux session per project (named from project; renamable)
     │   └─ Window      name, layout (split tree), panes[]
     │       └─ Pane    role, cwd, command spec, env, restart policy, start policy
     └─ Task           id, title, branch, base, worktree, state, conversations[]
         └─ Conversation  tool, native id, name, tags, purpose, profile, live state, lineage
```

- **Pane roles:** `shell`, `editor`, `agent` (bound to a conversation), `service` (a
  long-running command such as `docker compose logs -f`), `command` (any program).
- **Templates:** reusable window/pane layouts (`dev`: editor 60% + shell 40%; `agents`: one
  pane per conversation) referenced by projects; user-defined templates live in config.
- **Identity:** every object has a stable ULID; names are display labels and may change.
  In tmux, managed objects carry the id in user options (`@cairn_id`) so matching never
  depends on names.

## 5. Storage

| What | Where | Format | Why |
|---|---|---|---|
| Desired state | `$XDG_CONFIG_HOME/cairn/` (`workspace.toml`, `projects/<id>.toml`, `templates/`, `tools/`) | TOML | human-editable, diffable, can be a git repo |
| Change history | `$XDG_STATE_HOME/cairn/history.db` | SQLite (WAL) | append-only events with diff and inverse |
| Runtime state | `$XDG_STATE_HOME/cairn/state.db` | SQLite (WAL) | conversation states, locks, last observations |
| Search index | `$XDG_DATA_HOME/cairn/agents/index.db` | SQLite FTS5, mode 0600 | cross-tool transcript search |
| Worktrees | `tasks.worktree_root`, default `~/worktrees/<project>/<task>` | git worktree | outside repos, visible, never committed |
| Socket, tokens | `$XDG_RUNTIME_DIR/cairn/` | — | API access |

- All writes are atomic (temp file + rename, SQLite transactions). Editing TOML by hand is
  supported: the daemon watches the config directory, validates the change, records it in
  history as a `manual-edit` event, and reconciles. An invalid manual edit is rejected
  (the running state stays as it was) and reported, never half-applied.
- Undo restores the desired state recorded before a given event and reconciles.

## 6. Architecture

```
            ┌──────────── cairnd (systemd --user, socket-activated, crash-only) ───────────┐
CLI ───┐    │  config loader + schema validation     history (events, undo)                │
TUI ───┼──► │  API: JSON-RPC 2.0 over unix socket;    reconciler ◄── tmux observer (-C)    │
web ───┘    │       HTTP + WebSocket on 127.0.0.1     agents: adapters, hooks receiver,     │
            │       (token from $XDG_RUNTIME_DIR)             state machine, notifier,      │
            │  event bus (live updates to UIs)                task/worktree manager, index  │
            └───────────────────────────────────────────────────────────────────────────────┘
tmux server: separate systemd --user service; cairnd restarts never touch it.
```

- **Crash-only daemon:** no state only in memory. On start it loads config and state,
  observes tmux, and reconciles. Kill it at any point and restart it; the result converges.
- **Single writer:** all mutations go through the daemon (or through the CLI's embedded
  engine holding the same file lock when the daemon is not running).
- **Every operation has a CLI form with `--json` output.** The TUI and web use the same
  API; nothing is UI-only (D5).
- **Language:** Go — single static binary (`cairn` CLI + `cairnd`), strong concurrency, good
  tmux/process/SQLite libraries. (Builder may revisit only with a written reason.)

## 7. Reconciler

**Observe:** a persistent tmux control-mode client (`tmux -C`) receives notifications
(window/pane/session added, closed, renamed, layout changed); a full `list-*` snapshot is
taken on start and periodically as a safety net (level-triggered).

**Diff:** desired vs observed objects matched by `@cairn_id`. Operations: create, rename,
move, resize/layout, set options, start/restart command, kill. Only managed objects are
touched.

**Layouts:** a window's layout is a split tree (`{ split = "h", ratio = 0.6, a = pane, b = … }`).
The reconciler creates panes from the tree and applies an exact tmux layout string computed
for the current client size, so splits never flip orientation.

**Commands:** a pane's command is rebuilt from its spec at start — never replayed from a
snapshot. Editors start with their own session restore (e.g. `nvim` + persistence plugin);
agent panes start through their adapter's resume command.

**Start policy (per pane):** `eager` (start at reconcile), `lazy` (create pane, start the
command when the session is first attached), `manual`. Default: `eager` for shells and
editors, `lazy` for agents and services after boot (fast boot, low resource use).

**Restart policy:** `never` (default), `on-failure`, `always` with backoff, like systemd.

**Drift policy (per project):** `report` (default) shows the difference and offers
*capture* (adopt the change into desired state) or *revert*; `enforce` reverts
automatically; `ignore`.

**Safety:** before killing a pane, the reconciler checks its foreground process; anything
other than an idle shell requires confirmation (or `--force`). Removing a project archives
it first (state `archived`, session closed, config kept) — deleting the config is a second,
explicit step.

**Plan/apply:** `cairn plan` shows the operation list; `cairn apply` executes it; `cairn
undo [event]`, `cairn history`. Every apply is one history event.

**Capture:** `cairn capture <project|session>` reads the live session (windows, split tree,
cwd, foreground commands classified into roles) and proposes a desired-state diff. Unmanaged
sessions can be adopted as new projects the same way.

## 8. tmux integration (non-invasive)

- cairn never edits `tmux.conf`. `cairn init` offers to add one line:
  `source-file -q ~/.config/cairn/tmux/cairn.conf` (generated; key bindings for the popup UI,
  jump-to-waiting, status segment — all configurable or disabled).
- The tmux server runs as a `systemd --user` unit (`cairn-tmux.service`) so logouts and
  terminal/GNOME crashes do not kill it. Users with their own tmux server setup can opt out.
- Status segment: `#(cairn status --tmux)` reads a small cached file the daemon updates on
  every state change (target < 10 ms, no daemon round-trip).

## 9. Agents

### 9.1 Tool adapters (config, not code)

Each tool is a file in `tools/` (built-ins ship for `claude`, `codex`, `agy`; users add more):

```toml
# tools/claude.toml (abridged)
name = "claude"
binary = "claude"
version_check = ["claude", "--version"]

[capabilities]
name_at_start = true          # --name
preassign_id = true           # --session-id <uuid>
resume_by_id = true
resume_by_name = true
fork = true
list_json = true              # claude agents --json
hooks = "settings-file"       # hooks injected via --settings <file> at launch
delete = "native"

[commands]
start   = ["claude", "--session-id", "{id}", "--name", "{name}", "{profile_args...}"]
resume  = ["claude", "--resume", "{id}", "{profile_args...}"]
fork    = ["claude", "--resume", "{id}", "--fork-session", "--session-id", "{new_id}"]
list    = ["claude", "agents", "--json", "--all"]

[profile_flags]               # how generic profile settings map to this tool
model = ["--model", "{value}"]
effort = ["--effort", "{value}"]
permission_mode = ["--permission-mode", "{value}"]

[env]
unset = ["ANTHROPIC_API_KEY"]  # example: force subscription auth (user-configurable)

[history]                      # only for search indexing; tolerant, versioned parser
locator = "~/.claude/projects/*/{native_id}.jsonl"
parser = "claude-jsonl@1"
```

- **Identity strategy** per tool, in order of preference: pre-assign the id
  (`--session-id`), or name the conversation, or discover the id after launch (adapter
  declares how: directory diff, `list` command, or a tool state file), recorded at once.
  Tools without names (e.g. `agy`) get their names from cairn.
- **Gaps are filled by cairn:** no names → cairn names; no list → cairn's own record; no
  hooks → state from pane observation (fallback, marked as lower confidence); no fork →
  feature hidden for that tool.
- **Conformance suite:** each adapter has tests run against fake tools in CI and, on the
  user's machine, `cairn agents doctor` runs a non-destructive probe against the installed
  version (flags exist, list parses) and reports incompatibilities with the version.

### 9.2 Tasks and worktrees

- `cairn task new "<title>" [--project p] [--tool claude] [--from <ref>] [--here]`:
  creates branch `<prefix>/<slug>` (prefix configurable, default `task`), a worktree,
  copies files listed in `.worktreeinclude` (e.g. `.env`), runs the project's optional
  `setup` command (shown and approved once per project), opens a window with the agent.
- **The user chooses isolation per task:** `--worktree` or `--here` (main checkout). With
  neither flag, `tasks.default_isolation` decides (`here` | `worktree` | `ask`; default
  `ask`, which in the UI is a one-key toggle remembering the last choice).
- Several conversations per task are normal (a planner and an implementer; Claude and
  Codex on the same branch); each has its own pane.
- **Review gate:** `cairn task review` shows the diff (configurable viewer: `git diff`,
  `delta`, or the editor's diff view). `cairn task finish --merge | --pr | --abandon`:
  merge into base locally, push and open a PR via `gh`/`glab` if configured, or abandon.
  Never automatic.
- **Concurrency limit:** maximum running agents (default 4, configurable); further starts
  queue with a visible position.

### 9.3 Conversation model

Fields: `id` (ULID), `tool`, `native_id`, `name`, `purpose`, `tags[]`, `project`,
`task`, `cwd`, `worktree`, `branch`, `profile`, `launch_argv`, `state`, `state_source`
(`hook` | `native` | `observed`), `waiting_reason`, `last_activity_at`, `pane` (tmux
target), `parent_id` / `forked_from` / `handoff_from`, `usage` (tokens, cost estimate,
when available), `transcript_ref` (+ parser version), `summary`, `pinned`,
`created_at`, `archived_at`, `adapter_version`.

**States:** `starting` → `working` ⇄ `waiting` (needs input / permission) → `idle` →
`done` | `failed` | `stopped` | `archived`. Transitions come from hooks (Claude hook
events; Codex hooks/notify), from the tool's own status listing, or from observation.

### 9.4 Live state and notifications

- At launch, cairn passes hook configuration **per process** (e.g. Claude `--settings
  <cairn-file>`, Codex `-c` overrides) so the user's own tool settings are never edited.
  Hooks call `cairn event --conversation <id> --kind <kind>`, which posts to the daemon.
- Notifications on transitions only (debounced): desktop notification + tmux status badge
  (`2 waiting · 3 working`). Key binding "jump to next waiting agent". Quiet hours and
  per-project mute in config.

### 9.5 Reboot behaviour

On reconcile after boot, every non-archived conversation bound to a pane is restored with
its adapter's `resume` command, under the pane's start policy (default `lazy`: the pane
exists and shows the conversation name; the tool starts when the session is first attached, see R9). A per-conversation
lock (in `state.db`, keyed by `tool + native_id`) prevents the same conversation from being
resumed in two panes.

### 9.6 Finding conversations

- `cairn agents ls/find` and the UIs: filter by project, task, tool, state, tag, date;
  full-text query over transcripts.
- Index: incremental, background, low priority; adapters extract plain text with their
  versioned parser; a parse failure skips the item and is reported, never crashes.
- Redaction before indexing: token/key patterns, `.env`-style assignments, and adapter-
  declared secret fields. Index is local, 0600, purgeable per project.
- `cairn agents name` suggests a name and purpose from the first prompt (local heuristic;
  optional model-based summary through a configured tool, off by default).
- Import: `cairn agents import` adopts existing conversations of every configured tool
  (they appear unnamed under their project, ready to be named or archived).

### 9.7 Profiles and safety

- Named profiles (`[profiles.careful]`, `[profiles.fast]`…) with generic keys (model,
  effort, permission mode, sandbox, MCP set) mapped per tool by the adapter; per project
  and per conversation defaults.
- Sandbox on by default where the tool supports it. Flags that skip permission checks are
  allowed only per project with an explicit `i_understand_the_risk = true` and shown with a
  warning badge everywhere.
- Context files (CLAUDE.md, AGENTS.md, GEMINI.md, rules) are shown per project with drift
  warnings; editing them belongs to `confighub`.

### 9.8 Usage and limits

Per tool/project/task usage from native sources when available and from `ccusage`
(optional dependency) otherwise; dollar figures labelled as estimates. Quota/limit status
shown where the tool exposes it.

### 9.9 Removal

`archive` (hidden, resumable, undoable) → `delete` (adapter's native delete when
available; cairn record removed; transcript deletion only if the user confirms) → task
`finish`/`abandon` removes the worktree only when it has no uncommitted changes and no
unpushed commits, unless `--force` with an explicit summary of what would be lost.

## 10. CLI surface (initial)

```
cairn init | doctor | plan | apply | undo [event] | history | capture <target>
cairn project add|edit|pause|resume|archive|rm|ls
cairn open <project>                 # attach/switch, creating if needed
cairn task new|ls|open|review|finish
cairn agents new|resume|fork|name|tag|archive|rm|ls|find|import|doctor
cairn status [--tmux|--json]
cairn config explain <key> | validate | edit
```

## 11. Quality bar

- **Property tests** for the reconciler against a real tmux on an isolated socket
  (`tmux -L cairn-test`): random desired states → apply → observed equals desired; applying
  twice is a no-op; killing the process at random points mid-apply then re-applying
  converges.
- **Adapter conformance tests** with fake CLIs in CI; optional live tests against installed
  tools.
- **Performance targets:** reconcile 30 projects / 150 panes from empty tmux < 3 s;
  `cairn status --tmux` < 10 ms; daemon idle CPU ≈ 0.
- **Observability:** structured logs to journald, `cairn doctor` covers engine and agents.

## 12. Migration of the reference setup (summary; details in the profile)

Existing tmux sessions (~28), the `ai` registry (82 named conversations), `projectctl.py`
registry, the AI launch bindings and the usage/sessions popups are mapped into a profile:
projects from the registry, conversations imported and matched by native id, current
layouts captured with `cairn capture`. The old system (resurrect, continuum, scripts) stays
installed but disabled until the new one has run reliably for an agreed period.

## 13. Owner decisions

- Worktree location is configurable (`tasks.worktree_root`); default `~/worktrees`.
- Worktree or main checkout is chosen per task; the default is configurable.

## 14. Resolved design points (from review)

**R1. Milestones.** v1 is delivered in this order; each milestone is usable on its own:
M1 desired state + reconciler + history/undo + CLI + tmux service; M2 agent adapters,
conversation records, naming, resume after reboot, hook-based state, notifications;
M3 tasks + worktrees + review gate; M4 search index + `agents import`; M5 profiles,
usage. Migrating the reference setup (§12) is project 6, not this one; generic
`agents import` (from tool histories) is M4, importers for the author's own registries
belong to the migration.

**R2. Desired state vs records.**
- *Desired state* (TOML, history, undo, reconciled): projects, sessions, windows, panes
  (including which conversation an agent pane shows), templates, tool adapters, profiles,
  **tasks**.
- *Records* (`state.db`): conversations (native id, name, tags, purpose, lineage, state,
  archived), observations, locks, index metadata. Changes to curated fields (rename, tag,
  archive) are still written to history and undoable; `agents import` creates records only
  and never touches TOML.
- `cairn new` writes a task + window to desired state (one event) and creates the
  conversation record.

**R3. Undo semantics.** History is never rewritten. `cairn undo` reverts the latest event by
applying its inverse; undoing an older event applies its inverse only if the objects it
touched were not changed since, otherwise cairn offers `cairn history restore <event>`
(desired state as it was at that event, recorded as a new event). Every operation is
classified up front:
- *reversible*: structure, names, tags, archive;
- *compensatable*: created worktree (removed if clean), created branch (deleted if it has
  no new commits);
- *irreversible*: merge, push, PR creation, native conversation delete, transcript
  deletion — confirmed before running and shown as "not undoable".

**R4. Shared foundation.** Engine is built on the foundation delivered with `host`
(layered config, schema validation, doctor framework, managed-file inventory, event
library). There is one event schema and one library; `host` writes machine events from root
to `/var/lib/cairn/host/events.jsonl`, the engine writes user events to `history.db`.
Program dependency updated accordingly.

**R5. tmux server.** cairn uses the **default socket** so plain `tmux attach` keeps working.
`cairn-tmux.service` starts the server only if none is running; a server started from a
terminal before install is used as-is and the service takes over at the next login. In
opt-out mode cairn never starts a server itself except to reconcile (`tmux start-server`).
Unmanaged sessions are never touched; `cairn init` *offers* adoption session by session.

**R6. Multi-repo projects.** In v1 a task targets one repository: the project's primary
repo by default, or the one chosen with `--repo` / in the UI. The schema stores
`repos: [{repo, branch, worktree}]` so multi-repo tasks (same slug in several repos,
finished per repo) can be added later without migration.

**R7. Agent pane lifecycle.** Agent commands run through a small wrapper: when the tool
exits, the pane stays and shows the conversation name with `r` resume / `x` close /
`a` archive. The structure does not change by itself; closing or archiving from there
removes the pane binding from desired state (one event). `cairn find` → Enter: if the
conversation is bound to a pane, jump there; otherwise open it in its task's window (new
pane) or, without a task, in a new window of its project — recorded as an event.

**R8. Repo-level `.cairn.toml` and trust.** A repository file may declare only *settings*:
task `setup` command, worktree include list, preferred template names, context-file hints.
It cannot declare panes or commands that start on reconcile. Like `direnv`, a repo file is
ignored until trusted (`cairn trust`, keyed by content hash; any change asks again).

**R9. Lazy start.** Core trigger is the session's first attach/switch, observed in control
mode (`%client-session-changed`) — no user tmux config needed. Starting on *pane focus* is
an optional refinement enabled by the generated `cairn.conf` hooks. (§9.5 means session
attach.)

**R10. Adapters vs private files (refines D4).** Native commands first. Where a tool offers
no command, the *adapter* (never engine code) may use declared, versioned, tolerant parsers
of the tool's files, covered by conformance tests:
- **Claude:** id pre-assigned with `--session-id`; past interactive conversations are
  imported/indexed from transcript files (the native list covers only background ones).
- **Codex:** id discovered after launch as the newest session whose cwd matches and start
  time follows the launch; starts in the same directory are serialized for a few seconds
  so discovery is unambiguous; names kept by cairn.
- **agy:** id discovered from its conversation store / last-conversations cache after
  launch; resume with `--conversation <id>`; if discovery fails, resume falls back to
  `--continue` in that directory and is marked best-effort.

## 15. Builder's calls

Package layout, SQLite schema details, exact JSON-RPC method names, debounce timings, and
the list of observed-state heuristics are left to the builder within the principles.
