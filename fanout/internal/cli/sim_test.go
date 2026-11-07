package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/hc"
	"github.com/kingchan818/agent-tools/fanout/internal/orch"
)

// simTerm is one simulated agent terminal.
type simTerm struct {
	opts    hc.LaunchOpts
	turn    int
	working bool
	task    string
	killed  bool
	removed bool
}

// simHC behaves like hc running omp: the agent boots idle, works on what it
// is sent and writes the report the prompt asks for. Task text controls it:
// NOREPORT (ends its turn without a report), BLOCK (asks for permission),
// HANG (works forever).
type simHC struct {
	mu        sync.Mutex
	terms     map[string]*simTerm
	order     []string
	sends     map[string][]string
	listErr   error
	launchErr error
	others    []hc.Info
	deadlines []time.Duration // time left on each Wait's context; -1 = none
}

func newSim() *simHC { return &simHC{terms: map[string]*simTerm{}, sends: map[string][]string{}} }

var reportPath = regexp.MustCompile(`(?:Markdown to:\n|report to )(\S+?)(?:,|\n|$)`)

func (s *simHC) Launch(_ context.Context, o hc.LaunchOpts) (hc.Terminal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.launchErr != nil {
		return hc.Terminal{}, s.launchErr
	}
	addr := "t" + o.Name
	s.terms[addr] = &simTerm{opts: o}
	s.order = append(s.order, addr)
	return hc.Terminal{Address: addr, Name: o.Name, State: "live"}, nil
}

func status(st string, turn int) hc.WaitResult {
	b, _ := json.Marshal(st)
	return hc.WaitResult{Ended: true, Reason: "status", RawStat: b, Turn: turn}
}

func (s *simHC) Wait(ctx context.Context, t string, _ int) (hc.WaitResult, error) {
	s.mu.Lock()
	if d, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, time.Until(d))
	} else {
		s.deadlines = append(s.deadlines, -1)
	}
	term := s.terms[t]
	if term == nil || term.removed {
		s.mu.Unlock()
		return hc.WaitResult{}, &hc.Error{Kind: "not_found", Message: "no terminal"}
	}
	if !term.working {
		defer s.mu.Unlock()
		return status("idle", term.turn), nil
	}
	switch {
	case strings.Contains(term.task, "HANG"):
		s.mu.Unlock()
		<-ctx.Done()
		return hc.WaitResult{}, ctx.Err()
	case strings.Contains(term.task, "BLOCK"):
		defer s.mu.Unlock()
		return status("blocked", term.turn), nil
	}
	defer s.mu.Unlock()
	if !strings.Contains(term.task, "NOREPORT") {
		if m := reportPath.FindStringSubmatch(term.task); m != nil {
			first := strings.SplitN(term.task, "\n", 2)[0]
			os.WriteFile(m[1], []byte("REPORT turn "+string(rune('0'+term.turn+1))+": "+first+"\n"), 0o644)
		}
	}
	term.turn++
	term.working = false
	return status("done", term.turn), nil
}

func (s *simHC) Send(_ context.Context, t, text string, _ bool, _ int) (hc.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	term := s.terms[t]
	if term == nil || term.killed {
		return hc.SendResult{}, &hc.Error{Kind: "finished", Message: "gone"}
	}
	s.sends[t] = append(s.sends[t], text)
	term.working = true
	term.task = strings.TrimPrefix(text, "## Task\n\n")
	return hc.SendResult{Started: true, RawStat: json.RawMessage(`"idle"`), Turn: term.turn}, nil
}

func (s *simHC) Read(_ context.Context, t string, _ int) (string, error) {
	return "line one\nSCREEN of " + t + "\n\n\n", nil
}

func (s *simHC) Kill(_ context.Context, t string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if term := s.terms[t]; term != nil {
		term.killed = true
	}
	return nil
}

func (s *simHC) Rm(_ context.Context, t string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if term := s.terms[t]; term != nil {
		term.removed = true
	}
	return nil
}

func (s *simHC) Get(_ context.Context, t string) (hc.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.others {
		if o.Address == t {
			return o, nil
		}
	}
	term := s.terms[t]
	if term == nil || term.removed {
		return hc.Info{}, &hc.Error{Kind: "not_found", Message: "no"}
	}
	if term.killed {
		return hc.Info{Address: t, State: "finished"}, nil
	}
	st := "idle"
	if term.working {
		st = "working"
	}
	return hc.Info{Address: t, State: "live", Status: json.RawMessage(`{"app":"omp","state":"` + st + `"}`), Labels: term.opts.Labels}, nil
}

func (s *simHC) List(_ context.Context, _ bool, _ map[string]string) ([]hc.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := append([]hc.Info{}, s.others...)
	for _, a := range s.order {
		term := s.terms[a]
		if term.killed {
			continue
		}
		out = append(out, hc.Info{Address: a, Name: term.opts.Name, Agent: "omp", State: "live", Where: term.opts.Labels["where"]})
	}
	return out, nil
}

type harness struct {
	t      *testing.T
	env    *Env
	sim    *simHC
	vars   map[string]string
	out    *bytes.Buffer
	errOut *bytes.Buffer
	execd  [][]string
	ids    []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, sim: newSim(), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	home, cwd := t.TempDir(), t.TempDir()
	h.vars = map[string]string{
		"FANOUT_HOME":            t.TempDir(),
		"CLAUDE_CODE_SESSION_ID": "fc31e7ff-1111-2222",
		"XDG_CONFIG_HOME":        "",
	}
	h.env = &Env{
		Stdout: h.out, Stderr: h.errOut, Stdin: strings.NewReader(""),
		Getenv: func(k string) string { return h.vars[k] },
		Home:   home, Cwd: cwd, HC: h.sim,
		Exec: func(argv []string) error { h.execd = append(h.execd, argv); return nil },
		Opts: orch.Options{RetryDelay: time.Millisecond, PollSeconds: 1, ReadyPoll: time.Second},
		NewID: func() string {
			if len(h.ids) == 0 {
				return "run001"
			}
			id := h.ids[0]
			h.ids = h.ids[1:]
			return id
		},
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.errOut.Reset()
	return Main(context.Background(), args, h.env)
}

func (h *harness) must(code int, args ...string) string {
	h.t.Helper()
	if got := h.run(args...); got != code {
		h.t.Fatalf("fanout %q: exit %d want %d\nstdout:\n%s\nstderr:\n%s", args, got, code, h.out, h.errOut)
	}
	return h.out.String()
}

func has(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("output does not contain %q:\n%s", sub, s)
		}
	}
}

func hasNot(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("output should not contain %q:\n%s", sub, s)
		}
	}
}
