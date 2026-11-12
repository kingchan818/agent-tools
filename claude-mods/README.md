# claude-mods

Mods for Claude Code: changes to Claude Code's own interface, written as function hooks (`hooks/register.tsx`, loaded by Claude Code itself).

| Mod | What it does |
|---|---|
| [artifacts](artifacts/) | A pane beside the chat listing the pages, images and PDFs Claude made, to open, reveal, copy or mention. |

Run one from the repository root with `claude --plugin-dir claude-mods/<mod>`.
