// Package cli implements the fanout command line.
package cli

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/agentdef"
	"github.com/kingchan818/agent-tools/fanout/internal/hc"
	"github.com/kingchan818/agent-tools/fanout/internal/orch"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
	"github.com/kingchan818/agent-tools/fanout/internal/task"
	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

// Version is set at build time.
var Version = "0.1.0"

//go:embed skill.md
var skillDoc string

// HC is what the CLI needs from hc.
type HC interface {
	orch.HC
	Get(ctx context.Context, t string) (hc.Info, error)
	List(ctx context.Context, all bool, labels map[string]string) ([]hc.Info, error)
}

// Env is everything the CLI touches outside itself.
type Env struct {
	Stdout, Stderr io.Writer
	Stdin          io.Reader
	Getenv         func(string) string
	Home           string
	Cwd            string
	HC             HC
	Now            func() time.Time
	Sleep          func(ctx context.Context, d time.Duration) error
	NewID          func() string
	Git            orch.GitFunc
	Exec           func(argv []string) error // replaces the process (attach)
	Opts           orch.Options              // timing overrides (tests)
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) store() *store.Store {
	return &store.Store{Root: store.DefaultRoot(e.Getenv, e.Home)}
}

func (e *Env) orch(opts orch.Options) *orch.Orchestrator {
	o := orch.New(e.HC, e.store(), opts)
	o.Now = e.Now
	o.Sleep = e.Sleep
	return o
}

func (e *Env) baseOpts() orch.Options { return e.Opts }

func (e *Env) thread() thread.Info {
	return thread.Detect(e.Getenv, filepath.Join(e.Home, ".claude"))
}

func (e *Env) agentDirs() []string {
	cfg := e.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(e.Home, ".config")
	}
	return []string{filepath.Join(cfg, "fanout", "agents"), filepath.Join(e.Cwd, ".fanout", "agents")}
}

const mainUsage = `fanout - run non-Claude coding agents (omp, pi, opencode, codex) as parallel
subagents in hc terminals, and collect their reports.

Usage: fanout <command> [options]

  run [opts] TASK...        start one worker per task, wait, print reports
                            (--bg: start and return, like run_in_background)
  wait [RUN]                wait for a background run, print reports
  result [RUN] [N]          print reports again
  status [RUN]              workers of a run, with live hc status
  ls                        runs, newest first (--mine: this thread only)
  send RUN N MESSAGE        continue a worker (like SendMessage), print its new report
  attach RUN N              take over a worker's terminal (ctrl+\ detaches)
  kill RUN [N]              stop a run's workers (or one)
  rm RUN                    stop workers and delete the run's files
  agents                    list agent types (.fanout/agents, ~/.config/fanout/agents)
  threads                   live hc terminals grouped by the thread that started them
  skill                     print the agent skill (how Claude should use fanout)
  install-skill [DIR]       write it to ~/.claude/skills/fanout/SKILL.md
  version

RUN is a run id, a unique prefix, or "last" (the default).
Per command: fanout <command> -h`

// Main runs the CLI and returns the exit code.
func Main(ctx context.Context, args []string, env *Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, mainUsage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(ctx, rest, env)
	case "wait":
		err = cmdWait(ctx, rest, env)
	case "result", "results":
		err = cmdResult(rest, env)
	case "status":
		err = cmdStatus(ctx, rest, env)
	case "ls", "list":
		err = cmdLs(rest, env)
	case "send":
		err = cmdSend(ctx, rest, env)
	case "attach":
		err = cmdAttach(rest, env)
	case "kill", "stop":
		err = cmdKill(ctx, rest, env)
	case "rm":
		err = cmdRm(ctx, rest, env)
	case "agents":
		err = cmdAgents(rest, env)
	case "threads":
		err = cmdThreads(ctx, rest, env)
	case "skill":
		fmt.Fprint(env.Stdout, skillDoc)
	case "install-skill":
		err = cmdInstallSkill(rest, env)
	case "version", "--version", "-V":
		fmt.Fprintln(env.Stdout, "fanout", Version)
	case "help", "-h", "--help":
		fmt.Fprintln(env.Stdout, mainUsage)
	default:
		fmt.Fprintf(env.Stderr, "fanout: unknown command %q\n\n%s\n", cmd, mainUsage)
		return 2
	}
	return exitCode(err, env)
}

