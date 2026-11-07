package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/hc"
	"github.com/kingchan818/agent-tools/fanout/internal/store"
	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

func (h *harness) loadRun(ref string) *store.Run {
	h.t.Helper()
	r, err := h.env.store().Load(ref)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func TestMainBasics(t *testing.T) {
	h := newHarness(t)
	if h.run() != 2 || !strings.Contains(h.errOut.String(), "Usage: fanout <command>") {
		t.Fatal("no args: usage on stderr, exit 2")
	}
	for _, a := range []string{"help", "-h", "--help"} {
		has(t, h.must(0, a), "Usage: fanout <command>", "send RUN N MESSAGE")
	}
	for _, a := range []string{"version", "--version", "-V"} {
		if out := h.must(0, a); out != "fanout "+Version+"\n" {
			t.Fatalf("version: %q", out)
		}
	}
	if h.run("bogus") != 2 || !strings.Contains(h.errOut.String(), `unknown command "bogus"`) {
		t.Fatal("unknown command")
	}
	has(t, h.must(0, "skill"), "name: fanout", "fanout run --bg")
}

func TestRunHappyPath(t *testing.T) {
	h := newHarness(t)
	out := h.must(0, "run", "Review a.go\nmore detail", "Review b.go")
	has(t, out,
		"━━ [1/2] Review a.go · omp · done", "REPORT turn 1: Review a.go",
		"━━ [2/2] Review b.go · omp · done", "REPORT turn 1: Review b.go",
		"fanout: run run001: 2 done · reports in ")
	hasNot(t, out, "⚠", "(no report)")
	has(t, h.errOut.String(), `fanout: run run001 · 2 worker(s) · thread "claude fc31e7ff"`,
		"fanout: [1/2] starting  fo-run001-1 (tfo-run001-1) Review a.go", "fanout: [2/2] done")
	// Each state is printed once per worker even though events repeat.
	if n := strings.Count(h.errOut.String(), "[1/2] done"); n != 1 {
		t.Fatalf("done printed %d times", n)
	}
	for _, a := range h.sim.order {
		if !h.sim.terms[a].removed {
			t.Errorf("%s not cleaned up", a)
		}
		o := h.sim.terms[a].opts
		if o.Cwd != h.env.Cwd || o.Labels["claude.session"] != "fc31e7ff-1111-2222" || !o.NoParent || !o.NoNotify {
			t.Errorf("launch opts %+v", o)
		}
	}
	r := h.loadRun("last")
	if r.Thread.Kind != "claude" || r.Cwd != h.env.Cwd || r.Keep || len(r.Workers) != 2 {
		t.Fatalf("run %+v", r)
	}
}

func TestRunJSON(t *testing.T) {
	h := newHarness(t)
	out := h.must(0, "run", "--json", "Review a.go")
	var v runView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if v.Run != "run001" || v.Summary != "1 done" || len(v.Workers) != 1 || v.Workers[0].State != "done" ||
		!strings.HasPrefix(v.Workers[0].Report, "REPORT turn 1") || v.Workers[0].Agent != "omp" || !v.Workers[0].Closed {
		t.Fatalf("%+v", v)
	}
	if h.errOut.Len() != 0 {
		t.Fatalf("--json must keep stderr quiet: %s", h.errOut)
	}
}

func TestRunNoReport(t *testing.T) {
	h := newHarness(t)
	out := h.must(1, "run", "NOREPORT please", "fine")
	has(t, out, "━━ [1/2] NOREPORT please · omp · no_report", "(no report)", "⚠ turn ended without a report",
		"--- last lines of its screen ---", "SCREEN of tfo-run001-1", "1 done, 1 no_report")
	if strings.Contains(out, "SCREEN of tfo-run001-1\n\n\n") {
		t.Fatal("trailing blank lines should be trimmed")
	}
	if h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("a worker without a report must stay open")
	}
}

func TestRunTaskSources(t *testing.T) {
	h := newHarness(t)
	has(t, h.must(0, "run", "--each", "Review {} only", "a.go", "b.go"), "[1/2] a.go · omp", "REPORT turn 1: Review a.go only")

	h = newHarness(t)
	os.WriteFile(filepath.Join(h.env.Cwd, "tasks.md"), []byte("First task\n---\nSecond task\n"), 0o644)
	has(t, h.must(0, "run", "-f", "tasks.md", "Third task"), "[1/3] First task", "[2/3] Second task", "[3/3] Third task")

	h = newHarness(t)
	h.env.Stdin = strings.NewReader(`[{"description":"desc x","prompt":"Do x"}, "Do y"]`)
	has(t, h.must(0, "run", "--file", "-"), "[1/2] desc x", "[2/2] Do y")

	h = newHarness(t)
	abs := filepath.Join(t.TempDir(), "t.txt")
	os.WriteFile(abs, []byte("only one\n"), 0o644)
	has(t, h.must(0, "run", "-f", abs), "[1/1] only one")
}

