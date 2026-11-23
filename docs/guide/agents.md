# Agents guide

## Quickstart

1. Check the tools: `cairn agents doctor` (a tool that is not installed is a skip, not a failure).
2. Optionally define profiles in `~/.config/cairn/agents/profiles.toml` and pick a project default:
   `cairn agents profile default careful --project api`.
3. Start a conversation: `cairn agents new claude --project api --name "fix login" [--profile careful]`.
4. Open it: `cairn open api` (the tool starts on first attach), or later `cairn agents resume "fix login"`.
5. See who needs you: `cairn agents ls`, `cairn status` (`!` marks a conversation with risky flags).
6. Work in a task: `cairn task new "Fix login" --worktree`, then `cairn task review fix-login`.
7. Finish it: `cairn task finish fix-login --merge` (or `--pr`, `--abandon`), then
   `cairn task record fix-login --edit`.
8. Find old work: `cairn agents import`, then `cairn agents find login retry`.
9. See what it cost: `cairn agents usage --by project` (estimates; needs the optional `ccusage`).

Tools are described by adapters (`claude`, `codex`, `agy` built in; your own or overrides in
`~/.config/cairn/tools/<name>.toml`). Records are an append-only file,
`~/.local/state/cairn/agents/conversations.jsonl` (no database); the last line per id wins.

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
conversation. It runs only three kinds of command:

- the version check, and only if it has a `--version`, `-v`, `-V` or `version` argument;
- the help commands listed in `probe_flags` (each must contain `--help`, `-h` or `help`);
- the `list` command, and only when the top-level help was read and lists its subcommand (an indented
  line starting with it, not a word in prose). Otherwise `list` is a `skip` naming the reason.

Each command runs directly (no shell) in an empty temp dir, in its own process group, which is killed
whole after 5 s. Its environment is only `PATH`, `HOME` and `LANG`. Tool output quoted in a finding is
cut short and masked by the same redactor as the search index (see below).

It reports:

- the binary and its version (a tool that is not installed is `skip`, not a failure);
- that every declared capability has its command;
- each flag and subcommand the commands use that is missing from the help of that version (`warn`).
  The hooks flag must be in the top-level help, because cairn passes it before any subcommand;
- that the `list` output parses as JSON (empty output is accepted);
- that the generated hook file (or `-c` override) is valid, in a temp dir that is removed;
- what it cannot check without a running conversation (`hooks.delivery`: whether the tool merges the
  settings file and which events fire) as `skip`.

The exit code is 1 only on a `fail`. `cairn doctor agents` shows the same findings (a skip is marked `–`
and never fails), and `cairn doctor` includes them.

## Finding and importing conversations

```
cairn agents import [--tool claude] [--dry-run]
cairn agents find login retry [--project api --tool claude --tag x --since 7d --archived --json] [--reindex]
cairn agents index purge --project api      # delete its indexed text and keep it out
cairn agents index include --project api    # index it again
```

`import` adopts the conversations a tool already has on disk (found through the adapter's
`history.locator`). It creates records only, never touches project files, and is safe to run again: a
native id already recorded (or removed with `rm`) is not imported again. Imported conversations are
unnamed, in state `stopped`, under the project whose root holds their working directory; `agents name`
names them and `agents resume` opens them with the tool's resume command.

`find` brings the index up to date (only transcripts whose size or time changed are read again), then
shows the conversations whose name or text contains every word (case-insensitive, any script), most
recently active first. Open a hit with `cairn agents resume <id>`. When another `find` is updating the
index, it says so and waits up to 30 s, then searches the index as it stands.

- **Index:** `<State>/agents/index/`, directory 0700, one text file per conversation plus
  `manifest.json` and `excluded.json`, all 0600. Only user and assistant text is indexed: not tool calls,
  tool output, file contents or pasted images. `--reindex` reads every transcript again.
- **Purge:** `index purge` deletes a project's indexed text and records the project as excluded, so
  later updates leave it out until `index include`. The transcripts themselves are the tool's and are not
  touched.
- **Redaction** runs before anything is written: private key blocks, URL credentials (to the last `@`),
  bearer/basic credentials, `curl -u` and `mysql -p` passwords, known token formats (`sk-`, `sk_live_`,
  `ghp_`, `AKIA`, `xoxb-`, `hf_`, `npm_` ...), values of assignments, flags and JSON/YAML members whose
  key names a secret (`API_KEY=...`, `--password ...`, `"token": ...`, escaped JSON, a YAML value on the
  next line), long random-looking strings, and the fields an adapter lists in `history.secret_fields`.
  Key names stay visible; git SHAs, UUIDs, paths, identifiers and prose ("basic setup", `max_tokens=4096`)
  are kept. Every entry records the redaction rules it was written under; when the rules or an adapter's
  `secret_fields` change, it is redacted again on the next `find`.