// errIncomplete means the command worked but not every worker reported.
var errIncomplete = errors.New("incomplete")

func exitCode(err error, env *Env) int {
	if err == nil {
		return 0
	}
	var h errHelp
	if errors.As(err, &h) {
		fmt.Fprintln(env.Stdout, h.usage)
		return 0
	}
	var u usageErr
	if errors.As(err, &u) {
		fmt.Fprintf(env.Stderr, "fanout: %s\n\n%s\n", u.msg, u.usage)
		return 2
	}
	if !errors.Is(err, errIncomplete) {
		fmt.Fprintln(env.Stderr, "fanout:", err)
	}
	return 1
}

const runUsage = `Usage: fanout run [options] TASK...
       fanout run [options] -f FILE        (tasks separated by --- lines, or a JSON array)
       fanout run [options] --each 'Review {}' ITEM...

Starts one interactive agent per task in its own hc terminal, sends it the
task, waits for its turn to end and prints the report it wrote.

  -a, --agent NAME      agent type (default omp; see fanout agents)
  -m, --model MODEL     model for every worker
      --thinking LEVEL  thinking level (omp, pi, codex)
      --yolo            skip the agents' approval prompts (omp, codex)
  -C, --cwd DIR         folder to work in (default: here)
  -f, --file FILE       read tasks from FILE ("-" = stdin)
      --each TEMPLATE   one task per ITEM, replacing {} with it
  -j, --jobs N          at most N workers at once (default: all)
      --worktree        give each worker its own git worktree and branch
      --keep            keep terminals after they report (for fanout send)
      --bg              start the workers and return; then fanout wait
      --timeout DUR     stop waiting after DUR; workers keep running (default 30m, 0 = never)
      --json            print JSON instead of text`

func cmdRun(ctx context.Context, args []string, env *Env) error {
	f := newFlags("run", runUsage)
	agent := f.str("agent,a", "")
	model := f.str("model,m", "")
	thinking := f.str("thinking", "")
	yolo := f.boolean("yolo")
	cwd := f.str("cwd,C", "")
	file := f.str("file,f", "")
	each := f.str("each", "")
	jobs := f.integer("jobs,j", 0)
	worktree := f.boolean("worktree")
	keep := f.boolean("keep")
	bg := f.boolean("bg")
	timeout := f.duration("timeout", 30*time.Minute)
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	specs, err := gatherTasks(pos, *file, *each, env)
	if err != nil {
		return err
	}
	if *jobs < 0 {
		return usageErr{"--jobs must be 0 or more", runUsage}
	}
	dir := env.Cwd
	if *cwd != "" {
		dir = *cwd
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(env.Cwd, dir)
		}
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("folder %s does not exist", dir)
	}
	reg, err := agentdef.Load(env.agentDirs()...)
	if err != nil {
		fmt.Fprintln(env.Stderr, "fanout: warning:", err)
	}
	st := env.store()
	newID := env.NewID
	if newID == nil {
		newID = store.NewID
	}
	run, err := st.Create(newID, env.now())
	if err != nil {
		return err
	}
	run.Cwd = dir
	run.Thread = env.thread()
	run.Keep = *keep
	defaults := orch.Defaults{Agent: *agent, Model: *model, Thinking: *thinking, Yolo: *yolo, Worktree: *worktree}
	if err := orch.Plan(ctx, run, specs, reg, defaults, env.Git); err != nil {
		_ = st.Remove(run)
		return err
	}
	opts := env.baseOpts()
	opts.Concurrency = *jobs
	opts.Keep = *keep
	opts.Notify = *bg && run.Thread.Terminal != ""
	o := env.orch(opts)
	if err := o.Save(run); err != nil {
		return err
	}
	if !*asJSON {
		fmt.Fprintf(env.Stderr, "fanout: run %s · %d worker(s) · thread %q\n", run.ID, len(run.Workers), run.Thread.Label())
	}
	o.Event = progress(env.Stderr, *asJSON)
	if *bg {
		if err := o.Start(ctx, run); err != nil {
			return err
		}
		return printStarted(run, env, *asJSON)
	}
	wctx, cancel := withTimeout(ctx, *timeout)
	defer cancel()
	if err := o.Run(wctx, run); err != nil {
		return err
	}
	return printReports(run, env, *asJSON, 0)
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func gatherTasks(pos []string, file, each string, env *Env) ([]task.Spec, error) {
	var specs []task.Spec
	if each != "" && file != "" {
		return nil, usageErr{"use --each or --file, not both", runUsage}
	}
	if each != "" {
		s, err := task.Expand(each, pos)
		if err != nil {
			return nil, usageErr{err.Error(), runUsage}
		}
		specs = s
	} else if file != "" {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(env.Stdin)
		} else {
			if !filepath.IsAbs(file) {
				file = filepath.Join(env.Cwd, file)
			}
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, err
		}
		s, err := task.Parse(data)
		if err != nil {
			return nil, err
		}
		specs = append(s, task.FromArgs(pos)...)
	} else {
		specs = task.FromArgs(pos)
	}
	if len(specs) == 0 {
		return nil, usageErr{"no tasks given", runUsage}
	}
	return specs, nil
}

