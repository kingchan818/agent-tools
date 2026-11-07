package orch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/agentdef"
	"github.com/kingchan818/agent-tools/fanout/internal/hc"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
	"github.com/kingchan818/agent-tools/fanout/internal/task"
	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

type fixture struct {
	t     *testing.T
	clock *fakeClock
	hc    *fakeHC
	st    *store.Store
	run   *store.Run
	o     *Orchestrator
	ev    *eventLog
}

func setup(t *testing.T, prompts ...string) *fixture {
	t.Helper()
	c := newClock()
	f := newFake(t, c)
	s := &store.Store{Root: t.TempDir()}
	run, err := s.Create(func() string { return "abc123" }, c.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Cwd = "/repo"
	run.Thread = thread.Info{Kind: "claude", SessionID: "fc31e7ff-0000", Title: "My thread"}
	var specs []task.Spec
	for _, p := range prompts {
		specs = append(specs, task.Spec{Prompt: p})
	}
	if err := Plan(context.Background(), run, specs, agentdef.NewRegistry(), Defaults{}, nil); err != nil {
		t.Fatal(err)
	}
	ev := &eventLog{}
	o := &Orchestrator{HC: f, Store: s, Now: c.Now, Sleep: c.Sleep, Event: ev.record,
		Opts: Options{ReadyTimeout: 60 * time.Second, SendRetryFor: 10 * time.Second, RetryDelay: 2 * time.Second,
			PollSeconds: 30, NotStartedFor: 10 * time.Second, ScreenLines: 123,
			ReadyPoll: 60 * time.Second, NoStatusPoll: 5 * time.Second}}
	return &fixture{t: t, clock: c, hc: f, st: s, run: run, o: o, ev: ev}
}

func (fx *fixture) w(i int) *store.Worker {
	w, err := fx.run.Worker(i)
	if err != nil {
		fx.t.Fatal(err)
	}
	return w
}

// writesReport is a send step that makes the agent write its report.
func writesReport(w *store.Worker, turn int) sendStep {
	s := sendOK(turn)
	s.do = func(string) { os.WriteFile(w.ResultPath, []byte("# Report\nall good\n"), 0o644) }
	return s
}

func (fx *fixture) happy(i int) {
	w := fx.w(i)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("working", 0), st("done", 1)}, []sendStep{writesReport(w, 0)})
}

