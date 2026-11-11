# agents — AI agent sessions

**Status: idea.** Design: `docs/specs/agents.md` (not written yet).

Task-centric management of AI coding agents (Claude Code, Codex, Antigravity, and any CLI
agent added through config). See the "AI architecture scope" in `docs/program.md`.

- Task = branch + worktree + conversations; review and merge gate at the end.
- Tool adapters described in config, with conformance tests; native commands first.
- Live state from hooks, notifications, jump-to-waiting.
- Names, tags, purpose, lineage; cross-tool search over history (local, redacted).
- Profiles (model, effort, permissions, sandbox, MCP), usage and limits.
- Clean, reversible removal; one lock per conversation.