func TestRunUsageErrors(t *testing.T) {
	cases := [][]string{
		{"run"},
		{"run", "--each", "no placeholder", "a"},
		{"run", "--each", "x {}", "-f", "f", "a"},
		{"run", "-j", "-1", "task"},
		{"run", "--nope", "task"},
		{"run", "-f", "-"},
	}
	for _, c := range cases {
		h := newHarness(t)
		if code := h.run(c...); code != 2 {
			t.Errorf("%q: exit %d, stderr %s", c, code, h.errOut)
		}
		has(t, h.errOut.String(), "Usage: fanout run")
	}
	h := newHarness(t)
	has(t, h.must(0, "run", "-h"), "Usage: fanout run", "--worktree")
}

func TestRunErrors(t *testing.T) {
	h := newHarness(t)
	if h.run("run", "-f", "missing.md") != 1 || !strings.Contains(h.errOut.String(), "missing.md") {
		t.Fatalf("missing file: %s", h.errOut)
	}
	if h.run("run", "-C", "nowhere", "x") != 1 || !strings.Contains(h.errOut.String(), "nowhere does not exist") {
		t.Fatalf("bad cwd: %s", h.errOut)
	}
	if h.run("run", "-f", "-", "x") != 0 {
		t.Fatalf("empty stdin plus an arg is fine: %s", h.errOut)
	}
	h = newHarness(t)
	h.env.Stdin = strings.NewReader("[{")
	if h.run("run", "-f", "-") != 1 {
		t.Fatal("bad json")
	}
	h = newHarness(t)
	if h.run("run", "-a", "ghost", "x") != 1 || !strings.Contains(h.errOut.String(), `unknown agent "ghost"`) {
		t.Fatalf("unknown agent: %s", h.errOut)
	}
	if runs, _ := h.env.store().List(); len(runs) != 0 {
		t.Fatal("a failed plan must not leave a run behind")
	}
	h = newHarness(t)
	h.vars["FANOUT_HOME"] = filepath.Join(h.env.Cwd, "file")
	os.WriteFile(h.vars["FANOUT_HOME"], nil, 0o644)
	if h.run("run", "x") != 1 {
		t.Fatal("unusable state dir")
	}
}

func TestRunCwdAndFlagsAnywhere(t *testing.T) {
	h := newHarness(t)
	os.Mkdir(filepath.Join(h.env.Cwd, "sub"), 0o755)
	h.must(0, "run", "task one", "-C", "sub", "-m", "fast", "--thinking", "low", "--yolo", "--", "-literal task")
	r := h.loadRun("last")
	if len(r.Workers) != 2 || r.Workers[1].Task != "-literal task" {
		t.Fatalf("workers %+v", r.Workers)
	}
	w := r.Workers[0]
	if w.Cwd != filepath.Join(h.env.Cwd, "sub") {
		t.Fatalf("cwd %s", w.Cwd)
	}
	if strings.Join(w.Argv, " ") != "omp --model fast --thinking low --auto-approve --add-dir "+r.ResultsDir() {
		t.Fatalf("argv %q", w.Argv)
	}
	h = newHarness(t)
	abs := t.TempDir()
	h.must(0, "run", "--cwd", abs, "x")
	if h.loadRun("last").Workers[0].Cwd != abs {
		t.Fatal("absolute cwd")
	}
}

func TestRunConcurrencyFlag(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "-j", "1", "a", "b", "c")
	if s := h.loadRun("last").Summary(); s != "3 done" {
		t.Fatal(s)
	}
}

func TestRunTimeout(t *testing.T) {
	h := newHarness(t)
	out := h.must(1, "run", "--timeout", "30ms", "HANG forever", "quick")
	has(t, out, "[1/2] HANG forever · omp · timeout", "(no report)", "fanout wait run001", "1 done, 1 timeout")
	if h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("timed-out workers keep running")
	}
}

func TestRunBlocked(t *testing.T) {
	h := newHarness(t)
	out := h.must(1, "run", "BLOCK me")
	has(t, out, "· blocked ·", "⚠ agent is waiting for a person", "hc attach tfo-run001-1")
}

func TestBackgroundRunThenWait(t *testing.T) {
	h := newHarness(t)
	out := h.must(0, "run", "--bg", "Review a.go", "Review b.go")
	has(t, out, "fanout: started run run001 (2 running)", "#  TERMINAL", "tfo-run001-1", "running", "fanout wait run001")
	hasNot(t, out, "REPORT")

	status := h.must(0, "status")
	has(t, status, "run run001", `thread "claude fc31e7ff"`, "2 running", "LIVE", "working")

	waited := h.must(0, "wait", "run001")
	has(t, waited, "REPORT turn 1: Review a.go", "REPORT turn 1: Review b.go", "2 done")
	if !h.sim.terms["tfo-run001-1"].removed {
		t.Fatal("wait should clean up finished workers")
	}
	st := h.must(0, "status", "--json")
	has(t, st, `"closed": true`)
	hasNot(t, st, `"live"`)
	has(t, h.must(0, "status"), "(closed)")
}

