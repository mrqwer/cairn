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