// progress prints one stderr line per worker state change.
func progress(w io.Writer, quiet bool) func(*store.Run, *store.Worker) {
	var mu sync.Mutex
	last := map[int]string{}
	return func(run *store.Run, wk *store.Worker) {
		mu.Lock()
		defer mu.Unlock()
		if quiet || last[wk.Index] == wk.State {
			return
		}
		last[wk.Index] = wk.State
		line := fmt.Sprintf("fanout: [%d/%d] %-9s %s", wk.Index, len(run.Workers), wk.State, wk.Name)
		if wk.Terminal != "" {
			line += " (" + wk.Terminal + ")"
		}
		line += " " + wk.Description
		fmt.Fprintln(w, line)
	}
}

// workerView is a worker as printed in JSON.
type workerView struct {
	Index       int    `json:"index"`
	Description string `json:"description"`
	Agent       string `json:"agent"`
	State       string `json:"state"`
	Terminal    string `json:"terminal,omitempty"`
	Name        string `json:"name"`
	Live        string `json:"live,omitempty"`
	Closed      bool   `json:"closed"`
	Error       string `json:"error,omitempty"`
	ElapsedSec  int    `json:"elapsed_sec"`
	Cwd         string `json:"cwd"`
	Branch      string `json:"branch,omitempty"`
	ResultPath  string `json:"result_path"`
	Report      string `json:"report,omitempty"`
	ScreenTail  string `json:"screen_tail,omitempty"`
}

type runView struct {
	Run     string       `json:"run"`
	Dir     string       `json:"dir"`
	Thread  thread.Info  `json:"thread"`
	Summary string       `json:"summary"`
	Workers []workerView `json:"workers"`
}

func view(run *store.Run, now time.Time, withReports bool, only int) runView {
	v := runView{Run: run.ID, Dir: run.Dir, Thread: run.Thread, Summary: run.Summary(), Workers: []workerView{}}
	for _, w := range run.Workers {
		if only > 0 && w.Index != only {
			continue
		}
		wv := workerView{Index: w.Index, Description: w.Description, Agent: w.Agent, State: w.State,
			Terminal: w.Terminal, Name: w.Name, Closed: w.Closed, Error: w.Error,
			ElapsedSec: int(w.Elapsed(now).Seconds()), Cwd: w.Cwd, Branch: w.Branch, ResultPath: w.ResultPath}
		if withReports {
			wv.Report, wv.ScreenTail = reportOf(w)
		}
		v.Workers = append(v.Workers, wv)
	}
	return v
}

// reportOf returns the worker's report, or the tail of its screen when it
// wrote none.
func reportOf(w *store.Worker) (report, screen string) {
	if b, err := os.ReadFile(w.ResultPath); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), ""
	}
	if b, err := os.ReadFile(w.ScreenPath); err == nil {
		return "", tailLines(string(b), 40)
	}
	return "", ""
}