func TestBackgroundJSONAndIncomplete(t *testing.T) {
	h := newHarness(t)
	out := h.must(0, "run", "--bg", "--json", "a")
	var v runView
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Workers[0].State != "running" || v.Workers[0].Report != "" {
		t.Fatalf("%v %+v", err, v)
	}
	h = newHarness(t)
	h.sim.launchErr = &hc.Error{Kind: "spawn_failed", Message: "no omp"}
	out = h.must(1, "run", "--bg", "a")
	has(t, out, "started run run001 (1 failed)")
}

func TestBackgroundNotifiesInsideHC(t *testing.T) {
	h := newHarness(t)
	h.vars["HC_TERMINAL_ID"] = "t42"
	h.must(0, "run", "--bg", "a")
	o := h.sim.terms["tfo-run001-1"].opts
	if o.NoParent || o.NoNotify || !o.OutliveParent || o.Labels["fanout.caller"] != "t42" {
		t.Fatalf("%+v", o)
	}
	h = newHarness(t)
	h.vars["HC_TERMINAL_ID"] = "t42"
	h.must(0, "run", "a")
	if o := h.sim.terms["tfo-run001-1"].opts; !o.NoNotify || o.NoParent {
		t.Fatalf("blocking run inside hc: %+v", o)
	}
}

func TestStatusShowsGoneAndFinished(t *testing.T) {
	h := newHarness(t)
	h.ids = []string{"aaa111"}
	h.must(0, "run", "--bg", "--keep", "a", "b", "c")
	h.sim.terms["tfo-aaa111-1"].removed = true
	h.sim.terms["tfo-aaa111-2"].killed = true
	out := h.must(0, "status", "aaa")
	has(t, out, "gone", "finished", "working")
	if h.run("status", "a", "b") != 2 {
		t.Fatal("too many args")
	}
	if h.run("status", "zzz") != 1 || !strings.Contains(h.errOut.String(), `no run "zzz"`) {
		t.Fatal("missing run")
	}
}

func TestKeepAndSend(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--keep", "Review a.go")
	if h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("--keep must keep the terminal")
	}
	out := h.must(0, "send", "last", "1", "Also", "check b.go")
	has(t, out, "REPORT turn 2: Also check b.go", "━━ [1/1]")
	sent := h.sim.sends["tfo-run001-1"]
	if len(sent) != 2 || !strings.HasPrefix(sent[1], "Also check b.go\n\n(When you are done") {
		t.Fatalf("sent %q", sent)
	}
	if h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("send keeps the worker open by default")
	}
	prev, _ := os.ReadFile(filepath.Join(h.loadRun("last").ResultsDir(), "1.prev.md"))
	if !strings.Contains(string(prev), "REPORT turn 1") {
		t.Fatalf("prev %q", prev)
	}
	h.must(0, "send", "--close", "run001", "1", "last one")
	if !h.sim.terms["tfo-run001-1"].removed {
		t.Fatal("--close closes it")
	}
	if h.run("send", "run001", "1", "again") != 1 || !strings.Contains(h.errOut.String(), "--keep") {
		t.Fatalf("closed worker: %s", h.errOut)
	}
}

func TestBgKeepSurvivesWait(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "--keep", "a")
	h.must(0, "wait")
	if h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("run --bg --keep must stay open through wait")
	}
	if !h.loadRun("last").Keep {
		t.Fatal("keep must be saved")
	}
	h.must(0, "send", "last", "1", "more")

	h = newHarness(t)
	h.must(0, "run", "--bg", "a")
	h.must(0, "wait", "--keep")
	if h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("wait --keep")
	}
}

func TestSendErrors(t *testing.T) {
	h := newHarness(t)
	if h.run("send", "last", "1") != 2 {
		t.Fatal("missing message")
	}
	if h.run("send", "last", "1", "x") != 1 || !strings.Contains(h.errOut.String(), "no runs yet") {
		t.Fatal("no runs")
	}
	h.must(0, "run", "--keep", "a")
	if h.run("send", "last", "x", "msg") != 2 || !strings.Contains(h.errOut.String(), `worker must be a number, got "x"`) {
		t.Fatalf("bad index: %s", h.errOut)
	}
	if h.run("send", "last", "9", "msg") != 1 || !strings.Contains(h.errOut.String(), "no worker 9") {
		t.Fatal("missing worker")
	}
	h.sim.terms["tfo-run001-1"].task = "NOREPORT"
	if code := h.run("send", "last", "1", "NOREPORT now"); code != 1 || !strings.Contains(h.out.String(), "no_report") {
		t.Fatalf("follow-up without report: %d %s", code, h.out)
	}
}