func (fx *fixture) reload() *store.Run {
	r, err := fx.st.Load(fx.run.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func TestRunHappyPath(t *testing.T) {
	fx := setup(t, "Review a.go", "Review b.go")
	fx.happy(1)
	fx.happy(2)
	if err := fx.o.Run(context.Background(), fx.run); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		w := fx.w(i)
		if w.State != store.Done || w.Error != "" || !w.Closed || w.Turn != 1 || w.SentTurn != 0 || w.Terminal != addr(w.Name) {
			t.Errorf("worker %d: %+v", i, w)
		}
		if w.StartedAt.IsZero() || w.EndedAt.IsZero() {
			t.Errorf("worker %d: times not set", i)
		}
		if got := fx.ev.states[i]; !reflect.DeepEqual(got, []string{store.Starting, store.Running, store.Running, store.Done, store.Done}) {
			t.Errorf("worker %d events %v", i, got)
		}
		if sent := fx.hc.sent[addr(w.Name)]; len(sent) != 1 || sent[0] != w.Prompt {
			t.Errorf("worker %d sent %q", i, sent)
		}
		if _, err := os.Stat(w.ScreenPath); !os.IsNotExist(err) {
			t.Errorf("worker %d: no screen should be captured on success", i)
		}
	}
	if len(fx.hc.killed) != 2 || len(fx.hc.removed) != 2 {
		t.Fatalf("killed %v removed %v", fx.hc.killed, fx.hc.removed)
	}
	if len(fx.clock.sleeps) != 0 {
		t.Fatalf("a working agent needs no polling sleeps: %v", fx.clock.sleeps)
	}
	// Ready wait uses the remaining ready time; turn waits use PollSeconds.
	calls := fx.hc.waitCalls[addr(fx.w(1).Name)]
	if !reflect.DeepEqual(calls, []int{60, 30, 30}) {
		t.Fatalf("wait timeouts %v", calls)
	}
	saved := fx.reload()
	if saved.Workers[0].State != store.Done || !saved.Workers[1].Closed {
		t.Fatalf("manifest not saved: %+v", saved.Workers[0])
	}
}

func TestLaunchOpts(t *testing.T) {
	fx := setup(t, "Review the parser module")
	o := fx.o.LaunchOpts(fx.run, fx.w(1))
	want := hc.LaunchOpts{
		Name: "fo-abc123-1", Cwd: "/repo",
		Labels: map[string]string{
			"fanout.thread": "My thread (fc31e7ff)", "claude.session": "fc31e7ff-0000",
			"fanout.run": "abc123", "fanout.worker": "1", "fanout.agent": "omp",
			"where": "◆ My thread › Review the parser module",
		},
		NoParent: true, NoNotify: true, OutliveParent: false,
		Argv: []string{"omp", "--add-dir", fx.run.ResultsDir()},
	}
	if !reflect.DeepEqual(o, want) {
		t.Fatalf("got  %+v\nwant %+v", o, want)
	}
	fx.run.Thread.Terminal = "t9"
	fx.o.Opts.Notify = true
	o = fx.o.LaunchOpts(fx.run, fx.w(1))
	if o.NoParent || o.NoNotify || !o.OutliveParent || o.Labels["fanout.caller"] != "t9" {
		t.Fatalf("inside hc: %+v", o)
	}
	fx.o.Opts = Options{Keep: true}
	if o := fx.o.LaunchOpts(fx.run, fx.w(1)); !o.OutliveParent || !o.NoNotify {
		t.Fatalf("keep: %+v", o)
	}
}

func TestKeepLeavesTerminals(t *testing.T) {
	fx := setup(t, "a")
	fx.o.Opts.Keep = true
	fx.happy(1)
	fx.o.Run(context.Background(), fx.run)
	if fx.w(1).State != store.Done || fx.w(1).Closed || len(fx.hc.killed) != 0 {
		t.Fatalf("%+v killed %v", fx.w(1), fx.hc.killed)
	}
}

func TestLaunchFailure(t *testing.T) {
	fx := setup(t, "a", "b")
	fx.hc.launchErr[fx.w(1).Name] = &hc.Error{Kind: "spawn_failed", Message: "no omp"}
	fx.happy(2)
	fx.o.Run(context.Background(), fx.run)
	w := fx.w(1)
	if w.State != store.Failed || w.Terminal != "" || !strings.HasPrefix(w.Error, "launch: ") || w.EndedAt.IsZero() {
		t.Fatalf("%+v", w)
	}
	if fx.w(2).State != store.Done {
		t.Fatal("one failure must not stop the others")
	}
}

func TestReadyWaitsThroughWorkingAndTimeouts(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{
		{r: hc.WaitResult{Reason: "timeout", RawStat: raw("working")}, advance: 20 * time.Second},
		{r: hc.WaitResult{Reason: "status", RawStat: raw("working")}, advance: 15500 * time.Millisecond},
		st("idle", 0), st("done", 1),
	}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done {
		t.Fatalf("%+v", w)
	}
	// 60s, then 40s left, then 24.5s left (rounded up to 25).
	if got := fx.hc.waitCalls[addr(w.Name)][:3]; !reflect.DeepEqual(got, []int{60, 40, 25}) {
		t.Fatalf("ready timeouts %v", got)
	}
}

func TestReadyDeadlinePassesThenSends(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{
		{r: hc.WaitResult{Reason: "timeout", RawStat: raw("working")}, advance: 60 * time.Second},
		st("done", 1),
	}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done || len(fx.hc.waitCalls[addr(w.Name)]) != 2 {
		t.Fatalf("%+v calls %v", w, fx.hc.waitCalls)
	}
}

func TestReadyFailures(t *testing.T) {
	cases := []struct {
		name  string
		step  waitStep
		state string
		msg   string
	}{
		{"exit", exitStep(2), store.Exited, "agent exited during startup (exit 2)"},
		{"exit no code", waitStep{r: hc.WaitResult{Reason: "exit"}}, store.Exited, "agent exited during startup"},
		{"blocked", st("blocked", 0), store.Blocked, "hc attach Tfo-abc123-1"},
		{"error", st("error", 0), store.Errored, "hc read Tfo-abc123-1"},
		{"wait error", waitStep{err: &hc.Error{Kind: "x", Message: "boom"}}, store.Failed, "waiting for startup: hc: x: boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setup(t, "a")
			w := fx.w(1)
			fx.hc.script(w.Name, []waitStep{c.step}, nil)
			fx.o.Run(context.Background(), fx.run)
			if w.State != c.state || !strings.Contains(w.Error, c.msg) {
				t.Fatalf("%+v", w)
			}
			if c.name == "exit no code" && w.Error != "agent exited during startup" {
				t.Fatalf("msg %q", w.Error)
			}
			if len(fx.hc.sent) != 0 {
				t.Fatal("must not send")
			}
			screen, _ := os.ReadFile(w.ScreenPath)
			wantScreen := c.state != store.Failed
			if (string(screen) == "SCREEN\n") != wantScreen {
				t.Fatalf("screen captured=%q want %v", screen, wantScreen)
			}
			if wantScreen && fx.hc.reads[w.Terminal] != 123 {
				t.Fatalf("read lines %v", fx.hc.reads)
			}
		})
	}
}

