---
name: fanout
description: Fan tasks out to non-Claude coding agents (omp, pi, opencode, codex) running in hc terminals, then collect their reports — like Claude Code subagents, but other agents. Use when the user asks to fan out, parallelize work across agents, get a second opinion from another model, or delegate to omp/pi/opencode/codex.
---

# fanout: subagents that are not Claude

`fanout` starts one interactive agent per task in its own `hc` terminal,
hands it the task, waits for its turn to end and returns the Markdown
report each worker wrote. Terminals are labelled with this thread, so the
user sees in `hc` / `hc ls` which thread started which worker.

## Run tasks and get the reports (blocking)

```sh
fanout run "Review src/parser.go for bugs. Read-only." "Review src/lexer.go for bugs. Read-only."
fanout run --each "Write table tests for {}; run them; report coverage." pkg/a pkg/b pkg/c
fanout run -a pi -m openai/gpt-5.5 --thinking high "Propose a design for X"
fanout run -f tasks.md            # tasks separated by lines of ---, or a JSON array
```

Output: one section per worker with its report, then a summary line. The
exit code is 0 only when every worker reported.

Write each task like a brief for a peer: goal, exact scope (files), what to
leave alone, what to report. Workers cannot see this conversation.

## Background (like run_in_background)

Use the Bash tool's own timeout wisely: `fanout run` blocks until all
workers finish (default --timeout 30m). For long work:

```sh
fanout run --bg "..." "..."       # prints the run id and returns
fanout status                     # live state of the latest run
fanout wait                       # block until done, print reports
fanout result [RUN] [N]           # print reports again
```

## Continue a worker (like SendMessage)

```sh
fanout run --keep "..."           # keep terminals alive after they report
fanout send RUN 2 "Also cover the error paths"   # waits, prints the new report
```

## Agent types (like .claude/agents)

`fanout agents` lists them. Built-ins: omp (default), pi, opencode, codex.
Define your own as Markdown in `~/.config/fanout/agents/<name>.md` or
`<project>/.fanout/agents/<name>.md`:

```markdown
---
name: reviewer
description: Read-only reviewer on a fast model
backend: omp
model: deepseek-v4-flash
thinking: medium
---
You review code. Never edit files. Report findings with file:line.
```

Then `fanout run -a reviewer "Review x.go"`.

## Editing in parallel

Add `--worktree` so each worker gets its own git worktree and branch
(`fanout/<run>-<n>`); otherwise workers share the folder and can collide.

## Problems

- `blocked`: the agent wants a person (permission/question). Tell the user;
  they can `fanout attach RUN N`. Do not bypass it. `--yolo` (omp, codex)
  skips approval prompts when the user wants that.
- `no_report`: the turn ended without a report; its screen is printed.
- `timeout`: still working; `fanout wait RUN` resumes waiting.
- `fanout kill RUN` stops a run's workers; `fanout threads` shows every
  live hc terminal grouped by the thread that started it.