func TestResult(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "first", "second")
	out := h.must(0, "result")
	has(t, out, "first", "second")
	out = h.must(0, "result", "run001", "2")
	has(t, out, "━━ [2/2] second")
	hasNot(t, out, "[1/2]")
	var v runView
	json.Unmarshal([]byte(h.must(0, "result", "last", "1", "--json")), &v)
	if len(v.Workers) != 1 || v.Workers[0].Index != 1 {
		t.Fatalf("%+v", v)
	}
	if h.run("result", "run001", "3") != 1 || h.run("result", "a", "b", "c") != 2 || h.run("result", "run001", "z") != 2 {
		t.Fatal("bad result args")
	}
}

func TestResultStillWorking(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "a")
	out := h.must(1, "result")
	has(t, out, "(still working)")
}

func TestLs(t *testing.T) {
	h := newHarness(t)
	if out := h.must(0, "ls"); out != "no runs\n" {
		t.Fatalf("%q", out)
	}
	h.ids = []string{"aaa111", "bbb222", "ccc333"}
	h.must(0, "run", "a")
	h.vars["CLAUDE_CODE_SESSION_ID"] = "other-session"
	h.must(0, "run", "b", "c")
	h.vars["CLAUDE_CODE_SESSION_ID"] = ""
	h.vars["FANOUT_THREAD"] = "manual"
	h.must(0, "run", "d")

	out := h.must(0, "ls")
	has(t, out, "RUN", "aaa111", "bbb222", "ccc333", "claude fc31e7ff", "claude other-se", "manual", "2 done")
	mine := h.must(0, "ls", "--mine")
	has(t, mine, "ccc333")
	hasNot(t, mine, "aaa111", "bbb222")
	h.vars["FANOUT_THREAD"] = ""
	h.vars["CLAUDE_CODE_SESSION_ID"] = "fc31e7ff-1111-2222"
	mine = h.must(0, "ls", "--mine")
	has(t, mine, "aaa111")
	hasNot(t, mine, "bbb222", "ccc333")

	var rows []map[string]any
	json.Unmarshal([]byte(h.must(0, "ls", "--json", "-n", "2")), &rows)
	if len(rows) != 2 {
		t.Fatalf("limit: %v", rows)
	}
	json.Unmarshal([]byte(h.must(0, "ls", "--json", "-n", "0")), &rows)
	if len(rows) != 3 {
		t.Fatalf("no limit: %v", rows)
	}
	if h.run("ls", "extra") != 2 {
		t.Fatal("ls takes no args")
	}
	if out := h.must(0, "ls", "--json", "--mine"); !strings.Contains(out, "aaa111") {
		t.Fatal(out)
	}
	h2 := newHarness(t)
	if out := h2.must(0, "ls", "--json"); out != "[]\n" {
		t.Fatalf("empty json %q", out)
	}
}

func TestRank(t *testing.T) {
	if rank(true) != 0 || rank(false) != 1 {
		t.Fatal("rank")
	}
}

func TestThreadsSameTitleDifferentSessions(t *testing.T) {
	h := newHarness(t)
	h.vars["FANOUT_THREAD"] = "shared"
	h.sim.others = []hc.Info{
		{Address: "t8", Labels: map[string]string{"fanout.thread": "shared", "claude.session": "s2"}},
		{Address: "t9", Labels: map[string]string{"fanout.thread": "shared", "claude.session": "s1"}},
	}
	var groups []threadGroup
	json.Unmarshal([]byte(h.must(0, "threads", "--json")), &groups)
	if len(groups) != 2 || groups[0].Session != "s1" || groups[1].Session != "s2" {
		t.Fatalf("ties break by session: %+v", groups)
	}
}

func TestSameThread(t *testing.T) {
	cases := []struct {
		a, b thread.Info
		want bool
	}{
		{thread.Info{SessionID: "x"}, thread.Info{SessionID: "x"}, true},
		{thread.Info{SessionID: "x"}, thread.Info{SessionID: "y"}, false},
		{thread.Info{SessionID: "x", Kind: "k"}, thread.Info{Kind: "k"}, false},
		{thread.Info{Kind: "env", Title: "a"}, thread.Info{Kind: "env", Title: "a"}, true},
		{thread.Info{Kind: "env", Title: "a"}, thread.Info{Kind: "env", Title: "b"}, false},
		{thread.Info{Kind: "env", Title: "a"}, thread.Info{Kind: "hc", Title: "a"}, false},
	}
	for _, c := range cases {
		if sameThread(c.a, c.b) != c.want {
			t.Errorf("%+v %+v", c.a, c.b)
		}
	}
}

