// Package orch runs fan-outs: it launches one agent per task in an hc
// terminal, hands it its task, waits for its turn to end, and collects the
// report it wrote, the way Claude Code runs subagents.
package orch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/hc"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

// HC is the part of the hc client the orchestrator uses.
type HC interface {
	Launch(ctx context.Context, o hc.LaunchOpts) (hc.Terminal, error)
	Wait(ctx context.Context, t string, timeoutSec int) (hc.WaitResult, error)
	Send(ctx context.Context, t, text string, wait bool, timeoutSec int) (hc.SendResult, error)
	Read(ctx context.Context, t string, lines int) (string, error)
	Kill(ctx context.Context, t string) error
	Rm(ctx context.Context, t string) error
}

// Options tune the orchestrator. Zero values take the defaults below.
type Options struct {
	ReadyTimeout  time.Duration // how long an agent may take to boot (60s)
	SendRetryFor  time.Duration // how long to retry a send refused at startup (90s)
	RetryDelay    time.Duration // pause between retries (2s)
	PollSeconds   int           // timeout of each hc wait call (60)
	NotStartedFor time.Duration // how long a sent task may sit unstarted (60s)
	ScreenLines   int           // history captured when no report is written (400)
	ReadyPoll     time.Duration // startup wait chunk; agents without hc status are sent their task after it (10s)
	NoStatusPoll  time.Duration // wait chunk for agents without hc status, which finish when their report appears (5s)
	Concurrency   int           // max workers at once; 0 = all
	Keep          bool          // keep finished workers' terminals for follow-ups
	Notify        bool          // let hc type worker notes into the calling terminal
}

func (o Options) withDefaults() Options {
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 60 * time.Second
	}
	if o.SendRetryFor <= 0 {
		o.SendRetryFor = 90 * time.Second
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 2 * time.Second
	}
	if o.PollSeconds <= 0 {
		o.PollSeconds = 60
	}
	if o.NotStartedFor <= 0 {
		o.NotStartedFor = 60 * time.Second
	}
	if o.ScreenLines <= 0 {
		o.ScreenLines = 400
	}
	if o.ReadyPoll <= 0 {
		o.ReadyPoll = 10 * time.Second
	}
	if o.NoStatusPoll <= 0 {
		o.NoStatusPoll = 5 * time.Second
	}
	return o
}

// Orchestrator drives workers through hc.
type Orchestrator struct {
	HC    HC
	Store *store.Store
	Opts  Options
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Event is called after every worker state change (for progress output).
	Event func(run *store.Run, w *store.Worker)

	errMu   sync.Mutex
	saveErr error
}

// New returns an orchestrator with real time.
func New(h HC, s *store.Store, opts Options) *Orchestrator {
	return &Orchestrator{HC: h, Store: s, Opts: opts}
}