func TestReadyCancelled(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{{err: context.Canceled}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx.o.Run(ctx, fx.run)
	if w.State != store.Timeout || !strings.Contains(w.Error, "starting") {
		t.Fatalf("%+v", w)
	}
}

func TestSendRetriesWhileStarting(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("done", 1)},
		[]sendStep{{err: retryable}, {err: retryable}, writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done || len(fx.hc.sent[w.Terminal]) != 3 {
		t.Fatalf("%+v sent %d", w, len(fx.hc.sent[w.Terminal]))
	}
	if !reflect.DeepEqual(fx.clock.sleeps, []time.Duration{2 * time.Second, 2 * time.Second}) {
		t.Fatalf("sleeps %v", fx.clock.sleeps)
	}
}

func TestSendRetryGivesUp(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	steps := make([]sendStep, 10)
	for i := range steps {
		steps[i] = sendStep{err: retryable}
	}
	fx.hc.script(w.Name, []waitStep{st("idle", 0)}, steps)
	fx.o.Run(context.Background(), fx.run)
	// SendRetryFor 10s / RetryDelay 2s: tries at 0,2,4,6,8 then the try at 10s fails.
	if w.State != store.Failed || !strings.Contains(w.Error, "still starting") || len(fx.hc.sent[w.Terminal]) != 6 {
		t.Fatalf("%+v sent %d", w, len(fx.hc.sent[w.Terminal]))
	}
}

func TestSendFailures(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		state string
		msg   string
	}{
		{"blocked", &hc.Error{Kind: "blocked", Message: "x"}, store.Blocked, "hc attach"},
		{"finished", &hc.Error{Kind: "finished", Message: "x"}, store.Exited, "exited before"},
		{"other", &hc.Error{Kind: "usage", Message: "bad"}, store.Failed, "send: hc: usage: bad"},
		{"unavailable not retryable", &hc.Error{Kind: "unavailable", Message: "busy", Hint: "typed partially"}, store.Failed, "send:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setup(t, "a")
			w := fx.w(1)
			fx.hc.script(w.Name, []waitStep{st("idle", 0)}, []sendStep{{err: c.err}})
			fx.o.Run(context.Background(), fx.run)
			if w.State != c.state || !strings.Contains(w.Error, c.msg) || len(fx.hc.sent[w.Terminal]) != 1 {
				t.Fatalf("%+v", w)
			}
		})
	}
}

func TestSendRetrySleepCancelled(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.clock.err = context.Canceled
	fx.hc.script(w.Name, []waitStep{st("idle", 0)}, []sendStep{{err: retryable}})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Timeout {
		t.Fatalf("%+v", w)
	}
}

func TestTurnEndsWithoutReport(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), tick(0), st("done", 1)}, []sendStep{sendOK(0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.NoReport || w.Closed || len(fx.hc.killed) != 0 {
		t.Fatalf("%+v", w)
	}
	containsAll(t, w.Error, "without a report", w.ScreenPath)
	if b, _ := os.ReadFile(w.ScreenPath); string(b) != "SCREEN\n" {
		t.Fatalf("screen %q", b)
	}
}

func TestWhitespaceReportCountsAsMissing(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	s := sendOK(0)
	s.do = func(string) { os.WriteFile(w.ResultPath, []byte(" \n\t\r\n"), 0o644) }
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("done", 1)}, []sendStep{s})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.NoReport {
		t.Fatalf("%+v", w)
	}
}