func TestAttach(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--keep", "a")
	h.must(0, "attach", "last", "1")
	if len(h.execd) != 1 || strings.Join(h.execd[0], " ") != "hc attach tfo-run001-1" {
		t.Fatalf("exec %q", h.execd)
	}
	h.must(0, "kill", "last")
	if h.run("attach", "last", "1") != 1 || !strings.Contains(h.errOut.String(), "no open terminal") {
		t.Fatal("closed")
	}
	if h.run("attach", "last") != 2 || h.run("attach", "zzz", "1") != 1 || h.run("attach", "last", "x") != 2 {
		t.Fatal("bad attach args")
	}
	h.env.Exec = func([]string) error { return errors.New("exec failed") }
	h2 := newHarness(t)
	h2.env.Exec = h.env.Exec
	h2.must(0, "run", "--keep", "a")
	if h2.run("attach", "last", "1") != 1 || !strings.Contains(h2.errOut.String(), "exec failed") {
		t.Fatal("exec error")
	}
}

func TestKillAndRm(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "a", "b")
	out := h.must(0, "kill", "last", "2")
	has(t, out, "closed 1 terminal(s) of run run001")
	if !h.sim.terms["tfo-run001-2"].removed || h.sim.terms["tfo-run001-1"].killed {
		t.Fatal("kill one")
	}
	r := h.loadRun("last")
	if r.Workers[1].State != store.Exited || !r.Workers[1].Closed {
		t.Fatalf("%+v", r.Workers[1])
	}
	has(t, h.must(0, "kill", "last"), "closed 1 terminal(s)")
	has(t, h.must(0, "kill", "last"), "closed 0 terminal(s)")
	if h.run("kill") != 2 || h.run("kill", "last", "x") != 2 || h.run("kill", "zzz") != 1 {
		t.Fatal("bad kill args")
	}
	has(t, h.must(0, "rm", "run001"), "removed run run001")
	if runs, _ := h.env.store().List(); len(runs) != 0 {
		t.Fatal("run dir should be gone")
	}
	if h.run("rm") != 2 || h.run("rm", "zzz") != 1 {
		t.Fatal("bad rm args")
	}
}

func TestKillReportsErrors(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "a", "b")
	h.env.HC = &failingHC{simHC: h.sim}
	if h.run("kill", "last") != 1 || !strings.Contains(h.errOut.String(), "worker 1: hc: x: kill failed") ||
		!strings.Contains(h.errOut.String(), "worker 2: hc: x: kill failed") {
		t.Fatalf("every failure must be reported: %s", h.errOut)
	}
	has(t, h.out.String(), "closed 0 terminal(s)")
	if h.run("rm", "last") != 1 {
		t.Fatal("rm must stop when closing fails")
	}
	if _, err := h.env.store().Load("last"); err != nil {
		t.Fatal("run must survive a failed rm")
	}
}

type failingHC struct{ *simHC }

func (f *failingHC) Kill(context.Context, string) error {
	return &hc.Error{Kind: "x", Message: "kill failed"}
}

func TestRmRemovesWorktrees(t *testing.T) {
	h := newHarness(t)
	var calls []string
	h.env.Git = func(_ context.Context, dir string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[1] == "remove" {
			return "", errors.New("locked")
		}
		return "", nil
	}
	h.must(0, "run", "--worktree", "a")
	has(t, h.out.String(), "(branch fanout/run001-1 in ")
	h.must(0, "rm", "last")
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "worktree add -b fanout/run001-1") || !strings.HasPrefix(calls[1], "worktree remove --force") {
		t.Fatalf("git calls %q", calls)
	}
	has(t, h.errOut.String(), "warning: worker 1: locked")
}

func TestAgents(t *testing.T) {
	h := newHarness(t)
	user := filepath.Join(h.env.Home, ".config", "fanout", "agents")
	os.MkdirAll(user, 0o755)
	os.WriteFile(filepath.Join(user, "reviewer.md"), []byte("---\ndescription: user reviewer\nmodel: fast\n---\nReview."), 0o644)
	proj := filepath.Join(h.env.Cwd, ".fanout", "agents")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "fixer.md"), []byte("---\nbackend: pi\ndescription: project fixer\n---\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "broken.md"), []byte("---\nbackend: nope\n---\n"), 0o644)

	out := h.must(0, "agents")
	has(t, out, "NAME", "omp", "builtin", "reviewer", "fast", "~/.config/fanout/agents/reviewer.md", "fixer", "pi", "project fixer")
	has(t, h.errOut.String(), "warning:", "broken.md")
	var defs []map[string]any
	json.Unmarshal([]byte(h.must(0, "agents", "--json")), &defs)
	if len(defs) != 6 {
		t.Fatalf("defs %d", len(defs))
	}
	if h.run("agents", "x") != 2 {
		t.Fatal("agents takes no args")
	}

	h.vars["XDG_CONFIG_HOME"] = filepath.Join(h.env.Home, "elsewhere")
	hasNot(t, h.must(0, "agents"), "reviewer")

	// A run with a project agent type uses its backend and instructions.
	h.vars["XDG_CONFIG_HOME"] = ""
	h.must(0, "run", "-a", "reviewer", "Check x.go")
	w := h.loadRun("last").Workers[0]
	if w.Agent != "reviewer" || strings.Join(w.Argv[:3], " ") != "omp --model fast" || !strings.HasPrefix(w.Prompt, "Review.\n\n## Task") {
		t.Fatalf("%+v", w)
	}
	has(t, h.errOut.String(), "warning:")
}