func (o *Orchestrator) opts() Options { return o.Opts.withDefaults() }

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Orchestrator) sleep(ctx context.Context, d time.Duration) error {
	if o.Sleep != nil {
		return o.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// update applies fn to w under the run lock, saves the manifest, and emits
// an event.
func (o *Orchestrator) update(run *store.Run, w *store.Worker, fn func(w *store.Worker)) {
	run.Lock()
	fn(w)
	err := o.Store.Save(run)
	run.Unlock()
	if err != nil {
		o.errMu.Lock()
		if o.saveErr == nil {
			o.saveErr = err
		}
		o.errMu.Unlock()
	}
	if o.Event != nil {
		o.Event(run, w)
	}
}

// Save persists the run (used before workers start).
func (o *Orchestrator) Save(run *store.Run) error {
	run.Lock()
	defer run.Unlock()
	return o.Store.Save(run)
}

func (o *Orchestrator) err() error {
	o.errMu.Lock()
	defer o.errMu.Unlock()
	if o.saveErr != nil {
		return fmt.Errorf("saving run: %w", o.saveErr)
	}
	return nil
}

// forEach runs fn for every worker selected by pick, at most
// Concurrency at a time, and waits for all of them.
func (o *Orchestrator) forEach(run *store.Run, pick func(*store.Worker) bool, fn func(*store.Worker)) {
	limit := o.opts().Concurrency
	var sem chan struct{}
	if limit > 0 {
		sem = make(chan struct{}, limit)
	}
	var wg sync.WaitGroup
	for _, w := range run.Workers {
		if !pick(w) {
			continue
		}
		wg.Add(1)
		go func(w *store.Worker) {
			defer wg.Done()
			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}
			fn(w)
		}(w)
	}
	wg.Wait()
}

func isState(states ...string) func(*store.Worker) bool {
	return func(w *store.Worker) bool {
		for _, s := range states {
			if w.State == s {
				return true
			}
		}
		return false
	}
}

// Run starts every pending worker and waits for all of them to finish.
// When ctx ends first, unfinished workers are marked timeout and keep running.
func (o *Orchestrator) Run(ctx context.Context, run *store.Run) error {
	o.forEach(run, isState(store.Pending), func(w *store.Worker) {
		if o.start(ctx, run, w) {
			o.await(ctx, run, w)
		}
	})
	return o.err()
}

// Start launches every pending worker and hands it its task, then returns
// without waiting for the work (Claude Code's background agents).
func (o *Orchestrator) Start(ctx context.Context, run *store.Run) error {
	o.forEach(run, isState(store.Pending), func(w *store.Worker) { o.start(ctx, run, w) })
	return o.err()
}

// Awaitable are the states Await picks up again.
var Awaitable = []string{store.Running, store.Timeout, store.Blocked, store.Errored}

// Await waits for started workers (including ones that timed out, blocked
// or errored earlier and may have been helped since) and collects them.
func (o *Orchestrator) Await(ctx context.Context, run *store.Run) error {
	o.forEach(run, func(w *store.Worker) bool {
		return isState(Awaitable...)(w) && w.Terminal != "" && !w.Closed
	}, func(w *store.Worker) { o.await(ctx, run, w) })
	return o.err()
}

// Continue sends a follow-up message to one worker (like Claude Code's
// SendMessage to a finished agent), waits for that turn and collects its
// new report.
func (o *Orchestrator) Continue(ctx context.Context, run *store.Run, index int, message string) error {
	w, err := run.Worker(index)
	if err != nil {
		return err
	}
	if w.Terminal == "" {
		return fmt.Errorf("worker %d was never launched", index)
	}
	if w.Closed {
		return fmt.Errorf("worker %d's terminal was closed after it finished; use --keep to keep workers for follow-ups", index)
	}
	if w.State == store.Pending || w.State == store.Starting {
		return fmt.Errorf("worker %d has not received its task yet", index)
	}
	if err := archiveReport(w.ResultPath); err != nil {
		return err
	}
	if !o.send(ctx, run, w, FollowUp(message, w)) {
		return fmt.Errorf("worker %d: %s", index, w.Error)
	}
	o.await(ctx, run, w)
	return o.err()
}

// archiveReport moves an existing report to <n>.prev.md so the next turn
// must write a fresh one.
func archiveReport(path string) error {
	ext := filepath.Ext(path)
	prev := path[:len(path)-len(ext)] + ".prev" + ext
	err := os.Rename(path, prev)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// start launches w, waits for it to boot and sends its task. It reports
// whether the task was delivered.
func (o *Orchestrator) start(ctx context.Context, run *store.Run, w *store.Worker) bool {
	return o.launch(ctx, run, w) && o.ready(ctx, run, w) && o.send(ctx, run, w, w.Prompt)
}

func (o *Orchestrator) fail(run *store.Run, w *store.Worker, state, msg string) {
	o.update(run, w, func(w *store.Worker) {
		w.State = state
		w.Error = msg
		w.EndedAt = o.now()
	})
}

// LaunchOpts builds the hc launch options for w.
func (o *Orchestrator) LaunchOpts(run *store.Run, w *store.Worker) hc.LaunchOpts {
	opts := o.opts()
	labels := thread.Labels(run.Thread)
	labels["fanout.run"] = run.ID
	labels["fanout.worker"] = strconv.Itoa(w.Index)
	labels["fanout.agent"] = w.Agent
	labels["where"] = thread.Where(run.Thread, w.Description, 60)
	return hc.LaunchOpts{
		Name:          w.Name,
		Cwd:           w.Cwd,
		Labels:        labels,
		NoParent:      run.Thread.Terminal == "",
		NoNotify:      !opts.Notify,
		OutliveParent: opts.Keep || opts.Notify,
		Argv:          w.Argv,
	}
}

func (o *Orchestrator) launch(ctx context.Context, run *store.Run, w *store.Worker) bool {
	t, err := o.HC.Launch(ctx, o.LaunchOpts(run, w))
	if err != nil {
		o.fail(run, w, store.Failed, "launch: "+err.Error())
		return false
	}
	o.update(run, w, func(w *store.Worker) {
		w.Terminal = t.Address
		w.State = store.Starting
		w.StartedAt = o.now()
		w.Error = ""
	})
	return true
}

func ceilSeconds(d time.Duration) int {
	return int(math.Max(1, math.Ceil(d.Seconds())))
}

// ready waits until the freshly started agent is idle. If it never settles
// within ReadyTimeout the send is attempted anyway (it retries safely).
func (o *Orchestrator) ready(ctx context.Context, run *store.Run, w *store.Worker) bool {
	deadline := o.now().Add(o.opts().ReadyTimeout)
	for {
		remaining := deadline.Sub(o.now())
		if remaining <= 0 {
			return true
		}
		r, err := o.HC.Wait(ctx, w.Terminal, ceilSeconds(min(remaining, o.opts().ReadyPoll)))
		if err != nil {
			if ctx.Err() != nil {
				o.fail(run, w, store.Timeout, "gave up while the agent was starting")
				return false
			}
			o.fail(run, w, store.Failed, "waiting for startup: "+err.Error())
			return false
		}
		if r.Reason == "exit" {
			o.captureScreen(ctx, w)
			o.fail(run, w, store.Exited, "agent exited during startup"+exitDetail(r))
			return false
		}
		status := r.Status()
		if status == "blocked" {
			o.captureScreen(ctx, w)
			o.fail(run, w, store.Blocked, "agent needs a person before it can start (dialog or login?): hc attach "+w.Terminal)
			return false
		}
		if status == "error" {
			o.captureScreen(ctx, w)
			o.fail(run, w, store.Errored, "agent reported an error during startup: hc read "+w.Terminal)
			return false
		}
		if r.Reason == "status" && status != "working" {
			return true
		}
		if r.Reason == "timeout" && status == "" {
			// The agent reports no status to hc; hc's send retries until
			// its output settles, which is the best readiness signal left.
			return true
		}
	}
}

func exitDetail(r hc.WaitResult) string {
	if r.ExitCode != nil {
		return fmt.Sprintf(" (exit %d)", *r.ExitCode)
	}
	return ""
}

// send delivers text, retrying while hc reports the agent is still
// starting and nothing was typed.
func (o *Orchestrator) send(ctx context.Context, run *store.Run, w *store.Worker, text string) bool {
	deadline := o.now().Add(o.opts().SendRetryFor)
	for {
		r, err := o.HC.Send(ctx, w.Terminal, text, false, 0)
		if err == nil {
			o.update(run, w, func(w *store.Worker) {
				w.SentTurn = r.Turn
				w.Turn = r.Turn
				w.State = store.Running
				w.Error = ""
				w.EndedAt = time.Time{}
			})
			return true
		}
		if hc.IsKind(err, "blocked") {
			o.fail(run, w, store.Blocked, "agent is waiting for a person: hc attach "+w.Terminal)
			return false
		}
		if hc.IsKind(err, "finished") {
			o.fail(run, w, store.Exited, "agent exited before it got the task")
			return false
		}
		if !hc.IsStartupRetryable(err) || !o.now().Before(deadline) {
			o.fail(run, w, store.Failed, "send: "+err.Error())
			return false
		}
		if o.sleep(ctx, o.opts().RetryDelay) != nil {
			o.fail(run, w, store.Timeout, "gave up while the agent was starting")
			return false
		}
	}
}

// await waits for w's turn to end, then collects and cleans up.
func (o *Orchestrator) await(ctx context.Context, run *store.Run, w *store.Worker) {
	opts := o.opts()
	var unstartedSince time.Time
	noStatus := false
	for {
		if ctx.Err() != nil {
			o.timeout(ctx, run, w)
			return
		}
		poll := opts.PollSeconds
		if noStatus {
			poll = ceilSeconds(opts.NoStatusPoll)
		}
		r, err := o.HC.Wait(ctx, w.Terminal, poll)
		if err != nil {
			if ctx.Err() != nil {
				o.timeout(ctx, run, w)
			} else if hc.IsKind(err, "not_found") {
				o.fail(run, w, store.Failed, "terminal "+w.Terminal+" is gone")
			} else {
				o.fail(run, w, store.Failed, "wait: "+err.Error())
			}
			return
		}
		if r.Turn > w.Turn {
			o.update(run, w, func(w *store.Worker) { w.Turn = r.Turn })
		}
		if r.Reason == "exit" {
			o.finish(ctx, run, w, store.Exited, "agent exited"+exitDetail(r))
			return
		}
		if r.Reason != "status" {
			// A timeout check-in or a message. An agent that reports no
			// status to hc is done once its report appears.
			if r.Status() == "" {
				noStatus = true
				if HasReport(w.ResultPath) {
					o.finish(ctx, run, w, store.Done, "")
					return
				}
			}
			continue
		}
		switch r.Status() {
		case "working":
			unstartedSince = time.Time{}
			continue
		case "blocked":
			o.captureScreen(ctx, w)
			o.fail(run, w, store.Blocked, "agent is waiting for a person (permission or question): hc attach "+w.Terminal)
			return
		case "error":
			o.captureScreen(ctx, w)
			o.fail(run, w, store.Errored, "agent reported an error: hc read "+w.Terminal)
			return
		case "idle", "done":
			if r.Turn > w.SentTurn {
				o.finish(ctx, run, w, store.NoReport, "")
				return
			}
		}
		// Idle without a new turn (task not picked up yet) or no status.
		if unstartedSince.IsZero() {
			unstartedSince = o.now()
		} else if o.now().Sub(unstartedSince) >= opts.NotStartedFor {
			o.finish(ctx, run, w, store.Failed, "the task was sent but the agent never started a turn: hc attach "+w.Terminal)
			return
		}
		if o.sleep(ctx, opts.RetryDelay) != nil {
			o.timeout(ctx, run, w)
			return
		}
	}
}

// finish ends w: Done when it wrote its report, else the given state
// (with its screen saved), then cleans up its terminal.
func (o *Orchestrator) finish(ctx context.Context, run *store.Run, w *store.Worker, otherwise, msg string) {
	if HasReport(w.ResultPath) {
		otherwise, msg = store.Done, ""
	} else {
		o.captureScreen(ctx, w)
		if msg == "" {
			msg = "turn ended without a report; its screen is in " + w.ScreenPath
		}
	}
	o.update(run, w, func(w *store.Worker) {
		w.State = otherwise
		w.Error = msg
		w.EndedAt = o.now()
	})
	o.cleanup(ctx, run, w)
}

func (o *Orchestrator) timeout(ctx context.Context, run *store.Run, w *store.Worker) {
	o.captureScreen(ctx, w)
	o.fail(run, w, store.Timeout, "still running when the wait ended: fanout wait "+run.ID+", or hc attach "+w.Terminal)
}

// captureScreen saves w's screen and history next to its report. It runs
// even after ctx ended (for timeouts), with its own short deadline.
func (o *Orchestrator) captureScreen(ctx context.Context, w *store.Worker) {
	if w.Terminal == "" {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	text, err := o.HC.Read(cctx, w.Terminal, o.opts().ScreenLines)
	if err != nil {
		text = "(could not read the screen: " + err.Error() + ")\n"
	}
	_ = os.WriteFile(w.ScreenPath, []byte(text), 0o644)
}

// cleanup closes a finished worker's terminal unless Keep is set or it
// needs attention.
func (o *Orchestrator) cleanup(ctx context.Context, run *store.Run, w *store.Worker) {
	if o.opts().Keep || w.State != store.Done || w.Closed {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := o.HC.Kill(cctx, w.Terminal); err != nil && !hc.IsKind(err, "not_found") {
		return
	}
	if err := o.HC.Rm(cctx, w.Terminal); err != nil {
		return
	}
	o.update(run, w, func(w *store.Worker) { w.Closed = true })
}

// Close kills and removes a worker's terminal regardless of its state.
func (o *Orchestrator) Close(ctx context.Context, run *store.Run, w *store.Worker) error {
	if w.Terminal == "" || w.Closed {
		return nil
	}
	if err := o.HC.Kill(ctx, w.Terminal); err != nil && !hc.IsKind(err, "not_found") {
		return err
	}
	if err := o.HC.Rm(ctx, w.Terminal); err != nil {
		return err
	}
	o.update(run, w, func(w *store.Worker) {
		w.Closed = true
		if !store.Final(w.State) {
			w.State = store.Exited
			w.Error = "closed by fanout kill"
			w.EndedAt = o.now()
		}
	})
	return o.err()
}

// HasReport reports whether a non-blank report exists at path.
func HasReport(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, c := range data {
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			return true
		}
	}
	return false
}
