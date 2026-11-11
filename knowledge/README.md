# knowledge — knowledge base and playbooks

**Status: idea.** Design: `docs/specs/knowledge.md` (not written yet).

Knowledge that accumulates as a by-product of work, stays true, and is found by both
people and AI agents.

## Ideas to design

- **Plain Markdown in git.** No proprietary store. Location is configurable:
  - default: a personal root `~/knowledge/` with one folder per project (`knowledge.root`),
    so nothing is written into repositories unless asked (principle 11);
  - per project: `knowledge.location = "repo"` stores that project's notes in the repo
    (`docs/knowledge/` by default, path configurable) to share them through git;
  - any existing vault (e.g. an Obsidian vault) can be set as the root.
- **Few, strict note types:** decision (ADR), runbook, how-to, reference, task record,
  incident. Each type has a template with required fields and a length budget.
- **Task records by default:** `cairn finish` drafts a short record (goal, what changed,
  decisions and why, how to verify, links to branch/PR/commits/conversations) from the diff
  and the conversation; the user edits and approves it. Nothing is saved without approval.
- **Anchors:** notes reference code paths, symbols, commits, tasks and conversations.
  Links are bidirectional: from a file in the editor you see the notes about it; from a
  conversation you see the task record.
- **Staleness detection:** when anchored code changes substantially, the note is flagged
  "check if still true" in the UI and `doctor`.
- **Search:** full-text (BM25) plus link graph, optional local embeddings; one index with
  agent conversations.
- **For agents:** an MCP server (`cairn-knowledge`) lets every AI tool search and read
  project knowledge; context files stay short and point to it.
- **Anti-slop gate:** generated text is checked before saving — length budget, required
  concrete references (files, commits, commands), banned filler phrases — principle 16.
- **Artifacts:** each task has a declared place for outputs worth keeping (reports,
  benchmarks, screenshots); `finish` moves them there and cleans the rest (scratch files,
  temp branches, worktree) with a summary of what was kept and removed.

## Playbooks

Short, imperative rules shipped with cairn and shown in context (UI hints, `cairn help
<topic>`, optionally in agents' context files). Examples of topics:

- one conversation vs a new one vs a fork for the next piece of work;
- when a task deserves its own worktree;
- what to keep as an artifact and where; what to clean at finish;
- when to write a decision record vs a runbook.

Users can edit and add playbooks like any other config.