// tailLines returns the last n non-trailing-blank lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, " \n\t\r"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	lines = lines[max(0, len(lines)-n):]
	return strings.Join(lines, "\n")
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printReports prints every worker's report (or one worker's) and returns
// errIncomplete unless all printed workers are done.
func printReports(run *store.Run, env *Env, asJSON bool, only int) error {
	v := view(run, env.now(), true, only)
	if asJSON {
		if err := writeJSON(env.Stdout, v); err != nil {
			return err
		}
	} else {
		out := env.Stdout
		for _, w := range v.Workers {
			fmt.Fprintf(out, "━━ [%d/%d] %s · %s · %s · %s ━━\n", w.Index, len(run.Workers), w.Description, w.Agent, w.State, time.Duration(w.ElapsedSec)*time.Second)
			if w.Report != "" {
				fmt.Fprintln(out, w.Report)
			} else if w.State == store.Pending || w.State == store.Starting || w.State == store.Running {
				fmt.Fprintln(out, "(still working)")
			} else {
				fmt.Fprintln(out, "(no report)")
			}
			if w.Error != "" && w.State != store.Done {
				fmt.Fprintln(out, "⚠ "+w.Error)
			}
			if w.ScreenTail != "" {
				fmt.Fprintln(out, "--- last lines of its screen ---")
				fmt.Fprintln(out, w.ScreenTail)
			}
			if w.Branch != "" {
				fmt.Fprintf(out, "(branch %s in %s)\n", w.Branch, w.Cwd)
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "fanout: run %s: %s · reports in %s\n", run.ID, nonEmpty(v.Summary, "no workers"), run.ResultsDir())
	}
	for _, w := range v.Workers {
		if w.State != store.Done {
			return errIncomplete
		}
	}
	return nil
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func printStarted(run *store.Run, env *Env, asJSON bool) error {
	if asJSON {
		return writeJSON(env.Stdout, view(run, env.now(), false, 0))
	}
	fmt.Fprintf(env.Stdout, "fanout: started run %s (%s)\n", run.ID, run.Summary())
	printWorkerTable(env.Stdout, view(run, env.now(), false, 0).Workers, false)
	fmt.Fprintf(env.Stdout, "\nWait for the reports: fanout wait %s   ·   live state: fanout status %s\n", run.ID, run.ID)
	for _, w := range run.Workers {
		if w.State != store.Running {
			return errIncomplete
		}
	}
	return nil
}

func printWorkerTable(out io.Writer, ws []workerView, live bool) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if live {
		fmt.Fprintln(tw, "#\tTERMINAL\tAGENT\tSTATE\tLIVE\tTIME\tTASK")
	} else {
		fmt.Fprintln(tw, "#\tTERMINAL\tAGENT\tSTATE\tTIME\tTASK")
	}
	for _, w := range ws {
		term := w.Terminal
		if term == "" {
			term = "-"
		} else if w.Closed {
			term += " (closed)"
		}
		el := (time.Duration(w.ElapsedSec) * time.Second).String()
		if live {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", w.Index, term, w.Agent, w.State, nonEmpty(w.Live, "-"), el, w.Description)
		} else {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", w.Index, term, w.Agent, w.State, el, w.Description)
		}
	}
	tw.Flush()
}

func loadRun(env *Env, pos []string) (*store.Run, error) {
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	return env.store().Load(ref)
}

const waitUsage = `Usage: fanout wait [RUN] [--timeout DUR] [--keep] [--json]

Waits for a run's started workers (also ones that timed out, blocked or
errored before), then prints all reports. Workers keep running if the wait
times out (default 30m, 0 = never).`

func cmdWait(ctx context.Context, args []string, env *Env) error {
	f := newFlags("wait", waitUsage)
	timeout := f.duration("timeout", 30*time.Minute)
	keep := f.boolean("keep")
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 1, waitUsage); err != nil {
		return err
	}
	run, err := loadRun(env, pos)
	if err != nil {
		return err
	}
	opts := env.baseOpts()
	opts.Keep = *keep || run.Keep
	o := env.orch(opts)
	o.Event = progress(env.Stderr, *asJSON)
	wctx, cancel := withTimeout(ctx, *timeout)
	defer cancel()
	if err := o.Await(wctx, run); err != nil {
		return err
	}
	return printReports(run, env, *asJSON, 0)
}

const resultUsage = `Usage: fanout result [RUN] [N] [--json]

Prints the reports of a run, or of worker N.`

