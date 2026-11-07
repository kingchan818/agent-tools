package orch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/agentdef"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
	"github.com/kingchan818/agent-tools/fanout/internal/task"
	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

func newRun(t *testing.T) *store.Run {
	t.Helper()
	s := &store.Store{Root: t.TempDir()}
	r, err := s.Create(func() string { return "r1" }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.Cwd = "/run-cwd"
	r.Thread = thread.Info{Kind: "shell", Title: "shell"}
	return r
}

func registry(t *testing.T) *agentdef.Registry {
	r := agentdef.NewRegistry()
	r.Add(agentdef.Definition{Name: "reviewer", Backend: "pi", Model: "def-model", Instructions: "You only review.", Source: "t"})
	r.Add(agentdef.Definition{Name: "custom", Command: []string{"my-agent", "--x"}, Source: "t"})
	return r
}

func TestPlanResolvesWorkers(t *testing.T) {
	run := newRun(t)
	specs := []task.Spec{
		{Prompt: "Review a.go"},
		{Prompt: "Review b.go", Agent: "reviewer", Description: "b review"},
		{Prompt: "Review c.go", Agent: "reviewer", Model: "task-model", Cwd: "/elsewhere"},
		{Prompt: "custom job", Agent: "custom"},
	}
	err := Plan(context.Background(), run, specs, registry(t), Defaults{Model: "m0", Thinking: "low", Cwd: "/default-cwd"}, nil)
	if err == nil {
		t.Fatal("custom command with default model must be rejected")
	}
	if len(run.Workers) != 0 {
		t.Fatal("no worker may be added when any task fails")
	}
	specs = specs[:3]
	if err := Plan(context.Background(), run, specs, registry(t), Defaults{Model: "m0", Thinking: "low", Cwd: "/default-cwd"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(run.Workers) != 3 {
		t.Fatalf("workers %d", len(run.Workers))
	}
	res := run.ResultsDir()
	w1, w2, w3 := run.Workers[0], run.Workers[1], run.Workers[2]
	if w1.Agent != "omp" || w1.Backend != "omp" || w1.Cwd != "/default-cwd" || w1.Name != "fo-r1-1" || w1.Index != 1 || w1.State != store.Pending {
		t.Fatalf("w1 %+v", w1)
	}
	if !reflect.DeepEqual(w1.Argv, []string{"omp", "--model", "m0", "--thinking", "low", "--add-dir", res}) {
		t.Fatalf("w1 argv %q", w1.Argv)
	}
	if w1.ResultPath != filepath.Join(res, "1.md") || w1.ScreenPath != filepath.Join(res, "1.screen.txt") {
		t.Fatalf("paths %s %s", w1.ResultPath, w1.ScreenPath)
	}
	if w1.Description != "Review a.go" || w1.Task != "Review a.go" {
		t.Fatalf("w1 desc %q task %q", w1.Description, w1.Task)
	}
	if w2.Agent != "reviewer" || w2.Backend != "pi" || w2.Description != "b review" {
		t.Fatalf("w2 %+v", w2)
	}
	if !reflect.DeepEqual(w2.Argv, []string{"pi", "--model", "m0", "--thinking", "low"}) {
		t.Fatalf("w2 argv %q", w2.Argv)
	}
	if !strings.HasPrefix(w2.Prompt, "You only review.\n\n## Task\n\nReview b.go") {
		t.Fatalf("w2 prompt %q", w2.Prompt)
	}
	if w3.Cwd != "/elsewhere" || w3.Argv[2] != "task-model" || w3.Name != "fo-r1-3" {
		t.Fatalf("w3 %+v", w3)
	}
	containsAll(t, w3.Prompt, "worker 3 of 3", `"shell"`, filepath.Join(res, "3.md"))
}

func TestPlanDefaultsAndFallbacks(t *testing.T) {
	run := newRun(t)
	if err := Plan(context.Background(), run, []task.Spec{{Prompt: "x"}}, registry(t), Defaults{}, nil); err != nil {
		t.Fatal(err)
	}
	w := run.Workers[0]
	if w.Agent != "omp" || w.Cwd != "/run-cwd" {
		t.Fatalf("%+v", w)
	}
	run2 := newRun(t)
	if err := Plan(context.Background(), run2, []task.Spec{{Prompt: "x"}}, registry(t), Defaults{Agent: "reviewer"}, nil); err != nil {
		t.Fatal(err)
	}
	if run2.Workers[0].Argv[2] != "def-model" {
		t.Fatalf("definition model should apply: %q", run2.Workers[0].Argv)
	}
}

func TestPlanErrors(t *testing.T) {
	cases := []struct {
		specs []task.Spec
		d     Defaults
		msg   string
	}{
		{nil, Defaults{}, "no tasks"},
		{[]task.Spec{{Prompt: "ok"}, {Prompt: "x", Agent: "ghost"}}, Defaults{}, "task 2: unknown agent"},
		{[]task.Spec{{Prompt: "x"}}, Defaults{Agent: "ghost"}, "task 1: unknown agent"},
		{[]task.Spec{{Prompt: "x", Agent: "pi"}}, Defaults{Yolo: true}, "task 1: backend pi has no approval-skipping flag"},
	}
	for _, c := range cases {
		run := newRun(t)
		err := Plan(context.Background(), run, c.specs, registry(t), c.d, nil)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%+v: got %v want %q", c.specs, err, c.msg)
		}
		if len(run.Workers) != 0 {
			t.Errorf("workers added despite error")
		}
	}
}

func TestPlanWorktreeFakeGit(t *testing.T) {
	run := newRun(t)
	var calls [][]string
	git := func(_ context.Context, dir string, args ...string) (string, error) {
		calls = append(calls, append([]string{dir}, args...))
		return "", nil
	}
	specs := []task.Spec{{Prompt: "a"}, {Prompt: "b", Cwd: "/other"}}
	if err := Plan(context.Background(), run, specs, registry(t), Defaults{Worktree: true}, git); err != nil {
		t.Fatal(err)
	}
	w1 := run.Workers[0]
	wantWT := filepath.Join(run.Dir, "wt", "1")
	if w1.Worktree != wantWT || w1.Cwd != wantWT || w1.Branch != "fanout/r1-1" {
		t.Fatalf("w1 %+v", w1)
	}
	want := [][]string{
		{"/run-cwd", "worktree", "add", "-b", "fanout/r1-1", wantWT, "HEAD"},
		{"/other", "worktree", "add", "-b", "fanout/r1-2", filepath.Join(run.Dir, "wt", "2"), "HEAD"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %q", calls)
	}
	containsAll(t, w1.Prompt, "own git worktree at "+wantWT, "branch fanout/r1-1")

	run2 := newRun(t)
	failing := func(context.Context, string, ...string) (string, error) { return "", errors.New("not a repo") }
	err := Plan(context.Background(), run2, specs, registry(t), Defaults{Worktree: true}, failing)
	if err == nil || !strings.Contains(err.Error(), "task 1: worktree: not a repo") {
		t.Fatalf("got %v", err)
	}
}

func TestPlanWorktreeRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"-c", "user.email=a@b", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run := newRun(t)
	run.Cwd = repo
	if err := Plan(context.Background(), run, []task.Spec{{Prompt: "a"}}, registry(t), Defaults{Worktree: true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run.Workers[0].Worktree, ".git")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
	out, err := ExecGit(context.Background(), repo, "branch", "--list", "fanout/*")
	if err != nil || !strings.Contains(out, "fanout/r1-1") {
		t.Fatalf("branch: %q %v", out, err)
	}
	if _, err := ExecGit(context.Background(), t.TempDir(), "rev-parse", "HEAD"); err == nil || !strings.Contains(err.Error(), "git rev-parse HEAD") {
		t.Fatalf("want git error, got %v", err)
	}
}

func TestBuildPrompt(t *testing.T) {
	w := &store.Worker{Task: "  Do the thing\n", ResultPath: "/r/2.md"}
	got := BuildPrompt("", w, 2, 5, "T (abc)")
	want := "## Task\n\nDo the thing\n\n## Hand-off\n\n" +
		`You are worker 2 of 5 in a parallel fan-out started from "T (abc)". ` +
		"Work only on this task. Do not start other agents or subagents, and do not ask for confirmation.\n" +
		"When you are finished, or stuck, write your final report as Markdown to:\n/r/2.md\n" +
		"Put the actual answer in that file: findings, changes with file paths, test results, open questions. Write it in one go with your file-writing tool, then end your turn."
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	got = BuildPrompt("  Be terse.  ", w, 1, 1, "x")
	if !strings.HasPrefix(got, "Be terse.\n\n## Task") {
		t.Fatalf("instructions: %q", got)
	}
	if strings.Contains(got, "worktree") {
		t.Fatal("no worktree line without a worktree")
	}
}

func TestFollowUpAndName(t *testing.T) {
	w := &store.Worker{ResultPath: "/r/1.md"}
	if got := FollowUp("  more  ", w); got != "more\n\n(When you are done, write your complete updated report to /r/1.md, replacing the old one, then end your turn.)" {
		t.Fatalf("got %q", got)
	}
	if WorkerName("abc123", 12) != "fo-abc123-12" {
		t.Fatal(WorkerName("abc123", 12))
	}
}