func TestThreads(t *testing.T) {
	h := newHarness(t)
	h.ids = []string{"aaa111", "bbb222"}
	h.must(0, "run", "--bg", "--keep", "Mine")
	h.vars["CLAUDE_CODE_SESSION_ID"] = "zzz-other"
	h.must(0, "run", "--bg", "--keep", "Theirs")
	h.vars["CLAUDE_CODE_SESSION_ID"] = "fc31e7ff-1111-2222"
	h.sim.others = []hc.Info{{Address: "t1", Agent: "claude-code", State: "live", Status: []byte(`{"app":"claude-code","state":"idle"}`), Where: "~ / 1"}}

	out := h.must(0, "threads")
	mine := strings.Index(out, "◆ claude fc31e7ff   ← this thread")
	theirs := strings.Index(out, "◆ claude zzz-othe")
	other := strings.Index(out, "◆ (not started by fanout)")
	if mine < 0 || theirs < 0 || other < 0 || !(mine < theirs && theirs < other) {
		t.Fatalf("order/markers wrong:\n%s", out)
	}
	has(t, out, "tfo-aaa111-1", "run aaa111 #1", "◆ claude fc31e7ff › Mine", "t1", "claude-code", "idle")

	var groups []threadGroup
	json.Unmarshal([]byte(h.must(0, "threads", "--json")), &groups)
	if len(groups) != 3 || !groups[0].Current || groups[0].Session != "fc31e7ff-1111-2222" || groups[1].Current ||
		groups[0].Terminals[0].Run != "aaa111" || groups[0].Terminals[0].Status != "working" || groups[2].Terminals[0].Status != "idle" {
		t.Fatalf("%+v", groups)
	}
	if h.run("threads", "x") != 2 {
		t.Fatal("threads takes no args")
	}
}

func TestThreadsByTitleAndEmpty(t *testing.T) {
	h := newHarness(t)
	if out := h.must(0, "threads"); out != "no live terminals\n" {
		t.Fatalf("%q", out)
	}
	if out := h.must(0, "threads", "--json"); out != "[]\n" {
		t.Fatalf("%q", out)
	}
	h.vars["CLAUDE_CODE_SESSION_ID"] = ""
	h.vars["FANOUT_THREAD"] = "release prep"
	h.must(0, "run", "--bg", "a")
	has(t, h.must(0, "threads"), "◆ release prep   ← this thread")
	h.vars["FANOUT_THREAD"] = "other"
	hasNot(t, h.must(0, "threads"), "← this thread")
	h.sim.listErr = errors.New("hc down")
	if h.run("threads") != 1 || !strings.Contains(h.errOut.String(), "hc down") {
		t.Fatal("list error")
	}
}

