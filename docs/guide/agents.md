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

### Review and finish

```
cairn task review fix-login                      # the task's diff; status becomes review
cairn task review fix-login --viewer "delta --side-by-side"
cairn task finish fix-login --merge              # merge into the base locally (not undoable)
cairn task finish fix-login --pr                 # push + pull request via gh/glab (not undoable)
cairn task finish fix-login --abandon            # drop it
```

- The viewer is `tasks.diff_viewer` (default `git diff`). It is split on spaces and run without a
  shell. Arguments may contain `{base}`, `{branch}`, `{range}`, `{repo}`, `{worktree}`, replaced
  inside the argument. Without placeholders, git gets `<base>...<branch>` appended and any other
  viewer reads the diff on its input.
- `finish` needs exactly one mode and prints every step first; steps marked "not undoable"
  (merge, push, pull request) need `--yes` or a yes on a terminal.
  - `--merge` merges into the base branch where it is checked out (a fast-forward when possible).
    It refuses uncommitted changes there and ignored files the merge would overwrite; a conflict
    or an interrupted merge is rolled back and nothing changes. Then the worktree and the merged
    branch are removed. A base in the middle of a rebase or bisect blocks the merge.
  - The two merge paths differ: where the base is checked out, `git merge` runs there and follows
    your git config (`merge.ff`, hooks such as `pre-merge-commit`, `commit.gpgSign`). A base that
    is not checked out anywhere only moves as a ref: a fast-forward, or a merge commit made with
    `git merge-tree` and `git commit-tree`, which runs no hooks and is never signed. Check out the
    base first if you need hooks or signing.
  - `--pr` needs `tasks.pr_tool = "gh"` or `"glab"` (otherwise it stops and prints the commands to
    run yourself). It pushes to `tasks.pr_remote` (default `origin`) and opens the pull request in
    that remote's repository (`--repo`, when its URL names one), then removes the worktree; the
    branch stays. The tool is stopped after 2 minutes; a pull request that already exists for
    the branch counts as opened and its address is shown.
  - `--abandon` removes the worktree, and the branch when it has no commits beyond its base. A
    branch whose commits are all pushed is kept.
- Nothing that holds work is removed silently: uncommitted, changed or ignored files in the
  worktree and commits that exist nowhere else make `finish` refuse and list them. `--force --yes`
  lets exactly that list go and prints it as lost; work that appears after the list was shown is
  kept. A `--here` task's main checkout is only switched (back to what it had, or to the base
  after a merge), never cleaned: its uncommitted changes block the finish even with `--force`.
- The task's conversations are archived and its window removed; an agent still running in the
  task blocks the finish. `cairn undo` of a finish restores only the task's status.
- `review` never changes a task that was finished while the viewer was open.
- Once a finish has started its steps, Ctrl-C does not stop them (git is stopped only by a
  10-minute timeout per step, cleanly). A finish cut off anyway (a crash) is reported by
  `task ls` and `task recover`; run the same `finish` again to complete it: a merge already done
  is recognised, and what is left is removed under the same rules. A finished task whose worktree
  was kept is listed by `task recover`; `cairn task finish <task> --abandon` removes it then
  (the status and the branch stay). A branch deleted by hand can only be closed with `--abandon`.

### Finish record

After a merge or pull request, `task finish` prints a draft record (Goal, Changed, Decision,
Verify, Links) and saves it to `<State>/agents/drafts/<task>.md` (owner only). Changed lists the
real files of the diff and Links the branch and commit range; what cannot be known stays `TODO`.
Fill it in with `cairn task record <task> --edit` (or `--text-file FILE`, `-` for standard input).
The record is written to `<knowledge.root>/<project>/<slug>.md` (default `<State>/records`) only
when the quality gate passes: at most 120 words, at least 2 distinct concrete references (paths,
7-hex commits, `go test`, `make <target>`, `pnpm`), no `TODO`, none of the filler phrases (robust,
seamless, leverage, cutting-edge, state-of-the-art, best practices, plus `tasks.gate_banned`).
Each check is printed as pass or fail. Run `task record` again to redo it; the write is in
history, so `cairn undo` restores the previous record. A file at that path that is not the
record of the same task is never overwritten. The draft is private (0600); the record is a note meant to be read by other tools and people (0644). `knowledge.root` may not be, or contain, your home, state or config directory, nor lie inside `~/.ssh`, `~/.gnupg`, `~/.aws` or `~/.kube`. `task record --edit` does not hold the task lock while the editor is open.

## Concurrency limit

At most `agents.max_running` (default 4, 0 = no limit) tools run at once: conversations in
`starting`, `working` or `waiting` whose pane runs the tool. A further start is `queued` and its
pane shows its position; when a running agent goes idle, finishes or stops, the oldest waiting one
starts. `cairn agents archive`/`rm` take a waiting pane out of the queue.

## Checking the installed tools

`cairn agents doctor [--tool <name>] [--json]` checks each tool against its adapter without starting a
conversation. It runs only the version check, the help commands listed in `probe_flags` (each must
contain `--help`, `-h` or `help`) and the `list` command, each with a 5 s limit in an empty temp dir.
It reports:

- the binary and its version (a tool that is not installed is `skip`, not a failure);
- that every declared capability has its command;
- each flag and subcommand the commands use that is missing from the help of that version (`warn`).
  The hooks flag must be in the top-level help, because cairn passes it before any subcommand;
- that the `list` output parses as JSON (empty output is accepted). `list` is not run when its
  subcommand is missing from the help;
- that the generated hook file (or `-c` override) is valid, in a temp dir that is removed;
- what it cannot check without a running conversation (`hooks.delivery`: whether the tool merges the
  settings file and which events fire) as `skip`.

The exit code is 1 only on a `fail`. `cairn doctor agents` shows the same findings, and `cairn doctor`
includes them.
