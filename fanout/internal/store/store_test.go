package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

func ids(list ...string) func() string {
	i := 0
	return func() string {
		id := list[i%len(list)]
		i++
		return id
	}
}

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func TestFinalAndAttention(t *testing.T) {
	final := map[string]bool{Done: true, NoReport: true, Exited: true, Failed: true,
		Pending: false, Starting: false, Running: false, Blocked: false, Errored: false, Timeout: false}
	for s, want := range final {
		if Final(s) != want {
			t.Errorf("Final(%s) = %v", s, !want)
		}
	}
	attn := map[string]bool{Blocked: true, Errored: true, Timeout: true, Failed: true, Exited: true, NoReport: true,
		Done: false, Running: false, Starting: false, Pending: false}
	for s, want := range attn {
		if NeedsAttention(s) != want {
			t.Errorf("NeedsAttention(%s) = %v", s, !want)
		}
	}
}

func TestElapsed(t *testing.T) {
	w := &Worker{}
	if w.Elapsed(t0) != 0 {
		t.Fatal("not started")
	}
	w.StartedAt = t0
	if got := w.Elapsed(t0.Add(1500 * time.Millisecond)); got != 2*time.Second {
		t.Fatalf("running: %v", got)
	}
	w.EndedAt = t0.Add(10 * time.Second)
	if got := w.Elapsed(t0.Add(time.Hour)); got != 10*time.Second {
		t.Fatalf("ended: %v", got)
	}
}

func TestDefaultRoot(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := DefaultRoot(env(map[string]string{"FANOUT_HOME": "/f", "XDG_STATE_HOME": "/x"}), "/h"); got != "/f" {
		t.Fatal(got)
	}
	if got := DefaultRoot(env(map[string]string{"XDG_STATE_HOME": "/x"}), "/h"); got != "/x/fanout" {
		t.Fatal(got)
	}
	if got := DefaultRoot(env(nil), "/h"); got != "/h/.local/state/fanout" {
		t.Fatal(got)
	}
}

func TestNewID(t *testing.T) {
	a, b := NewID(), NewID()
	if len(a) != 6 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("bad id %q", a)
	}
	if a == b {
		t.Fatal("ids should differ (collision chance 1/16M)")
	}
}