func TestInstallSkill(t *testing.T) {
	h := newHarness(t)
	has(t, h.must(0, "install-skill"), filepath.Join(h.env.Home, ".claude", "skills", "fanout", "SKILL.md"))
	b, _ := os.ReadFile(filepath.Join(h.env.Home, ".claude", "skills", "fanout", "SKILL.md"))
	if string(b) != skillDoc {
		t.Fatal("skill content")
	}
	dir := t.TempDir()
	h.must(0, "install-skill", dir)
	if _, err := os.Stat(filepath.Join(dir, "fanout", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if h.run("install-skill", "a", "b") != 2 {
		t.Fatal("too many args")
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	if h.run("install-skill", file) != 1 {
		t.Fatal("dir is a file")
	}
}

func TestWaitArgs(t *testing.T) {
	h := newHarness(t)
	if h.run("wait", "a", "b") != 2 || h.run("wait") != 1 || h.run("wait", "--bogus") != 2 {
		t.Fatal("wait args")
	}
	has(t, h.must(0, "wait", "-h"), "Usage: fanout wait")
}

func TestSubcommandHelp(t *testing.T) {
	h := newHarness(t)
	for cmd, want := range map[string]string{
		"result": "fanout result", "status": "fanout status", "ls": "fanout ls", "send": "fanout send",
		"attach": "fanout attach", "kill": "fanout kill", "rm": "fanout rm", "agents": "fanout agents",
		"threads": "fanout threads", "install-skill": "fanout install-skill",
	} {
		has(t, h.must(0, cmd, "--help"), "Usage: "+want)
		if h.run(cmd, "--bogus-flag") != 2 {
			t.Errorf("%s: bad flag should exit 2", cmd)
		}
	}
}

func TestTailLines(t *testing.T) {
	if got := tailLines("a  \nb\t\nc\n\n  \n", 2); got != "b\nc" {
		t.Fatalf("%q", got)
	}
	if got := tailLines("one", 5); got != "one" {
		t.Fatalf("%q", got)
	}
	if nonEmpty("", "d") != "d" || nonEmpty("x", "d") != "x" {
		t.Fatal("nonEmpty")
	}
}

func TestReportOfMissingEverything(t *testing.T) {
	r, s := reportOf(&store.Worker{ResultPath: "/nope/1.md", ScreenPath: "/nope/1.screen.txt"})
	if r != "" || s != "" {
		t.Fatal("nothing to show")
	}
}

func TestWantArgs(t *testing.T) {
	if wantArgs([]string{"a"}, 0, -1, "") != nil {
		t.Fatal("unbounded")
	}
	err := wantArgs([]string{"a", "b"}, 0, 1, "u")
	var u usageErr
	if !errors.As(err, &u) || u.msg != `unexpected argument "b"` || u.usage != "u" {
		t.Fatalf("%v", err)
	}
	if !errors.As(wantArgs(nil, 1, 1, "u"), &u) || u.msg != "missing arguments" {
		t.Fatal("missing")
	}
	if (errHelp{}).Error() != "help requested" {
		t.Fatal("errHelp")
	}
}

func (h *harness) lastDeadline() time.Duration {
	h.t.Helper()
	if len(h.sim.deadlines) == 0 {
		h.t.Fatal("no waits recorded")
	}
	return h.sim.deadlines[len(h.sim.deadlines)-1]
}

func near30m(d time.Duration) bool { return d > 29*time.Minute && d <= 30*time.Minute }

func TestDefaultTimeouts(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--keep", "a")
	if d := h.lastDeadline(); !near30m(d) {
		t.Fatalf("run default timeout: %v", d)
	}
	h.must(0, "send", "last", "1", "more")
	if d := h.lastDeadline(); !near30m(d) {
		t.Fatalf("send default timeout: %v", d)
	}
	h.ids = []string{"bbb222"}
	h.must(0, "run", "--bg", "b")
	h.must(0, "wait")
	if d := h.lastDeadline(); !near30m(d) {
		t.Fatalf("wait default timeout: %v", d)
	}
	h.ids = []string{"ccc333"}
	h.must(0, "run", "--timeout", "0", "c")
	if d := h.lastDeadline(); d != -1 {
		t.Fatalf("--timeout 0 means no deadline: %v", d)
	}
	h.ids = []string{"ddd444"}
	h.must(0, "run", "--timeout", "5m", "d")
	if d := h.lastDeadline(); d <= 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("--timeout 5m: %v", d)
	}
}

func TestBlankReportShowsScreen(t *testing.T) {
	dir := t.TempDir()
	w := &store.Worker{ResultPath: filepath.Join(dir, "1.md"), ScreenPath: filepath.Join(dir, "1.screen.txt")}
	os.WriteFile(w.ResultPath, []byte(" \n\t\n"), 0o644)
	os.WriteFile(w.ScreenPath, []byte("screen\n"), 0o644)
	r, s := reportOf(w)
	if r != "" || s != "screen" {
		t.Fatalf("report %q screen %q", r, s)
	}
	os.WriteFile(w.ResultPath, []byte("  real  \n"), 0o644)
	if r, s := reportOf(w); r != "real" || s != "" {
		t.Fatalf("report %q screen %q", r, s)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestJSONWriteErrors(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "a")
	h.env.Stdout = errWriter{}
	for _, args := range [][]string{{"result", "--json"}, {"status", "--json"}, {"ls", "--json"}, {"agents", "--json"}, {"threads", "--json"}} {
		h.errOut.Reset()
		if code := Main(context.Background(), args, h.env); code != 1 || !strings.Contains(h.errOut.String(), "disk full") {
			t.Errorf("%q: exit %d stderr %q", args, code, h.errOut)
		}
	}
}

func TestElapsedIsShown(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--keep", "a")
	r := h.loadRun("last")
	r.Workers[0].StartedAt = r.CreatedAt
	r.Workers[0].EndedAt = r.CreatedAt.Add(42 * time.Second)
	if err := h.env.store().Save(r); err != nil {
		t.Fatal(err)
	}
	has(t, h.must(0, "result"), "· omp · done · 42s ━━")
	has(t, h.must(0, "status"), " 42s ")
}

func TestStillWorkingHasNoWarning(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "a")
	hasNot(t, h.must(1, "result"), "⚠")
}

func TestStatusAfterClosingFirstWorker(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "a", "b")
	h.must(0, "kill", "last", "1")
	has(t, h.must(0, "status"), "tfo-run001-1 (closed)", "working")
	var v runView
	json.Unmarshal([]byte(h.must(0, "status", "--json")), &v)
	if v.Workers[0].Live != "" || v.Workers[1].Live != "working" {
		t.Fatalf("%+v", v.Workers)
	}
}

func TestKillSkipsClosedThenClosesRest(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--bg", "a", "b")
	has(t, h.must(0, "kill", "last", "1"), "closed 1 terminal(s)")
	has(t, h.must(0, "kill", "last"), "closed 1 terminal(s)")
	if !h.sim.terms["tfo-run001-2"].removed {
		t.Fatal("second worker must be closed")
	}
}

func TestRmSkipsWorkersWithoutWorktree(t *testing.T) {
	h := newHarness(t)
	var removed []string
	h.env.Git = func(_ context.Context, dir string, args ...string) (string, error) {
		if args[1] == "remove" {
			removed = append(removed, args[3])
		}
		return "", nil
	}
	h.must(0, "run", "--worktree", "a", "b")
	r := h.loadRun("last")
	keep := r.Workers[1].Worktree
	r.Workers[0].Worktree = ""
	h.env.store().Save(r)
	h.must(0, "rm", "last")
	if len(removed) != 1 || removed[0] != keep {
		t.Fatalf("removed %q", removed)
	}
}

func TestAgentsShowsAbsolutePathOutsideHome(t *testing.T) {
	h := newHarness(t)
	proj := filepath.Join(h.env.Cwd, ".fanout", "agents")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "fixer.md"), []byte("Fix."), 0o644)
	out := h.must(0, "agents")
	has(t, out, filepath.Join(proj, "fixer.md"))
	hasNot(t, out, "~/..")
}

