# Agents guide

## Conversations in panes

```
cairn agents new claude --project api --name "fix login"
cairn agents ls
cairn agents resume "fix login"
```

`new` adds a pane to the project's `agents` window. The tool starts the first time the session is
attached (or at once with `--now`). After a reboot `cairn up` recreates the pane; the tool resumes on
the first attach.

## Live state

States: `starting → working ⇄ waiting → idle → done | failed | stopped`. `waiting` means the tool needs
you (a permission prompt, or input).

- **From hooks.** At launch cairn passes its hooks to that one process; your own tool settings are
  never read or changed.
  - claude: a settings file `~/.local/state/cairn/agents/hooks/<id>.json` (0600) given with `--settings`.
  - codex: `-c notify=[...]`. This replaces a `notify` program from your codex config for that process.
  - The hooks run `cairn event --conversation <id> --kind <kind>`. It appends to
    `~/.local/state/cairn/agents/events/<id>.jsonl` and updates the record. It always exits 0 and prints
    nothing to stdout, so a hook never breaks the tool.
- **From observation.** Tools without hooks (agy), or with hooks that never fire, get a lower-confidence
  state (`state_source = observed`). It comes from the pane: output means working, no output for 2
  minutes means idle, and the tool's exit means stopped or failed.

Which events map to which kind is adapter data (`[hooks]` in `tools/<name>.toml`). Besides the states, two
kinds exist: `ready` (the tool is up and waits for input; it only moves `starting` to `idle`, so a restart
signal in the middle of a turn changes nothing) and `activity` (only the time moves). When a tool's hooks
never report `working` (codex reports only finished turns), new output in the pane moves it from `idle`
back to `working`.

## Status line

```
cairn status            # projects, then "agents: 2 waiting · 3 working" and who waits
cairn status --json     # adds "agents": {"counts": {...}, "waiting": [...]}
cairn status --tmux     # just "2 waiting · 3 working", empty when nobody waits or works
```

`--tmux` reads cairn's records only and never calls tmux, so it is fast enough for the status line:

```
set -g status-right '#(cairn status --tmux)'
```

A conversation whose tool is not running (after a crash or reboot) is counted as stopped.

Not in this release: desktop notifications, a badge pushed on transitions, "jump to next waiting
agent". Use `cairn status --tmux` for now.

## Tasks

```
cairn task new "Fix login" --worktree     # branch task/fix-login + worktree + window with an agent
cairn task new "Quick fix" --here         # branch in the main checkout, no worktree
cairn task ls [--project p] [--status open] [--json]
cairn task open fix-login                 # jump to the task's window (re-creating its pane if needed)
cairn agents new codex --task fix-login   # a second agent on the same task, in the same window
```

- Without `--worktree`/`--here`, `tasks.default_isolation` decides (`ask` asks on a terminal and
  fails on a pipe). `tasks.branch_prefix` (default `task`) and `tasks.worktree_root` (default
  `<state>/worktrees`, as `<root>/<project>/<slug>`) are configurable.
- `.worktreeinclude` in the repo lists files to copy into a new worktree (one path or glob per
  line, `#` comments), e.g. `.env`. Absolute paths, `..`, `.git` and symlinks out of the repo are
  refused; file modes are kept.
- A `.cairn.toml` in the repo may set `[task] setup = ["make", "deps"]` (an argv, not a shell
  line). It is shown with a hash of the file and runs only once approved: on a terminal, or with
  `--approve-setup=<hash>` (at least 8 digits of the hash shown). The approval is remembered per
  project until the file changes. Only the file is hashed: a script it runs (`./setup.sh`) can
  change without a new question, so review what it calls.
- A title without Latin letters or digits gets a slug `task-<hash>`; the window is named by the
  title.
- If `task new` fails before the task is recorded, it removes what it made (the worktree only when
  nothing in it is work, the branch only without new commits) and lists what remains; once the
  task is recorded, a later error (opening the window) leaves it for `cairn task open`. A crash in
  the middle is reported by `task ls`/`task open` and cleaned up by `cairn task recover` (or the
  next `task new`).
- `cairn undo` of a task removes its window binding and task file, archives its conversations,
  then removes its worktree and branch under the same rules. Ignored files (a local database,
  notes) count as work: the worktree is kept and they are listed; `cairn undo --force <id>` lets
  them go and lists them as lost. A worktree an agent still runs in is kept.

## Concurrency limit

At most `agents.max_running` (default 4, 0 = no limit) tools run at once: conversations in
`starting`, `working` or `waiting` whose pane runs the tool. A further start is `queued` and its
pane shows its position; when a running agent goes idle, finishes or stops, the oldest waiting one
starts. `cairn agents archive`/`rm` take a waiting pane out of the queue.