func TestIdleBeforeTurnStartsThenDone(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("idle", 0), st("done", 0), st("idle", 1)}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done {
		t.Fatalf("%+v", w)
	}
	if len(fx.clock.sleeps) != 2 {
		t.Fatalf("sleeps %v", fx.clock.sleeps)
	}
}

func TestWorkingResetsUnstartedTimer(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	// idle(unstarted) 0s, 8s later idle again, then working, then idle unstarted
	// twice more: the timer restarted so 8s+8s never reaches 10s.
	fx.hc.script(w.Name, []waitStep{
		st("idle", 0), {r: hc.WaitResult{Reason: "status", RawStat: raw("idle")}, advance: 6 * time.Second},
		st("working", 0),
		st("idle", 0), {r: hc.WaitResult{Reason: "status", RawStat: raw("idle")}, advance: 6 * time.Second},
		st("done", 1),
	}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done {
		t.Fatalf("%+v", w)
	}
}

func TestNeverStarted(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	steps := []waitStep{st("idle", 0)}
	for range 10 {
		steps = append(steps, st("idle", 0))
	}
	fx.hc.script(w.Name, steps, []sendStep{sendOK(0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Failed || !strings.Contains(w.Error, "never started a turn") {
		t.Fatalf("%+v", w)
	}
	// unstarted at t, then sleeps of 2s: 2,4,6,8 -> at 10s it gives up.
	if len(fx.clock.sleeps) != 5 {
		t.Fatalf("sleeps %v", fx.clock.sleeps)
	}
	if b, _ := os.ReadFile(w.ScreenPath); string(b) != "SCREEN\n" {
		t.Fatal("screen should be saved")
	}
}

func TestNoStatusKeepsWaiting(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("", 0), {r: hc.WaitResult{Reason: "message"}}, st("done", 1)}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done {
		t.Fatalf("%+v", w)
	}
}

func TestTurnOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		step   waitStep
		report bool
		state  string
		msg    string
	}{
		{"exit with report", exitStep(0), true, store.Done, ""},
		{"exit without report", exitStep(3), false, store.Exited, "agent exited (exit 3)"},
		{"blocked", st("blocked", 0), false, store.Blocked, "permission"},
		{"error", st("error", 1), false, store.Errored, "hc read"},
		{"gone", waitStep{err: &hc.Error{Kind: "not_found"}}, false, store.Failed, "is gone"},
		{"wait error", waitStep{err: errors.New("pipe")}, false, store.Failed, "wait: pipe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setup(t, "a")
			w := fx.w(1)
			send := sendOK(0)
			if c.report {
				send = writesReport(w, 0)
			}
			fx.hc.script(w.Name, []waitStep{st("idle", 0), c.step}, []sendStep{send})
			fx.o.Run(context.Background(), fx.run)
			if w.State != c.state || !strings.Contains(w.Error, c.msg) {
				t.Fatalf("%+v", w)
			}
			if c.msg == "" && w.Error != "" {
				t.Fatalf("error should be cleared: %q", w.Error)
			}
		})
	}
}

func TestTurnTracksProgress(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	var seen []int
	fx.o.Event = func(_ *store.Run, w *store.Worker) { seen = append(seen, w.Turn) }
	fx.hc.script(w.Name, []waitStep{st("idle", 2), tick(3), tick(3), st("done", 4)}, []sendStep{writesReport(w, 2)})
	fx.o.Run(context.Background(), fx.run)
	if w.Turn != 4 || w.SentTurn != 2 || w.State != store.Done {
		t.Fatalf("%+v", w)
	}
	// starting(0), running(2), turn 3, turn 4, done(4), closed(4)
	if !reflect.DeepEqual(seen, []int{0, 2, 3, 4, 4, 4}) {
		t.Fatalf("turn events %v", seen)
	}
}

func TestOverallTimeout(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), {err: context.DeadlineExceeded}}, []sendStep{sendOK(0)})
	ctx, cancel := context.WithCancel(context.Background())
	fx.hc.waits[w.Terminal+"x"] = nil
	go func() {
		for {
			fx.hc.mu.Lock()
			n := len(fx.hc.waitCalls[addr(w.Name)])
			fx.hc.mu.Unlock()
			if n >= 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	fx.o.Run(ctx, fx.run)
	if w.State != store.Timeout || !strings.Contains(w.Error, "fanout wait abc123") || w.Closed {
		t.Fatalf("%+v", w)
	}
	if b, _ := os.ReadFile(w.ScreenPath); string(b) != "SCREEN\n" {
		t.Fatal("screen should be captured after the deadline")
	}
}

func TestAwaitCancelledBeforeWait(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	w.State, w.Terminal = store.Running, addr(w.Name)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx.o.Await(ctx, fx.run)
	if w.State != store.Timeout || len(fx.hc.waitCalls) != 0 {
		t.Fatalf("%+v calls %v", w, fx.hc.waitCalls)
	}
}

func TestUnstartedSleepCancelled(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("idle", 0)}, []sendStep{sendOK(0)})
	fx.clock.err = context.Canceled
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Timeout {
		t.Fatalf("%+v", w)
	}
}