func TestThreadsOrderAndLayout(t *testing.T) {
	h := newHarness(t)
	h.ids = []string{"aaa111", "bbb222", "ccc333"}
	h.sim.others = []hc.Info{{Address: "t1", Agent: "claude-code", State: "live"}}
	h.vars["CLAUDE_CODE_SESSION_ID"] = "zzz-other"
	h.must(0, "run", "--bg", "Z")
	h.vars["CLAUDE_CODE_SESSION_ID"] = "fc31e7ff-1111-2222"
	h.must(0, "run", "--bg", "Mine")
	h.vars["CLAUDE_CODE_SESSION_ID"] = "aaa-third"
	h.must(0, "run", "--bg", "A")
	h.vars["CLAUDE_CODE_SESSION_ID"] = "fc31e7ff-1111-2222"
	out := h.must(0, "threads")
	if !strings.HasPrefix(out, "◆ claude fc31e7ff   ← this thread\n") {
		t.Fatalf("current thread first, no leading blank line:\n%s", out)
	}
	a, z, other := strings.Index(out, "◆ claude aaa-thir"), strings.Index(out, "◆ claude zzz-othe"), strings.Index(out, "◆ (not started")
	if !(a > 0 && a < z && z < other) {
		t.Fatalf("other threads sorted by name, unmanaged last:\n%s", out)
	}
	if strings.Count(out, "\n\n◆") != 3 {
		t.Fatalf("one blank line between groups:\n%s", out)
	}
}

func TestDoubleDashKeepsFlagLikeTasks(t *testing.T) {
	h := newHarness(t)
	h.must(0, "run", "--", "-x one", "-y two")
	r := h.loadRun("last")
	if len(r.Workers) != 2 || r.Workers[0].Task != "-x one" || r.Workers[1].Task != "-y two" {
		t.Fatalf("%+v", r.Workers)
	}
}

func TestSmallHelpers(t *testing.T) {
	if (usageErr{msg: "m"}).Error() != "m" {
		t.Fatal("usageErr")
	}
	e := &Env{}
	if time.Since(e.now()) > time.Second {
		t.Fatal("real clock")
	}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e.Now = func() time.Time { return fixed }
	if !e.now().Equal(fixed) {
		t.Fatal("injected clock")
	}
}

func TestRunUsesStoreNewIDByDefault(t *testing.T) {
	h := newHarness(t)
	h.env.NewID = nil
	h.must(0, "run", "a")
	r := h.loadRun("last")
	if len(r.ID) != 6 || r.ID == "run001" {
		t.Fatalf("id %q", r.ID)
	}
}
