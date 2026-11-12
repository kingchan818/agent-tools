# agent-tools

Tools for working with coding agents.

| Tool | What it does |
|---|---|
| [fanout](fanout/) | Fan tasks out to non-Claude coding agents (omp, pi, codex) in `hc` (herdr) terminals and collect their reports, like Claude Code subagents. Each worker shows which Claude thread started it when you type `hc`. |
| [claude-mods](claude-mods/) | Mods for Claude Code's own interface. [artifacts](claude-mods/artifacts/): a pane listing the pages, images and PDFs Claude made. |

```sh
cd fanout && make install        # builds ~/.local/bin/fanout
make install-skill               # teaches Claude Code to use it
```