func TestReadErrorIsRecorded(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.hc.readErr = errors.New("gone")
	fx.hc.script(w.Name, []waitStep{st("idle", 0), st("done", 1)}, []sendStep{sendOK(0)})
	fx.o.Run(context.Background(), fx.run)
	b, _ := os.ReadFile(w.ScreenPath)
	if !strings.Contains(string(b), "could not read the screen: gone") {
		t.Fatalf("screen %q", b)
	}
}

func TestCleanupErrorsKeepOpen(t *testing.T) {
	fx := setup(t, "a")
	fx.hc.killErr = &hc.Error{Kind: "x"}
	fx.happy(1)
	fx.o.Run(context.Background(), fx.run)
	if fx.w(1).Closed || len(fx.hc.removed) != 0 {
		t.Fatal("kill failure must leave it open")
	}

	fx = setup(t, "a")
	fx.hc.killErr = &hc.Error{Kind: "not_found"}
	fx.happy(1)
	fx.o.Run(context.Background(), fx.run)
	if !fx.w(1).Closed {
		t.Fatal("not_found kill is fine")
	}

	fx = setup(t, "a")
	fx.hc.rmErr = &hc.Error{Kind: "x"}
	fx.happy(1)
	fx.o.Run(context.Background(), fx.run)
	if fx.w(1).Closed {
		t.Fatal("rm failure must leave it open")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	fx := setup(t, "a", "b", "c", "d")
	fx.o.Opts.Concurrency = 2
	for i := 1; i <= 4; i++ {
		fx.happy(i)
	}
	fx.o.Run(context.Background(), fx.run)
	if fx.hc.maxActive > 2 {
		t.Fatalf("max active %d", fx.hc.maxActive)
	}
	for i := 1; i <= 4; i++ {
		if fx.w(i).State != store.Done {
			t.Fatalf("worker %d %s", i, fx.w(i).State)
		}
	}

	fx = setup(t, "a", "b", "c")
	for i := 1; i <= 3; i++ {
		w := fx.w(i)
		fx.hc.script(w.Name, []waitStep{st("idle", 0)}, []sendStep{sendOK(0)})
	}
	fx.o.Start(context.Background(), fx.run)
	if fx.hc.maxActive != 3 {
		t.Fatalf("unlimited start should run all at once, max %d", fx.hc.maxActive)
	}
}

func TestStartThenAwaitFromDisk(t *testing.T) {
	fx := setup(t, "a", "b")
	for i := 1; i <= 2; i++ {
		w := fx.w(i)
		fx.hc.script(w.Name, []waitStep{st("idle", 0)}, []sendStep{sendOK(0)})
	}
	if err := fx.o.Start(context.Background(), fx.run); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if fx.w(i).State != store.Running {
			t.Fatalf("worker %d %s", i, fx.w(i).State)
		}
	}
	// A later process reloads the run and waits.
	run := fx.reload()
	w1, _ := run.Worker(1)
	w2, _ := run.Worker(2)
	fx.hc.waits[w1.Terminal] = []waitStep{{r: hc.WaitResult{Reason: "status", RawStat: raw("done"), Turn: 1},
		do: func() { os.WriteFile(w1.ResultPath, []byte("r1"), 0o644) }}}
	fx.hc.waits[w2.Terminal] = []waitStep{st("blocked", 0)}
	if err := fx.o.Await(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if w1.State != store.Done || w2.State != store.Blocked {
		t.Fatalf("w1 %s w2 %s", w1.State, w2.State)
	}
	// Awaiting again re-checks the blocked one only.
	fx.hc.waits[w2.Terminal] = []waitStep{{r: hc.WaitResult{Reason: "status", RawStat: raw("idle"), Turn: 1},
		do: func() { os.WriteFile(w2.ResultPath, []byte("r2"), 0o644) }}}
	fx.o.Await(context.Background(), run)
	if w2.State != store.Done {
		t.Fatalf("w2 %+v", w2)
	}
}

func TestAwaitSkips(t *testing.T) {
	fx := setup(t, "a", "b", "c", "d", "e")
	states := []struct {
		state  string
		term   string
		closed bool
	}{
		{store.Pending, "", false}, {store.Done, "T1", false}, {store.Running, "", false},
		{store.Timeout, "T4", true}, {store.Failed, "T5", false},
	}
	for i, s := range states {
		w := fx.w(i + 1)
		w.State, w.Terminal, w.Closed = s.state, s.term, s.closed
	}
	fx.o.Await(context.Background(), fx.run)
	if len(fx.hc.waitCalls) != 0 {
		t.Fatalf("nothing should be awaited: %v", fx.hc.waitCalls)
	}
	for _, s := range []string{store.Running, store.Timeout, store.Blocked, store.Errored} {
		if !isState(Awaitable...)(&store.Worker{State: s}) {
			t.Errorf("%s should be awaitable", s)
		}
	}
}

func TestContinue(t *testing.T) {
	fx := setup(t, "a")
	fx.o.Opts.Keep = true
	fx.happy(1)
	fx.o.Run(context.Background(), fx.run)
	w := fx.w(1)
	s := sendOK(1)
	s.do = func(string) { os.WriteFile(w.ResultPath, []byte("v2"), 0o644) }
	fx.hc.script(w.Name, []waitStep{st("working", 1), st("done", 2)}, []sendStep{s})
	if err := fx.o.Continue(context.Background(), fx.run, 1, "  also check c.go  "); err != nil {
		t.Fatal(err)
	}
	if w.State != store.Done || w.SentTurn != 1 || w.Turn != 2 {
		t.Fatalf("%+v", w)
	}
	sent := fx.hc.sent[w.Terminal]
	if sent[len(sent)-1] != FollowUp("also check c.go", w) {
		t.Fatalf("sent %q", sent[len(sent)-1])
	}
	prev, _ := os.ReadFile(strings.TrimSuffix(w.ResultPath, ".md") + ".prev.md")
	cur, _ := os.ReadFile(w.ResultPath)
	if !strings.Contains(string(prev), "all good") || string(cur) != "v2" {
		t.Fatalf("prev %q cur %q", prev, cur)
	}
}

func TestContinueErrors(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	ctx := context.Background()
	if err := fx.o.Continue(ctx, fx.run, 7, "x"); err == nil || !strings.Contains(err.Error(), "no worker 7") {
		t.Fatalf("got %v", err)
	}
	if err := fx.o.Continue(ctx, fx.run, 1, "x"); err == nil || !strings.Contains(err.Error(), "never launched") {
		t.Fatalf("got %v", err)
	}
	w.Terminal = "T"
	w.State = store.Starting
	if err := fx.o.Continue(ctx, fx.run, 1, "x"); err == nil || !strings.Contains(err.Error(), "not received its task") {
		t.Fatalf("got %v", err)
	}
	w.State = store.Pending
	if err := fx.o.Continue(ctx, fx.run, 1, "x"); err == nil || !strings.Contains(err.Error(), "not received its task") {
		t.Fatalf("got %v", err)
	}
	w.State, w.Closed = store.Done, true
	if err := fx.o.Continue(ctx, fx.run, 1, "x"); err == nil || !strings.Contains(err.Error(), "--keep") {
		t.Fatalf("got %v", err)
	}
	w.Closed = false
	w.Terminal = addr(w.Name)
	fx.hc.script(w.Name, nil, []sendStep{{err: &hc.Error{Kind: "finished", Message: "gone"}}})
	if err := fx.o.Continue(ctx, fx.run, 1, "x"); err == nil || !strings.Contains(err.Error(), "exited before") {
		t.Fatalf("got %v", err)
	}
	// An unmovable old report stops the follow-up before anything is sent.
	os.MkdirAll(w.ResultPath, 0o755)
	os.MkdirAll(strings.TrimSuffix(w.ResultPath, ".md")+".prev.md/x", 0o755)
	w.State = store.Done
	before := len(fx.hc.sent[w.Terminal])
	if err := fx.o.Continue(ctx, fx.run, 1, "x"); err == nil {
		t.Fatal("want archive error")
	}
	if len(fx.hc.sent[w.Terminal]) != before {
		t.Fatal("must not send after archive failure")
	}
}

func TestClose(t *testing.T) {
	fx := setup(t, "a", "b", "c")
	ctx := context.Background()
	w1, w2, w3 := fx.w(1), fx.w(2), fx.w(3)
	if err := fx.o.Close(ctx, fx.run, w1); err != nil || len(fx.hc.killed) != 0 {
		t.Fatal("unlaunched worker: nothing to close")
	}
	w2.Terminal, w2.State = "T2", store.Running
	if err := fx.o.Close(ctx, fx.run, w2); err != nil {
		t.Fatal(err)
	}
	if !w2.Closed || w2.State != store.Exited || w2.Error != "closed by fanout kill" || w2.EndedAt.IsZero() {
		t.Fatalf("%+v", w2)
	}
	if err := fx.o.Close(ctx, fx.run, w2); err != nil || len(fx.hc.killed) != 1 {
		t.Fatal("closing twice must be a no-op")
	}
	w3.Terminal, w3.State = "T3", store.Done
	fx.hc.killErr = &hc.Error{Kind: "not_found"}
	if err := fx.o.Close(ctx, fx.run, w3); err != nil || w3.State != store.Done || !w3.Closed {
		t.Fatalf("%+v %v", w3, err)
	}
	w3.Closed = false
	fx.hc.killErr = &hc.Error{Kind: "x"}
	if err := fx.o.Close(ctx, fx.run, w3); err == nil {
		t.Fatal("kill error should surface")
	}
	fx.hc.killErr = nil
	fx.hc.rmErr = &hc.Error{Kind: "y"}
	if err := fx.o.Close(ctx, fx.run, w3); err == nil || w3.Closed {
		t.Fatal("rm error should surface")
	}
}

func TestSaveErrorSurfaces(t *testing.T) {
	fx := setup(t, "a")
	fx.happy(1)
	os.RemoveAll(fx.run.Dir)
	err := fx.o.Run(context.Background(), fx.run)
	if err == nil || !strings.Contains(err.Error(), "saving run") {
		t.Fatalf("got %v", err)
	}
	if err := fx.o.Start(context.Background(), fx.run); err == nil {
		t.Fatal("error should stick")
	}
	if err := fx.o.Save(fx.run); err == nil {
		t.Fatal("Save should fail too")
	}
}

func TestOptionsDefaults(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{ReadyTimeout: 60 * time.Second, SendRetryFor: 90 * time.Second, RetryDelay: 2 * time.Second,
		PollSeconds: 60, NotStartedFor: 60 * time.Second, ScreenLines: 400, ReadyPoll: 10 * time.Second, NoStatusPoll: 5 * time.Second}
	if got != want {
		t.Fatalf("got %+v", got)
	}
	custom := Options{ReadyTimeout: 1, SendRetryFor: 2, RetryDelay: 3, PollSeconds: 4, NotStartedFor: 5, ScreenLines: 6,
		ReadyPoll: 8, NoStatusPoll: 9, Concurrency: 7, Keep: true, Notify: true}
	if custom.withDefaults() != custom {
		t.Fatal("custom options must be kept")
	}
	neg := Options{ReadyTimeout: -1, PollSeconds: -1, ScreenLines: -1, ReadyPoll: -1, NoStatusPoll: -1}.withDefaults()
	if neg.ReadyTimeout != 60*time.Second || neg.PollSeconds != 60 || neg.ScreenLines != 400 || neg.ReadyPoll != 10*time.Second || neg.NoStatusPoll != 5*time.Second {
		t.Fatalf("negatives: %+v", neg)
	}
}

func TestCeilSeconds(t *testing.T) {
	cases := map[time.Duration]int{0: 1, time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2, 60 * time.Second: 60, -time.Second: 1}
	for d, want := range cases {
		if got := ceilSeconds(d); got != want {
			t.Errorf("ceilSeconds(%v) = %d want %d", d, got, want)
		}
	}
}

func TestRealClockAndSleep(t *testing.T) {
	o := New(nil, nil, Options{})
	if time.Since(o.now()) > time.Second {
		t.Fatal("now")
	}
	if err := o.sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestHasReportAndArchive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "1.md")
	if HasReport(p) {
		t.Fatal("missing")
	}
	os.WriteFile(p, []byte("\n x"), 0o644)
	if !HasReport(p) {
		t.Fatal("present")
	}
	if err := archiveReport(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "1.prev.md")); err != nil {
		t.Fatal("archived file missing")
	}
	if err := archiveReport(p); err != nil {
		t.Fatal("archiving a missing report is fine")
	}
}

