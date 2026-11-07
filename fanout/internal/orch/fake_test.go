package orch

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/hc"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
)

// fakeClock is a manual clock; Sleep advances it.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
	err    error // returned by Sleep when set
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	if c.err != nil {
		return c.err
	}
	c.t = c.t.Add(d)
	return nil
}

type waitStep struct {
	r       hc.WaitResult
	err     error
	advance time.Duration
	do      func()
}

type sendStep struct {
	r   hc.SendResult
	err error
	do  func(text string)
}

// fakeHC scripts hc per worker name. Terminal address = "T" + name.
type fakeHC struct {
	t     *testing.T
	clock *fakeClock
	mu    sync.Mutex

	launches  []hc.LaunchOpts
	launchErr map[string]error
	waits     map[string][]waitStep
	sends     map[string][]sendStep
	sent      map[string][]string
	waitCalls map[string][]int // timeout seconds per call
	reads     map[string]int
	readErr   error
	readText  string
	killed    []string
	removed   []string
	killErr   error
	rmErr     error

	active, maxActive int
}

func newFake(t *testing.T, c *fakeClock) *fakeHC {
	return &fakeHC{t: t, clock: c, launchErr: map[string]error{}, waits: map[string][]waitStep{},
		sends: map[string][]sendStep{}, sent: map[string][]string{}, waitCalls: map[string][]int{},
		reads: map[string]int{}, readText: "SCREEN\n"}
}

func addr(name string) string { return "T" + name }

func (f *fakeHC) Launch(_ context.Context, o hc.LaunchOpts) (hc.Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, o)
	if err := f.launchErr[o.Name]; err != nil {
		return hc.Terminal{}, err
	}
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	return hc.Terminal{Address: addr(o.Name), Name: o.Name, State: "live"}, nil
}

func (f *fakeHC) Wait(ctx context.Context, t string, timeoutSec int) (hc.WaitResult, error) {
	f.mu.Lock()
	f.waitCalls[t] = append(f.waitCalls[t], timeoutSec)
	steps := f.waits[t]
	if len(steps) == 0 {
		f.mu.Unlock()
		f.t.Errorf("unexpected wait on %s", t)
		return hc.WaitResult{}, context.Canceled
	}
	s := steps[0]
	f.waits[t] = steps[1:]
	f.mu.Unlock()
	if s.do != nil {
		s.do()
	}
	if s.advance > 0 {
		f.clock.Advance(s.advance)
	}
	if s.err == context.Canceled || s.err == context.DeadlineExceeded {
		<-ctx.Done()
		return hc.WaitResult{}, ctx.Err()
	}
	return s.r, s.err
}

func (f *fakeHC) Send(_ context.Context, t, text string, wait bool, _ int) (hc.SendResult, error) {
	if wait {
		f.t.Errorf("orchestrator must not use send --wait")
	}
	f.mu.Lock()
	f.sent[t] = append(f.sent[t], text)
	steps := f.sends[t]
	if len(steps) == 0 {
		f.mu.Unlock()
		f.t.Errorf("unexpected send on %s", t)
		return hc.SendResult{}, &hc.Error{Kind: "test"}
	}
	s := steps[0]
	f.sends[t] = steps[1:]
	f.mu.Unlock()
	if s.do != nil {
		s.do(text)
	}
	return s.r, s.err
}

func (f *fakeHC) Read(ctx context.Context, t string, lines int) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[t] = lines
	return f.readText, f.readErr
}

func (f *fakeHC) Kill(ctx context.Context, t string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, t)
	return f.killErr
}

func (f *fakeHC) Rm(ctx context.Context, t string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rmErr != nil {
		return f.rmErr
	}
	f.removed = append(f.removed, t)
	f.active--
	return nil
}

func (f *fakeHC) script(name string, waits []waitStep, sends []sendStep) {
	f.waits[addr(name)] = waits
	f.sends[addr(name)] = sends
}

func raw(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(s)
	return b
}

func st(status string, turn int) waitStep {
	return waitStep{r: hc.WaitResult{Ended: true, Reason: "status", RawStat: raw(status), Turn: turn}}
}

func tick(turn int) waitStep {
	return waitStep{r: hc.WaitResult{Reason: "timeout", RawStat: raw("working"), Turn: turn}}
}

func exitStep(code int) waitStep {
	return waitStep{r: hc.WaitResult{Ended: true, Reason: "exit", RawStat: raw(""), ExitCode: &code}}
}

func sendOK(turn int) sendStep {
	return sendStep{r: hc.SendResult{Started: true, RawStat: raw("idle"), Turn: turn}}
}

var retryable = &hc.Error{Kind: "unavailable", Message: "t3 is still starting: its output has not settled", Hint: "nothing was typed; check hc read t3, then retry"}

func containsAll(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%q does not contain %q", s, sub)
		}
	}
}

// eventLog records worker states in order, per worker.
type eventLog struct {
	mu     sync.Mutex
	states map[int][]string
}

func (e *eventLog) record(_ *store.Run, w *store.Worker) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.states == nil {
		e.states = map[int][]string{}
	}
	e.states[w.Index] = append(e.states[w.Index], w.State)
}