func cmdResult(args []string, env *Env) error {
	f := newFlags("result", resultUsage)
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 2, resultUsage); err != nil {
		return err
	}
	run, err := loadRun(env, pos)
	if err != nil {
		return err
	}
	only := 0
	if len(pos) == 2 {
		if only, err = workerIndex(run, pos[1]); err != nil {
			return err
		}
	}
	return printReports(run, env, *asJSON, only)
}

func workerIndex(run *store.Run, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, usageErr{fmt.Sprintf("worker must be a number, got %q", s), mainUsage}
	}
	if _, err := run.Worker(n); err != nil {
		return 0, err
	}
	return n, nil
}

const statusUsage = `Usage: fanout status [RUN] [--json]

Shows a run's workers with their live hc status.`

func cmdStatus(ctx context.Context, args []string, env *Env) error {
	f := newFlags("status", statusUsage)
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 1, statusUsage); err != nil {
		return err
	}
	run, err := loadRun(env, pos)
	if err != nil {
		return err
	}
	v := view(run, env.now(), false, 0)
	for i, w := range v.Workers {
		if w.Terminal == "" || w.Closed {
			continue
		}
		info, err := env.HC.Get(ctx, w.Terminal)
		if err != nil {
			v.Workers[i].Live = "gone"
		} else if info.State == "finished" {
			v.Workers[i].Live = "finished"
		} else {
			v.Workers[i].Live = nonEmpty(info.StatusState(), info.State)
		}
	}
	if *asJSON {
		return writeJSON(env.Stdout, v)
	}
	fmt.Fprintf(env.Stdout, "run %s · %s · thread %q · %s\n\n", run.ID, run.CreatedAt.Local().Format("Jan 2 15:04"), run.Thread.Label(), nonEmpty(v.Summary, "no workers"))
	printWorkerTable(env.Stdout, v.Workers, true)
	return nil
}

const lsUsage = `Usage: fanout ls [--mine] [-n N] [--json]

Lists runs, newest first. --mine shows only runs started from this thread.`

func cmdLs(args []string, env *Env) error {
	f := newFlags("ls", lsUsage)
	mine := f.boolean("mine")
	limit := f.integer("n", 20)
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 0, lsUsage); err != nil {
		return err
	}
	runs, err := env.store().List()
	if err != nil {
		return err
	}
	me := env.thread()
	type row struct {
		ID      string    `json:"id"`
		Created time.Time `json:"created_at"`
		Thread  string    `json:"thread"`
		Workers int       `json:"workers"`
		Summary string    `json:"summary"`
		Cwd     string    `json:"cwd"`
	}
	rows := []row{}
	for _, r := range runs {
		if *mine && !sameThread(r.Thread, me) {
			continue
		}
		rows = append(rows, row{r.ID, r.CreatedAt, r.Thread.Label(), len(r.Workers), r.Summary(), r.Cwd})
	}
	if *limit > 0 {
		rows = rows[:min(len(rows), *limit)]
	}
	if *asJSON {
		return writeJSON(env.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "no runs")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTARTED\tTHREAD\tWORKERS\tSTATE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.ID, r.Created.Local().Format("Jan 2 15:04"), task.Truncate(r.Thread, 40), r.Workers, r.Summary)
	}
	return tw.Flush()
}

func sameThread(a, b thread.Info) bool {
	if a.SessionID != "" || b.SessionID != "" {
		return a.SessionID == b.SessionID
	}
	return a.Kind == b.Kind && a.Title == b.Title
}

const sendUsage = `Usage: fanout send RUN N MESSAGE [--timeout DUR] [--close] [--json]

Sends a follow-up to worker N (its terminal must still be open: start the run
with --keep), waits for that turn and prints its new report. The worker stays
open for more follow-ups unless --close is given.`

func cmdSend(ctx context.Context, args []string, env *Env) error {
	f := newFlags("send", sendUsage)
	timeout := f.duration("timeout", 30*time.Minute)
	closeAfter := f.boolean("close")
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return usageErr{"need RUN, N and MESSAGE", sendUsage}
	}
	run, err := env.store().Load(pos[0])
	if err != nil {
		return err
	}
	n, err := workerIndex(run, pos[1])
	if err != nil {
		return err
	}
	opts := env.baseOpts()
	opts.Keep = !*closeAfter
	o := env.orch(opts)
	o.Event = progress(env.Stderr, *asJSON)
	wctx, cancel := withTimeout(ctx, *timeout)
	defer cancel()
	if err := o.Continue(wctx, run, n, strings.Join(pos[2:], " ")); err != nil {
		return err
	}
	return printReports(run, env, *asJSON, n)
}

