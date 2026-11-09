# fanout

Run **non-Claude coding agents** (omp by default, also pi, opencode, codex,
or any interactive agent CLI) as **parallel subagents**, through
[herdr-core's `hc`](https://herdr.dev). It works like Claude Code's Agent
tool: give it tasks, and it starts one worker per task, hands each worker its
brief, waits for every turn to end and returns the reports.

Every worker is a real, persistent `hc` terminal: you can watch it, attach to
it and take over. Each one is labelled with the thread that started it, so
typing `hc` (or `hc ls`) shows which Claude thread each worker belongs to:

```
     NAME          WHERE                                            STATUS
●   t1             ~ / 1                                            idle
●   t9  fo-d72fdd-1 · ◆ Omp fanout agent tool › Review parser.go    working
```

## Install

```sh
make install          # builds ./fanout into ~/.local/bin
fanout install-skill  # teaches Claude Code to use it (~/.claude/skills/fanout)
```

Or take a release (macOS and Linux, arm64 and amd64):

```sh
go install github.com/kingchan818/agent-tools/fanout/cmd/fanout@latest
# or download fanout_<version>_<os>_<arch>.tar.gz from GitHub Releases
```

Needs `hc` (herdr-core) and the agent CLIs you use on `PATH`.

### Releasing

Set `Version` in `internal/cli/cli.go`, commit, then push a prefixed tag:

```sh
git tag fanout/v0.2.0 && git push origin fanout/v0.2.0
```

`.github/workflows/release-fanout.yml` tests, checks that the tag matches
`Version`, builds the four binaries and publishes the GitHub release.

## Use

```sh
fanout run "Review src/parser.go for bugs. Read-only." "Review src/lexer.go. Read-only."
fanout run --each "Write table tests for {} and run them." pkg/a pkg/b pkg/c
fanout run -f tasks.md                  # tasks split by '---' lines, or a JSON array
fanout run -a pi -m openai/gpt-5.5 "…"  # another agent / model
fanout run --worktree "…" "…"           # each worker gets its own git worktree + branch
fanout run --yolo "…"                   # skip the agent's approval prompts (omp, codex)
fanout run -j 3 …                       # at most 3 at a time
```

| Claude Code subagents | fanout |
|---|---|
| Agent tool, `subagent_type` | `fanout run -a TYPE "task"` |
| `.claude/agents/*.md` | `~/.config/fanout/agents/*.md`, `.fanout/agents/*.md` |
| `run_in_background` + notification | `fanout run --bg` → `fanout status` / `fanout wait` (inside hc, the caller gets hc notes) |
| SendMessage to an agent | `fanout run --keep …` then `fanout send RUN N "…"` |
| final report | the Markdown file each worker writes; printed by `run`, `wait`, `result` |

Other commands: `fanout ls [--mine]`, `fanout result [RUN] [N]`,
`fanout attach RUN N`, `fanout kill RUN [N]`, `fanout rm RUN`,
`fanout agents`, `fanout threads`. Run `fanout help` for the full list. RUN
defaults to the latest run. Add `--json` for machine-readable output.

### Agent types

```markdown
---
name: reviewer
description: Read-only reviewer on a fast model
backend: omp            # omp | pi | opencode | codex, or `command: my-agent --flag`
model: deepseek-v4-flash
thinking: medium
yolo: false
args: --no-lsp
---
You review code. Never edit files. Report findings with file:line.
```

Unknown keys (`tools:`, `color:`) are ignored, so Claude Code agent files can
be reused unchanged. Project definitions override user ones, and user ones
override the built-ins.

### Worker states

`done` (report written) · `no_report` (the turn ended without a report; its
screen is saved and printed) · `blocked` (it wants a person: `fanout attach`)
· `error` · `exited` · `timeout` (still running; `fanout wait` resumes) ·
`failed` (could not launch or send). The exit code is 0 only when every
worker is `done`.

## How it works (the plan)

```
fanout run ─┬─ plan: resolve agent type → argv, cwd/worktree, report path, prompt
            ├─ per worker, in parallel (limit -j):
            │    hc run --bg --name fo-RUN-N --label where=◆ thread › task -- omp …
            │    hc wait   → agent booted (idle)   [agents without hc status: first quiet chunk]
            │    hc send   → the brief; retried while hc says "still starting, nothing was typed"
            │    hc wait   → loop until its turn ends (turn > sent turn), or blocked/error/exit
            │    collect   → report file present ⇒ done; otherwise save `hc read` screen
            │    cleanup   → kill + rm the finished terminal (unless --keep)
            └─ print every report, then a summary
```

- **No polling with sleep**: waiting is `hc wait`, which returns when the
  agent's own OSC 7501 status changes. An agent that hc has no status for
  (pi here) is polled every 5s and counts as done when its report appears.
- **The report contract**: each brief ends with a hand-off section telling the
  worker to write its final Markdown report to
  `~/.local/state/fanout/runs/RUN/results/N.md` (omp/codex get `--add-dir` for
  that folder). Long answers come back as files, not scraped screens.
- **State**: `run.json` per run (atomic writes), so `wait`, `status`,
  `result`, `send` and `kill` work from any later process.
- **Thread identity**: `CLAUDE_CODE_SESSION_ID`, plus the session's title from
  its transcript (`custom-title`, else `ai-title`), else `FANOUT_THREAD`, else
  `HC_TERMINAL_ID`. It goes on hc labels `where`, `fanout.thread` and
  `claude.session`.

Layout: `internal/hc` (typed hc client), `internal/agentdef` (agent types),
`internal/task` (task input), `internal/thread` (caller identity),
`internal/store` (runs), `internal/orch` (worker lifecycle), `internal/cli`.

## Proving it works

```sh
make test       # unit tests, -race
make cover      # 98.9% of statements overall (the gaps are OS failure paths such as crypto/rand or temp-file errors)
make mutation   # gremlins with every mutator enabled; fails if a mutant survives
make e2e        # real hc + omp: parallel run, bg + follow-up, worktree edit
```

Unit tests replace the `hc` process with fakes: a scripted fake for the
lifecycle engine (every startup, send-retry, turn, timeout and cleanup branch),
and a simulated omp for the CLI (it boots idle, works on what it is sent, and
writes the requested report). Mutation testing then checks that those tests
actually catch broken code.
