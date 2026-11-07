package hc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type call struct {
	stdout, stderr string
	code           int
	err            error
}

type fakeRunner struct {
	got   [][]string
	resp  []call
	index int
}

func (f *fakeRunner) Run(_ context.Context, args []string) ([]byte, []byte, int, error) {
	f.got = append(f.got, args)
	if f.index >= len(f.resp) {
		return nil, nil, 0, nil
	}
	r := f.resp[f.index]
	f.index++
	return []byte(r.stdout), []byte(r.stderr), r.code, r.err
}

func client(resp ...call) (*Client, *fakeRunner) {
	f := &fakeRunner{resp: resp}
	return &Client{R: f}, f
}

func TestLaunchArgsFull(t *testing.T) {
	got := LaunchArgs(LaunchOpts{
		Name: "fo-a-1", Cwd: "/repo",
		Labels:   map[string]string{"z": "1", "a": "2"},
		NoParent: true, NoNotify: true, OutliveParent: true,
		Argv: []string{"omp", "--model", "x"},
	})
	want := []string{"run", "--json", "--bg", "--name", "fo-a-1", "--cwd", "/repo",
		"--label", "a=2", "--label", "z=1", "--no-parent", "--no-notify", "--outlive-parent",
		"--", "omp", "--model", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestLaunchArgsMinimalAndParent(t *testing.T) {
	got := LaunchArgs(LaunchOpts{Argv: []string{"pi"}})
	want := []string{"run", "--json", "--bg", "--", "pi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	got = LaunchArgs(LaunchOpts{Parent: "t3", Argv: []string{"pi"}})
	want = []string{"run", "--json", "--bg", "--parent", "t3", "--", "pi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	// NoParent wins over Parent.
	got = LaunchArgs(LaunchOpts{Parent: "t3", NoParent: true, Argv: []string{"pi"}})
	want = []string{"run", "--json", "--bg", "--no-parent", "--", "pi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLaunchParsesTerminal(t *testing.T) {
	c, f := client(call{stdout: `{"address":"t5","agent":"omp","cwd":"/tmp","name":"probe","state":"live","turn":0,"where":"w"}` + "\n"})
	term, err := c.Launch(context.Background(), LaunchOpts{Argv: []string{"omp"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Terminal{Address: "t5", Agent: "omp", Cwd: "/tmp", Name: "probe", State: "live", Where: "w"}
	if term != want {
		t.Fatalf("got %+v", term)
	}
	if f.got[0][0] != "run" {
		t.Fatalf("args %q", f.got[0])
	}
}

func TestLaunchEmptyArgv(t *testing.T) {
	c, f := client()
	if _, err := c.Launch(context.Background(), LaunchOpts{}); err == nil {
		t.Fatal("want error")
	}
	if len(f.got) != 0 {
		t.Fatal("must not run hc")
	}
}

func TestErrorParsing(t *testing.T) {
	c, _ := client(call{stderr: `{"error":{"hint":"hc read t4","kind":"finished","message":"t4 has finished"}}`, code: 11})
	_, err := c.Get(context.Background(), "t4")
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("want *Error, got %v", err)
	}
	if he.Kind != "finished" || he.Message != "t4 has finished" || he.Hint != "hc read t4" || he.ExitCode != 11 {
		t.Fatalf("got %+v", he)
	}
	if !IsKind(err, "finished") || IsKind(err, "not_found") {
		t.Fatal("IsKind wrong")
	}
	if got := he.Error(); got != "hc: finished: t4 has finished (hc read t4)" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (&Error{Kind: "k", Message: "m"}).Error(); got != "hc: k: m" {
		t.Fatalf("Error() no hint = %q", got)
	}
}

func TestErrorFromStdoutAndPlainText(t *testing.T) {
	c, _ := client(
		call{stdout: `{"error":{"kind":"usage","message":"bad"}}`, code: 2},
		call{stderr: "  boom  ", code: 1},
		call{stdout: "out only", code: 4},
	)
	_, err := c.Get(context.Background(), "x")
	if !IsKind(err, "usage") {
		t.Fatalf("stdout error: %v", err)
	}
	_, err = c.Get(context.Background(), "x")
	var he *Error
	if !errors.As(err, &he) || he.Kind != "exit" || he.Message != "exit 1: boom" || he.ExitCode != 1 {
		t.Fatalf("plain error: %+v", err)
	}
	_, err = c.Get(context.Background(), "x")
	if !errors.As(err, &he) || he.Message != "exit 4: out only" {
		t.Fatalf("stdout plain: %+v", err)
	}
}

func TestJSONWithoutErrorField(t *testing.T) {
	c, _ := client(call{stderr: `{"note":"x"}`, stdout: `{"error":{"kind":"from_stdout","message":"m"}}`, code: 5})
	_, err := c.Get(context.Background(), "x")
	if !IsKind(err, "from_stdout") {
		t.Fatalf("stderr JSON without error must fall through to stdout: %v", err)
	}
	c, _ = client(call{stderr: `{"note":"x"}`, code: 5})
	_, err = c.Get(context.Background(), "x")
	var he *Error
	if !errors.As(err, &he) || he == nil || he.Kind != "exit" {
		t.Fatalf("got %#v", err)
	}
}

func TestRunnerErrorPassesThrough(t *testing.T) {
	boom := errors.New("spawn failed")
	c, _ := client(call{err: boom, code: -1})
	if _, err := c.Wait(context.Background(), "t1", 5); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	c, _ = client(call{err: boom, code: -1})
	if _, err := c.Read(context.Background(), "t1", 5); !errors.Is(err, boom) {
		t.Fatalf("read got %v", err)
	}
}

func TestBadJSON(t *testing.T) {
	c, _ := client(call{stdout: "not json"})
	_, err := c.Get(context.Background(), "t1")
	if err == nil || !strings.Contains(err.Error(), "bad json") {
		t.Fatalf("got %v", err)
	}
}

func TestIsStartupRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&Error{Kind: "unavailable", Message: "t3 is still starting", Hint: "nothing was typed; check hc read t3, then retry"}, true},
		{&Error{Kind: "unavailable", Message: "x", Hint: "something else"}, false},
		{&Error{Kind: "blocked", Hint: "nothing was typed"}, false},
		{errors.New("nothing was typed"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := IsStartupRetryable(c.err); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestWait(t *testing.T) {
	c, f := client(call{stdout: `{"children":[],"ended":true,"exit_code":null,"reason":"status","signal":null,"status":"done","terminal":"t5","turn":2}`})
	r, err := c.Wait(context.Background(), "t5", 30)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ended || r.Reason != "status" || r.Status() != "done" || r.Turn != 2 || r.Terminal != "t5" || r.ExitCode != nil {
		t.Fatalf("got %+v", r)
	}
	want := []string{"wait", "--json", "--timeout", "30", "--", "t5"}
	if !reflect.DeepEqual(f.got[0], want) {
		t.Fatalf("args %q", f.got[0])
	}
	c, f = client(call{stdout: `{"ended":true,"exit_code":2,"reason":"exit","status":null,"turn":0}`})
	r, _ = c.Wait(context.Background(), "t5", 0)
	if r.Status() != "" || r.ExitCode == nil || *r.ExitCode != 2 {
		t.Fatalf("exit result %+v", r)
	}
	if !reflect.DeepEqual(f.got[0], []string{"wait", "--json", "--", "t5"}) {
		t.Fatalf("args %q", f.got[0])
	}
}

func TestSend(t *testing.T) {
	c, f := client(call{stdout: `{"started":true,"status":"idle","turn":0,"wait":{"ended":true,"reason":"status","status":"done","terminal":"t5","turn":1}}`})
	r, err := c.Send(context.Background(), "t5", "-do it", true, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Started || r.Status() != "idle" || r.Turn != 0 || r.Wait == nil || r.Wait.Status() != "done" || r.Wait.Turn != 1 {
		t.Fatalf("got %+v", r)
	}
	want := []string{"send", "--json", "--wait", "--timeout", "60", "--", "t5", "-do it"}
	if !reflect.DeepEqual(f.got[0], want) {
		t.Fatalf("args %q", f.got[0])
	}
	c, f = client(call{stdout: `{"started":true,"status":"done","turn":1}`})
	r, _ = c.Send(context.Background(), "t5", "hi", false, 60)
	if r.Wait != nil || r.Turn != 1 {
		t.Fatalf("got %+v", r)
	}
	if !reflect.DeepEqual(f.got[0], []string{"send", "--json", "--", "t5", "hi"}) {
		t.Fatalf("args %q", f.got[0])
	}
}

func TestGetStatusForms(t *testing.T) {
	c, f := client(
		call{stdout: `{"address":"t2","state":"live","status":{"app":"omp","state":"working"},"turn":3,"labels":{"a":"b"}}`},
		call{stdout: `{"address":"t2","state":"finished","status":null,"exit_code":129,"reason":"killed"}`},
		call{stdout: `{"address":"t2","status":"idle"}`},
		call{stdout: `{"address":"t2","status":42}`},
	)
	i, err := c.Get(context.Background(), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if i.StatusState() != "working" || i.Turn != 3 || i.Labels["a"] != "b" {
		t.Fatalf("got %+v", i)
	}
	if !reflect.DeepEqual(f.got[0], []string{"get", "--json", "--", "t2"}) {
		t.Fatalf("args %q", f.got[0])
	}
	i, _ = c.Get(context.Background(), "t2")
	if i.StatusState() != "" || *i.ExitCode != 129 || *i.Reason != "killed" {
		t.Fatalf("got %+v", i)
	}
	i, _ = c.Get(context.Background(), "t2")
	if i.StatusState() != "idle" {
		t.Fatalf("string status: %+v", i)
	}
	i, _ = c.Get(context.Background(), "t2")
	if i.StatusState() != "" {
		t.Fatalf("numeric status should be empty")
	}
	if (Info{}).StatusState() != "" {
		t.Fatal("empty info")
	}
}

func TestList(t *testing.T) {
	c, f := client(call{stdout: `{"hidden_finished":0,"terminals":[{"address":"t1","where":"x"},{"address":"t2"}]}`})
	ts, err := c.List(context.Background(), true, map[string]string{"fanout.run": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Where != "x" {
		t.Fatalf("got %+v", ts)
	}
	want := []string{"ls", "--json", "--all", "--label", "fanout.run=abc"}
	if !reflect.DeepEqual(f.got[0], want) {
		t.Fatalf("args %q", f.got[0])
	}
	c, f = client(call{stdout: `{"terminals":[]}`})
	_, _ = c.List(context.Background(), false, nil)
	if !reflect.DeepEqual(f.got[0], []string{"ls", "--json"}) {
		t.Fatalf("args %q", f.got[0])
	}
}

func TestRead(t *testing.T) {
	c, f := client(call{stdout: "screen text\n"}, call{stderr: `{"error":{"kind":"not_found","message":"no"}}`, code: 3})
	s, err := c.Read(context.Background(), "t1", 400)
	if err != nil || s != "screen text\n" {
		t.Fatalf("got %q %v", s, err)
	}
	if !reflect.DeepEqual(f.got[0], []string{"read", "--lines", "400", "--", "t1"}) {
		t.Fatalf("args %q", f.got[0])
	}
	if _, err := c.Read(context.Background(), "t1", 1); !IsKind(err, "not_found") {
		t.Fatalf("got %v", err)
	}
}

func TestKillRm(t *testing.T) {
	c, f := client(
		call{stdout: `{}`},
		call{stderr: `{"error":{"kind":"finished","message":"done"}}`, code: 11},
		call{stderr: `{"error":{"kind":"not_found","message":"x"}}`, code: 3},
		call{stdout: `{"removed":"t1"}`},
		call{stderr: `{"error":{"kind":"not_found","message":"x"}}`, code: 3},
		call{stderr: `{"error":{"kind":"live","message":"x"}}`, code: 10},
	)
	ctx := context.Background()
	if err := c.Kill(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Kill(ctx, "t1"); err != nil {
		t.Fatalf("finished kill should be nil: %v", err)
	}
	if err := c.Kill(ctx, "t1"); !IsKind(err, "not_found") {
		t.Fatalf("missing kill: %v", err)
	}
	if err := c.Rm(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rm(ctx, "t1"); err != nil {
		t.Fatalf("missing rm should be nil: %v", err)
	}
	if err := c.Rm(ctx, "t1"); !IsKind(err, "live") {
		t.Fatalf("live rm: %v", err)
	}
	if !reflect.DeepEqual(f.got[0], []string{"kill", "--json", "--", "t1"}) || !reflect.DeepEqual(f.got[3], []string{"rm", "--json", "--", "t1"}) {
		t.Fatalf("args %q", f.got)
	}
}

func TestLabel(t *testing.T) {
	c, f := client(call{stdout: `{}`})
	if err := c.Label(context.Background(), "t1", nil); err != nil || len(f.got) != 0 {
		t.Fatal("empty labels must not call hc")
	}
	if err := c.Label(context.Background(), "t1", map[string]string{"b": "", "a": "x"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.got[0], []string{"label", "--json", "--", "t1", "a=x", "b="}) {
		t.Fatalf("args %q", f.got[0])
	}
}

func TestTruncate(t *testing.T) {
	if truncate("abc", 3) != "abc" || truncate("abcd", 3) != "abc…" {
		t.Fatal("truncate")
	}
}

func TestNewAndExecRunner(t *testing.T) {
	c := New("/bin/echo")
	r, ok := c.R.(ExecRunner)
	if !ok || r.Bin != "/bin/echo" {
		t.Fatalf("New: %+v", c.R)
	}
	out, _, code, err := r.Run(context.Background(), []string{"hello"})
	if err != nil || code != 0 || string(out) != "hello\n" {
		t.Fatalf("echo: %q %d %v", out, code, err)
	}
	_, _, code, err = ExecRunner{Bin: "/usr/bin/false"}.Run(context.Background(), nil)
	if err != nil || code != 1 {
		t.Fatalf("false: %d %v", code, err)
	}
	_, _, code, err = ExecRunner{Bin: "/nonexistent/hc"}.Run(context.Background(), nil)
	if err == nil || code != -1 {
		t.Fatalf("missing bin: %d %v", code, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, code, err = ExecRunner{Bin: "/bin/sleep"}.Run(ctx, []string{"5"})
	if !errors.Is(err, context.Canceled) || code != -1 {
		t.Fatalf("cancelled: %d %v", code, err)
	}
	// Cancelled while running: the program is killed, which must surface as
	// the context error, not as an exit code.
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, code, err = ExecRunner{Bin: "/bin/sleep"}.Run(ctx, []string{"5"})
	if !errors.Is(err, context.DeadlineExceeded) || code != -1 {
		t.Fatalf("killed by deadline: %d %v", code, err)
	}
	if (ExecRunner{}).Bin != "" {
		t.Fatal("zero value")
	}
}