const attachUsage = `Usage: fanout attach RUN N

Puts this terminal on worker N's terminal (hc attach); ctrl+\ detaches.`

func cmdAttach(args []string, env *Env) error {
	f := newFlags("attach", attachUsage)
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 2, 2, attachUsage); err != nil {
		return err
	}
	run, err := env.store().Load(pos[0])
	if err != nil {
		return err
	}
	n, err := workerIndex(run, pos[1])
	if err != nil {
		return err
	}
	w, _ := run.Worker(n)
	if w.Terminal == "" || w.Closed {
		return fmt.Errorf("worker %d has no open terminal", n)
	}
	return env.Exec([]string{"hc", "attach", w.Terminal})
}

const killUsage = `Usage: fanout kill RUN [N]

Kills and removes the terminals of a run's workers (or of worker N).`

func cmdKill(ctx context.Context, args []string, env *Env) error {
	f := newFlags("kill", killUsage)
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, 2, killUsage); err != nil {
		return err
	}
	run, err := env.store().Load(pos[0])
	if err != nil {
		return err
	}
	only := 0
	if len(pos) == 2 {
		if only, err = workerIndex(run, pos[1]); err != nil {
			return err
		}
	}
	n, err := closeWorkers(ctx, run, env, only)
	fmt.Fprintf(env.Stdout, "fanout: closed %d terminal(s) of run %s\n", n, run.ID)
	return err
}

func closeWorkers(ctx context.Context, run *store.Run, env *Env, only int) (int, error) {
	o := env.orch(env.baseOpts())
	var errs []error
	closed := 0
	for _, w := range run.Workers {
		if (only > 0 && w.Index != only) || w.Terminal == "" || w.Closed {
			continue
		}
		if err := o.Close(ctx, run, w); err != nil {
			errs = append(errs, fmt.Errorf("worker %d: %w", w.Index, err))
			continue
		}
		closed++
	}
	return closed, errors.Join(errs...)
}

const rmUsage = `Usage: fanout rm RUN

Closes the run's terminals and deletes its reports and manifest (git
worktrees under it are removed with git worktree remove; branches stay).`

func cmdRm(ctx context.Context, args []string, env *Env) error {
	f := newFlags("rm", rmUsage)
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, 1, rmUsage); err != nil {
		return err
	}
	run, err := env.store().Load(pos[0])
	if err != nil {
		return err
	}
	if _, err := closeWorkers(ctx, run, env, 0); err != nil {
		return err
	}
	git := env.Git
	if git == nil {
		git = orch.ExecGit
	}
	for _, w := range run.Workers {
		if w.Worktree == "" {
			continue
		}
		if _, err := git(ctx, w.Worktree, "worktree", "remove", "--force", w.Worktree); err != nil {
			fmt.Fprintf(env.Stderr, "fanout: warning: worker %d: %v\n", w.Index, err)
		}
	}
	if err := env.store().Remove(run); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "fanout: removed run %s\n", run.ID)
	return nil
}

const agentsUsage = `Usage: fanout agents [--json]

Lists agent types. Define more as Markdown files in
~/.config/fanout/agents/NAME.md or ./.fanout/agents/NAME.md (project wins):

  ---
  description: Read-only reviewer
  backend: omp          # omp | pi | opencode | codex   (or command: my-agent --flag)
  model: deepseek-v4-flash
  thinking: medium
  yolo: false
  args: --no-lsp
  ---
  Instructions every worker of this type receives before its task.`

