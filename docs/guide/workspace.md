# Workspace guide

Needs tmux 3.2 or newer. Check with `cairn doctor workspace`.

## Quickstart (1 minute)

```
cairn project add api --root ~/code/api --template dev
cairn up
cairn open api
```

The root directory must exist. `project add` writes `~/.config/cairn/projects/api.toml`. `up` creates every active
project's session. `open` switches to it (or attaches). Templates: `dev`, `service`, `shell`.

## After a reboot

Run `cairn up`. It rebuilds sessions from config, never replays old processes, never kills anything.

To do it at every login: `cairn tmux service install --socket <name>`. Nothing starts until
the next login. Remove with `cairn tmux service uninstall`. Check with `cairn tmux service status`.

## Keep an existing tmux session

- `cairn capture --list` shows live sessions cairn does not manage.
- `cairn capture <session>` prints the config that recreates it. Add `--write` to save it.
- `cairn adopt <session>` manages it in place, without restarting it. Remove the markers with `cairn adopt --release <project>`.

## Change the layout

Edit with `cairn project edit <id>`. Then `cairn plan` to preview and `cairn apply` to apply.
`apply` only adds and fixes managed windows and panes. Hand-made ones are reported, not touched.

## Undo

Every config change is recorded: list with `cairn history`, inspect with `cairn history show <id>`.
`cairn undo` reverts the latest change (or `cairn undo <id>`). It changes config only; run `cairn apply`
afterwards. If a file changed after the event, undo refuses and writes nothing.

## Try it beside your own tmux

Use a separate tmux server for every command:

```
mkdir -p /tmp/demo
cairn project add demo --root /tmp/demo
cairn --socket try up
cairn --socket try open demo
```

Or `export CAIRN_TMUX_SOCKET=try`. Your default server is never touched.
Clean up: `tmux -L try kill-server`, then `cairn project archive demo` and `cairn project rm demo`.
Without the last two, a later `cairn up` on the default server creates `demo` there.

## Check health

`cairn doctor workspace` is read-only. It checks the tmux version, config files, project roots,
drift from config (`cairn apply` fixes it; clashes need `cairn plan` and a manual fix) and the optional service.

## Playbook

| Question | Rule |
|---|---|
| How many sessions? | One session per project. Name it after the project (`session` in the config). |
| When does a window get its own entry? | When it runs a long process (server, watcher, logs) or needs its own layout. Everything else is a pane in an existing window. |
| Which restart policy? | `never` for shells and editors (default). `on-failure` for services (default for `role = "service"`); it also respawns a pane killed by a signal (Ctrl-C, SIGTERM, OOM). `always` only for processes that must never stay dead. |
| What does `apply --prune` kill? | Managed windows and panes that left the config, and only when they are idle shells. Busy panes, hand-made windows and other sessions are kept. It asks first; `--yes` skips the question. |
| A root directory is gone? | `cairn doctor workspace` warns. Fix the path with `cairn project edit <id>`, or `cairn project archive <id>`. |