func TestReadyPollChunks(t *testing.T) {
	fx := setup(t, "a")
	fx.o.Opts.ReadyPoll = 10 * time.Second
	w := fx.w(1)
	fx.hc.script(w.Name, []waitStep{
		{r: hc.WaitResult{Reason: "timeout", RawStat: raw("working")}, advance: 10 * time.Second},
		{r: hc.WaitResult{Reason: "timeout", RawStat: raw("working")}, advance: 45 * time.Second},
		{r: hc.WaitResult{Reason: "timeout", RawStat: raw("working")}, advance: 5 * time.Second},
		st("done", 1),
	}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	// 10s chunks, but never longer than what is left of the 60s: 10, 10 (50 left), 5.
	if got := fx.hc.waitCalls[addr(w.Name)][:3]; !reflect.DeepEqual(got, []int{10, 10, 5}) {
		t.Fatalf("ready timeouts %v", got)
	}
	if w.State != store.Done {
		t.Fatalf("%+v", w)
	}
}

// An agent hc has no status for (e.g. pi without an hc integration): it is
// sent its task after the first quiet startup chunk and is done when its
// report appears.
func TestNoStatusAgent(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	quiet := waitStep{r: hc.WaitResult{Reason: "timeout", RawStat: raw("")}}
	appears := quiet
	appears.do = func() { os.WriteFile(w.ResultPath, []byte("42"), 0o644) }
	fx.hc.script(w.Name, []waitStep{quiet, quiet, quiet, appears}, []sendStep{sendOK(0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done || !w.Closed {
		t.Fatalf("%+v", w)
	}
	// ready (60), first turn wait (30), then fast polls (5, 5).
	if got := fx.hc.waitCalls[addr(w.Name)]; !reflect.DeepEqual(got, []int{60, 30, 5, 5}) {
		t.Fatalf("wait timeouts %v", got)
	}
}

func TestStatusAgentIgnoresEarlyReport(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	// The report exists while the agent is still working: keep waiting for the turn.
	fx.hc.script(w.Name, []waitStep{st("idle", 0), tick(0), tick(0), st("done", 1)}, []sendStep{writesReport(w, 0)})
	fx.o.Run(context.Background(), fx.run)
	if w.State != store.Done || len(fx.hc.waitCalls[addr(w.Name)]) != 4 {
		t.Fatalf("%+v calls %v", w, fx.hc.waitCalls)
	}
	if got := fx.hc.waitCalls[addr(w.Name)]; !reflect.DeepEqual(got, []int{60, 30, 30, 30}) {
		t.Fatalf("status agents keep the normal poll: %v", got)
	}
}

// Screens are captured and terminals cleaned up even after the run's
// context ended, with a fresh deadline of their own.
func TestCaptureAndCleanupOutliveContext(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	ctx, cancel := context.WithCancel(context.Background())
	fx.hc.script(w.Name, []waitStep{st("idle", 0), {r: hc.WaitResult{Reason: "status", RawStat: raw("done"), Turn: 1}, do: cancel}},
		[]sendStep{writesReport(w, 0)})
	fx.o.Run(ctx, fx.run)
	if w.State != store.Done || !w.Closed {
		t.Fatalf("cleanup must run after ctx ended: %+v", w)
	}
	fx = setup(t, "a")
	w = fx.w(1)
	ctx, cancel = context.WithCancel(context.Background())
	fx.hc.script(w.Name, []waitStep{st("idle", 0), {r: hc.WaitResult{Reason: "status", RawStat: raw("done"), Turn: 1}, do: cancel}},
		[]sendStep{sendOK(0)})
	fx.o.Run(ctx, fx.run)
	if b, _ := os.ReadFile(w.ScreenPath); string(b) != "SCREEN\n" {
		t.Fatalf("screen must be captured after ctx ended: %q", b)
	}
}

func TestCaptureScreenWithoutTerminal(t *testing.T) {
	fx := setup(t, "a")
	w := fx.w(1)
	fx.o.captureScreen(context.Background(), w)
	if _, err := os.Stat(w.ScreenPath); !os.IsNotExist(err) {
		t.Fatal("no terminal, nothing to capture")
	}
	if len(fx.hc.reads) != 0 {
		t.Fatal("must not call hc read")
	}
}
