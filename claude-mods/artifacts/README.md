# artifacts

A Claude Code mod: a pane beside the chat that lists the pages, images and PDFs Claude made, so you can open them without hunting for paths.

- **What it lists:** files Claude writes with Write or Edit, and files a Bash command makes (a build step, screenshots in a folder it names), when they are `.html`, `.svg`, `.png`, `.jpg`, `.gif`, `.webp`, `.pdf`, `.mp4`, `.webm` or `.mov`. Markdown counts only under `.scratch/`, `tmp/`, `reports/` or `out/`. Anything under `node_modules`, `.git`, `dist` or `build` is skipped.
- **Two lists:** "This session" (press `s`) and "This project" (press `p`), kept across sessions per working folder.
- **Keys in the pane:** `1`–`9` pick a file (press again to open), `o` open, `r` reveal in Finder, `c` copy the path, `m` mention it in the prompt as `@path`.
- **Command:** `/custom-artifact` opens the pane; `/custom-artifact 2` opens the second file of this session.

The pane opens by itself with the first file of a session, beside the chat when the terminal is at least 144 columns wide.

## Try it from this folder

```sh
claude --plugin-dir claude-mods/artifacts
```

## Check it

```sh
claude plugin validate claude-mods/artifacts
claude plugin test claude-mods/artifacts
```
