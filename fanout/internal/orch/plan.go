package orch

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kingchan818/agent-tools/fanout/internal/agentdef"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
	"github.com/kingchan818/agent-tools/fanout/internal/task"
)

// Defaults apply to every task that does not override them.
type Defaults struct {
	Agent    string
	Model    string
	Thinking string
	Yolo     bool
	Cwd      string
	Worktree bool // give each worker its own git worktree
}

// GitFunc runs git in dir and returns its combined output.
type GitFunc func(ctx context.Context, dir string, args ...string) (string, error)

// ExecGit runs the real git binary.
func ExecGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// WorkerName is the hc terminal name of worker i in run id.
func WorkerName(runID string, index int) string {
	return "fo-" + runID + "-" + strconv.Itoa(index)
}

// Plan adds one pending worker per task to run, resolving agent types,
// command lines, folders and prompts. With Defaults.Worktree it also
// creates a git worktree per worker.
func Plan(ctx context.Context, run *store.Run, specs []task.Spec, reg *agentdef.Registry, d Defaults, git GitFunc) error {
	specs, err := task.Validate(specs)
	if err != nil {
		return err
	}
	if d.Agent == "" {
		d.Agent = "omp"
	}
	type planned struct {
		def  agentdef.Definition
		argv []string
	}
	plans := make([]planned, len(specs))
	// Resolve everything first so a bad task fails before anything starts.
	for i, s := range specs {
		def, err := reg.Get(firstNonEmpty(s.Agent, d.Agent))
		if err != nil {
			return fmt.Errorf("task %d: %w", i+1, err)
		}
		argv, err := def.Argv(agentdef.Overrides{
			Model:    firstNonEmpty(s.Model, d.Model),
			Thinking: d.Thinking,
			Yolo:     d.Yolo,
			AddDirs:  []string{run.ResultsDir()},
		})
		if err != nil {
			return fmt.Errorf("task %d: %w", i+1, err)
		}
		plans[i] = planned{def, argv}
	}
	total := len(specs)
	for i, s := range specs {
		idx := i + 1
		cwd := firstNonEmpty(s.Cwd, d.Cwd, run.Cwd)
		w := &store.Worker{
			Index:       idx,
			Description: s.Description,
			Task:        s.Prompt,
			Agent:       plans[i].def.Name,
			Backend:     plans[i].def.BackendName(),
			Argv:        plans[i].argv,
			Cwd:         cwd,
			Name:        WorkerName(run.ID, idx),
			State:       store.Pending,
			ResultPath:  filepath.Join(run.ResultsDir(), strconv.Itoa(idx)+".md"),
			ScreenPath:  filepath.Join(run.ResultsDir(), strconv.Itoa(idx)+".screen.txt"),
		}
		if d.Worktree {
			if git == nil {
				git = ExecGit
			}
			w.Branch = "fanout/" + run.ID + "-" + strconv.Itoa(idx)
			w.Worktree = filepath.Join(run.Dir, "wt", strconv.Itoa(idx))
			if _, err := git(ctx, cwd, "worktree", "add", "-b", w.Branch, w.Worktree, "HEAD"); err != nil {
				return fmt.Errorf("task %d: worktree: %w", idx, err)
			}
			w.Cwd = w.Worktree
		}
		w.Prompt = BuildPrompt(plans[i].def.Instructions, w, idx, total, run.Thread.Label())
		run.Workers = append(run.Workers, w)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// BuildPrompt is the first message a worker receives: the agent type's
// instructions, the task, and how to hand back its result.
func BuildPrompt(instructions string, w *store.Worker, index, total int, threadLabel string) string {
	var b strings.Builder
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString("## Task\n\n")
	b.WriteString(strings.TrimSpace(w.Task))
	b.WriteString("\n\n## Hand-off\n\n")
	fmt.Fprintf(&b, "You are worker %d of %d in a parallel fan-out started from %q. ", index, total, threadLabel)
	b.WriteString("Work only on this task. Do not start other agents or subagents, and do not ask for confirmation.\n")
	if w.Worktree != "" {
		fmt.Fprintf(&b, "You are in your own git worktree at %s on branch %s; make and commit your changes there.\n", w.Worktree, w.Branch)
	}
	fmt.Fprintf(&b, "When you are finished, or stuck, write your final report as Markdown to:\n%s\n", w.ResultPath)
	b.WriteString("Put the actual answer in that file: findings, changes with file paths, test results, open questions. Write it in one go with your file-writing tool, then end your turn.")
	return b.String()
}

// FollowUp wraps a message sent to a worker that already reported.
func FollowUp(message string, w *store.Worker) string {
	return strings.TrimSpace(message) + "\n\n(When you are done, write your complete updated report to " + w.ResultPath + ", replacing the old one, then end your turn.)"
}