func TestCreateSaveLoad(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	r, err := s.Create(ids("abc123"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "abc123" || !r.CreatedAt.Equal(t0) || r.Dir != filepath.Join(s.Root, "runs", "abc123") {
		t.Fatalf("got %+v", r)
	}
	if fi, err := os.Stat(r.ResultsDir()); err != nil || !fi.IsDir() {
		t.Fatal("results dir missing")
	}
	r.Cwd = "/repo"
	r.Thread = thread.Info{Kind: "claude", SessionID: "s", Title: "T"}
	r.Workers = []*Worker{{Index: 1, Description: "d", State: Done, Argv: []string{"omp"}, StartedAt: t0}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cwd != "/repo" || got.Thread.Title != "T" || len(got.Workers) != 1 || got.Workers[0].Argv[0] != "omp" || got.Dir != r.Dir {
		t.Fatalf("got %+v", got)
	}
	if !got.Workers[0].StartedAt.Equal(t0) {
		t.Fatal("time lost")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(r.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".run-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	data, _ := os.ReadFile(filepath.Join(r.Dir, "run.json"))
	if !strings.HasSuffix(string(data), "}\n") || strings.Contains(string(data), `"Dir"`) {
		t.Fatalf("manifest format: %s", data)
	}
}

func TestCreateSkipsTakenIDs(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, err := s.Create(ids("aaaaaa"), t0); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(ids("aaaaaa", "bbbbbb"), t0)
	if err != nil || r.ID != "bbbbbb" {
		t.Fatalf("got %+v %v", r, err)
	}
	if _, err := s.Create(ids("aaaaaa"), t0); err == nil {
		t.Fatal("should give up after repeated collisions")
	}
}

func TestCreateErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	os.WriteFile(file, []byte("x"), 0o644)
	if _, err := (&Store{Root: file}).Create(ids("a"), t0); err == nil {
		t.Fatal("root is a file: want error")
	}
	s := &Store{Root: root}
	os.MkdirAll(filepath.Join(root, "runs"), 0o755)
	os.WriteFile(filepath.Join(root, "runs", "zz"), nil, 0o644)
	if _, err := s.Create(ids("zz/sub"), t0); err == nil {
		t.Fatal("bad id path: want error")
	}
}

func TestSaveError(t *testing.T) {
	r := &Run{ID: "x", Dir: filepath.Join(t.TempDir(), "missing")}
	if err := (&Store{}).Save(r); err == nil {
		t.Fatal("want error saving into missing dir")
	}
}

func TestLoadRefs(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, err := s.Load("last"); err == nil || !strings.Contains(err.Error(), "no runs yet") {
		t.Fatalf("empty: %v", err)
	}
	if _, err := s.Load("abc"); err == nil {
		t.Fatal("missing store: want not found")
	}
	mk := func(id string, at time.Time) {
		r, err := s.Create(ids(id), at)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	mk("abc111", t0)
	mk("abc222", t0.Add(time.Minute))
	mk("abc", t0.Add(-time.Minute))
	mk("def333", t0.Add(-time.Hour))

	for ref, want := range map[string]string{"last": "abc222", "": "abc222", "abc1": "abc111", "d": "def333", "abc": "abc"} {
		r, err := s.Load(ref)
		if err != nil || r.ID != want {
			t.Errorf("Load(%q) = %v %v, want %s", ref, r, err, want)
		}
	}
	if _, err := s.Load("ab"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := s.Load("zzz"); err == nil || !strings.Contains(err.Error(), `no run "zzz"`) {
		t.Fatalf("missing: %v", err)
	}
	for _, bad := range []string{"../x", "a/b", "."} {
		if _, err := s.Load(bad); err == nil || !strings.Contains(err.Error(), "bad run id") {
			t.Errorf("Load(%q): %v", bad, err)
		}
	}
}

func TestListOrderAndSkipsBroken(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if runs, err := s.List(); err != nil || runs != nil {
		t.Fatalf("empty: %v %v", runs, err)
	}
	for _, id := range []string{"aaa", "bbb", "ccc"} {
		r, _ := s.Create(ids(id), t0)
		s.Save(r)
	}
	newer, _ := s.Create(ids("000"), t0.Add(time.Second))
	s.Save(newer)
	os.MkdirAll(filepath.Join(s.Root, "runs", "+broken"), 0o755)       // sorts first too
	os.WriteFile(filepath.Join(s.Root, "runs", "-notdir"), nil, 0o644) // sorts first
	bad, _ := s.Create(ids("badjson"), t0)
	os.WriteFile(filepath.Join(bad.Dir, "run.json"), []byte("{"), 0o644)

	runs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.ID)
	}
	if strings.Join(got, ",") != "000,ccc,bbb,aaa" {
		t.Fatalf("order %v", got)
	}
	if _, err := s.Load("badjson"); err == nil || !strings.Contains(err.Error(), "run badjson") {
		t.Fatalf("bad json: %v", err)
	}
}

func TestListError(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "runs"), []byte("x"), 0o644)
	s := &Store{Root: root}
	if _, err := s.List(); err == nil {
		t.Fatal("runs is a file: want error")
	}
	if _, err := s.Load("last"); err == nil {
		t.Fatal("want error")
	}
	if _, err := s.Load("abc"); err == nil {
		t.Fatal("want error")
	}
}

func TestWorkerLookupCountsSummary(t *testing.T) {
	r := &Run{ID: "x", Workers: []*Worker{
		{Index: 1, State: Done}, {Index: 2, State: Running}, {Index: 3, State: Done}, {Index: 4, State: Blocked},
	}}
	w, err := r.Worker(3)
	if err != nil || w.Index != 3 {
		t.Fatal(err)
	}
	if _, err := r.Worker(9); err == nil || !strings.Contains(err.Error(), "has 4") {
		t.Fatalf("got %v", err)
	}
	if c := r.Counts(); c[Done] != 2 || c[Running] != 1 || c[Blocked] != 1 {
		t.Fatalf("counts %v", c)
	}
	if got := r.Summary(); got != "2 done, 1 running, 1 blocked" {
		t.Fatalf("summary %q", got)
	}
	all := &Run{Workers: []*Worker{
		{State: Starting}, {State: Pending}, {State: NoReport}, {State: Errored}, {State: Timeout}, {State: Exited}, {State: Failed},
	}}
	if got := all.Summary(); got != "1 starting, 1 pending, 1 no_report, 1 error, 1 timeout, 1 exited, 1 failed" {
		t.Fatalf("summary %q", got)
	}
	if (&Run{}).Summary() != "" {
		t.Fatal("empty summary")
	}
}

func TestRemove(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	r, _ := s.Create(ids("abc"), t0)
	if err := s.Remove(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Dir); !os.IsNotExist(err) {
		t.Fatal("dir should be gone")
	}
	for _, dir := range []string{"", "/tmp", filepath.Join(s.Root, "runs", "a", "b")} {
		if err := s.Remove(&Run{Dir: dir}); err == nil {
			t.Errorf("Remove(%q) should refuse", dir)
		}
	}
}

func TestLockSerializesSaves(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	r, _ := s.Create(ids("lock"), t0)
	r.Workers = []*Worker{{Index: 1}}
	done := make(chan struct{})
	for i := range 20 {
		go func() {
			r.Lock()
			r.Workers[0].Turn = i
			if err := s.Save(r); err != nil {
				t.Error(err)
			}
			r.Unlock()
			done <- struct{}{}
		}()
	}
	for range 20 {
		<-done
	}
	if _, err := s.Load("lock"); err != nil {
		t.Fatal(err)
	}
}