func cmdAgents(args []string, env *Env) error {
	f := newFlags("agents", agentsUsage)
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 0, agentsUsage); err != nil {
		return err
	}
	reg, loadErr := agentdef.Load(env.agentDirs()...)
	if loadErr != nil {
		fmt.Fprintln(env.Stderr, "fanout: warning:", loadErr)
	}
	if *asJSON {
		return writeJSON(env.Stdout, reg.All())
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBACKEND\tMODEL\tSOURCE\tDESCRIPTION")
	for _, d := range reg.All() {
		src := d.Source
		if src != "builtin" {
			if rel, err := filepath.Rel(env.Home, src); err == nil && !strings.HasPrefix(rel, "..") {
				src = "~/" + rel
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.BackendName(), nonEmpty(d.Model, "-"), src, d.Description)
	}
	return tw.Flush()
}

const threadsUsage = `Usage: fanout threads [--json]

Shows every live hc terminal grouped by the thread (Claude Code session, or
other caller) that started it. Terminals not started by fanout are listed
last.`

type threadTerm struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Status  string `json:"status"`
	Where   string `json:"where,omitempty"`
	Run     string `json:"run,omitempty"`
	Worker  string `json:"worker,omitempty"`
}

type threadGroup struct {
	Thread    string       `json:"thread"`
	Session   string       `json:"session,omitempty"`
	Current   bool         `json:"current"`
	Terminals []threadTerm `json:"terminals"`
}

func cmdThreads(ctx context.Context, args []string, env *Env) error {
	f := newFlags("threads", threadsUsage)
	asJSON := f.boolean("json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 0, threadsUsage); err != nil {
		return err
	}
	terms, err := env.HC.List(ctx, false, nil)
	if err != nil {
		return err
	}
	me := env.thread()
	groups := map[string]*threadGroup{}
	var other []threadTerm
	for _, t := range terms {
		labels := t.Labels
		if labels == nil {
			if info, err := env.HC.Get(ctx, t.Address); err == nil {
				labels = info.Labels
				t.Status = info.Status
			}
		}
		tt := threadTerm{Address: t.Address, Name: t.Name, Agent: t.Agent, Status: nonEmpty(t.StatusState(), t.State),
			Where: t.Where, Run: labels["fanout.run"], Worker: labels["fanout.worker"]}
		name := labels["fanout.thread"]
		if name == "" {
			other = append(other, tt)
			continue
		}
		key := name + "\x00" + labels["claude.session"]
		g := groups[key]
		if g == nil {
			g = &threadGroup{Thread: name, Session: labels["claude.session"]}
			g.Current = (g.Session != "" && g.Session == me.SessionID) || (g.Session == "" && name == me.Label())
			groups[key] = g
		}
		g.Terminals = append(g.Terminals, tt)
	}
	var list []threadGroup
	for _, g := range groups {
		list = append(list, *g)
	}
	slices.SortFunc(list, func(a, b threadGroup) int {
		return cmp.Or(cmp.Compare(rank(a.Current), rank(b.Current)),
			strings.Compare(a.Thread, b.Thread), strings.Compare(a.Session, b.Session))
	})
	if other != nil {
		list = append(list, threadGroup{Thread: "(not started by fanout)", Terminals: other})
	}
	if *asJSON {
		if list == nil {
			list = []threadGroup{}
		}
		return writeJSON(env.Stdout, list)
	}
	if len(list) == 0 {
		fmt.Fprintln(env.Stdout, "no live terminals")
		return nil
	}
	for i, g := range list {
		if i > 0 {
			fmt.Fprintln(env.Stdout)
		}
		title := "◆ " + g.Thread
		if g.Current {
			title += "   ← this thread"
		}
		fmt.Fprintln(env.Stdout, title)
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		for _, t := range g.Terminals {
			ref := ""
			if t.Run != "" {
				ref = "run " + t.Run + " #" + t.Worker
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", t.Address, nonEmpty(t.Name, "-"), nonEmpty(t.Agent, "-"), t.Status, ref, t.Where)
		}
		tw.Flush()
	}
	return nil
}

// rank sorts true before false.
func rank(first bool) int {
	if first {
		return 0
	}
	return 1
}

const installSkillUsage = `Usage: fanout install-skill [DIR]

Writes the fanout skill to DIR/fanout/SKILL.md (default ~/.claude/skills),
so Claude Code knows how to use fanout as subagents.`

func cmdInstallSkill(args []string, env *Env) error {
	f := newFlags("install-skill", installSkillUsage)
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 0, 1, installSkillUsage); err != nil {
		return err
	}
	dir := filepath.Join(env.Home, ".claude", "skills")
	if len(pos) == 1 {
		dir = pos[0]
	}
	path := filepath.Join(dir, "fanout", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(skillDoc), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "fanout: wrote", path)
	return nil
}