- **Terminal safety:** escape sequences and other control characters from transcripts, file names and
  records are removed from the index and from everything `find`, `import`, `ls` and `name` print.
- **Tolerant parsing:** parsers are versioned (`claude-jsonl@1`). A malformed line is left out; image data
  is dropped while reading, so a pasted screenshot does not make a line too long; a line still over 4 MiB
  is skipped without being held in memory (its session id, directory and time are still read). A file
  that is not a transcript, a symlink, or over 256 MiB is listed under "skipped" with the reason. A tool
  whose parser cairn does not know (codex for now) is reported once and skipped; a tool without
  `history.locator` (agy) is not indexed. None of these is an error exit.

## Profiles and safety

```toml
# ~/.config/cairn/agents/profiles.toml
[profiles.careful]
effort = "high"
sandbox = "read-only"        # a tool without a sandbox flag reports it and goes on

[profiles.careful.claude]    # values for one tool replace the generic ones
model = "opus"
permission_mode = "plan"

[profiles.careful.codex]
permission_mode = "on-request"
```

- The file was `~/.config/cairn/profiles.toml` before; that path is still read when the new one is
  missing, and `agents doctor` says to move it. (`~/.config/cairn/profiles/<name>.toml` are config
  layers, a different thing.)
- Keys: `model`, `effort`, `permission_mode`, `sandbox`, `mcp_set`. Each adapter maps a key to its
  tool's flags (`[profile_flags]`) and may list the values the tool takes (`[profile_values]`; claude
  `permission_mode`: acceptEdits, auto, bypassPermissions, default, dontAsk, manual, plan; codex
  `permission_mode`: untrusted, on-failure, on-request, never; codex `sandbox`: read-only,
  workspace-write, danger-full-access). Tools take different values, so put tool-specific ones in
  `[profiles.<name>.<tool>]`. A value a tool does not take stops `agents new` and `task new` before
  anything is created. A key a tool cannot express is reported and left out. An unknown key, a
  non-string value or a value starting with `-` keeps that profile from loading (`cairn agents profile
  ls` and `cairn agents doctor` show why).
- The profile of a launch is the conversation's `--profile`, else its task's (`task new --profile`),
  else the project's `default_profile` (`cairn agents profile default <name> --project p`, undoable;
  `--clear` removes it). It is resolved again at each launch; when the task file cannot be read, the
  pane says so, and a launch that would then be risky is refused.
- A sandbox flag is passed only when the profile sets `sandbox`. Otherwise the tool's own config
  decides (codex: `sandbox_mode` in `~/.codex/config.toml`, and its per-project trust). `agents doctor`
  shows that setting and warns when it is `danger-full-access`.
- Flags that skip permission checks or the sandbox are refused unless the project file says so itself.
  They are the adapter's `risky_flags` plus the known ones (any `--dangerously-*`,
  `--allow-dangerously-skip-permissions`, `--permission-mode bypassPermissions`, `--yolo`,
  `--approve-for-me`, `--sandbox`/`-s danger-full-access`, `-c sandbox_mode=danger-full-access`),
  matched in every spelling (`--x=v`, `-xv`, `-x=v`, `-c k=v`, `--config=k=v`, quoted values):

  ```toml
  [agents]
  i_understand_the_risk = true
  ```

  There is no global switch and no command sets it (edit the file: `cairn project edit api`). Removing
  it stops the next launch. `agents ls` marks such conversations with `!`; `status` (and
  `status --tmux`) shows the mark while their tool runs; `agents doctor` lists every project that
  allows them.

## Usage

`cairn agents usage [--by tool|project|task] [--json]` sums tokens and cost over all conversations of
tools whose adapter sets `usage = "ccusage"` (claude). It asks `ccusage` (optional) once per
conversation: `ccusage session --id <id> --json --offline`, no shell, 10 s each and 30 s in all,
at most 1 MiB of output. **These ccusage flags are not verified against a real install.** Only a
session reported with exactly the conversation's id counts; anything else is `unavailable` with a
one-line reason, and a failed or unreadable run is also a warning. Without `ccusage` it says
`unavailable` and exits 0. Every dollar figure is an estimate, not a bill. Quota and limit status is
not shown yet.
