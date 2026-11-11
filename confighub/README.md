# confighub — every config in one place

**Status: idea.** Design: `docs/specs/confighub.md` (not written yet).

Makes all configuration visible, validated and versioned, editable from the CLI, terminal
UI and web dashboard, applied live where the tool allows it.

- tmux and Neovim configs (adopted from the user, not replaced).
- AI tools: CLAUDE.md / AGENTS.md / GEMINI.md, settings, Codex `config.toml`, profiles.
- MCP servers, skills, plugins, hooks, subagents: one source of truth rendered into each
  tool native format, with drift detection.
- History and undo for every edit; schema validation before writing.
