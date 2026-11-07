package thread

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeTranscript(t *testing.T, home, project, id, content string) {
	t.Helper()
	dir := filepath.Join(home, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sid = "fc31e7ff-59d6-41c7-835b-2e807f5c33f3"

func TestDetectClaudeWithTitle(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-Users-x-proj", sid, strings.Join([]string{
		`{"type":"user","message":"title of nothing"}`,
		`{"type":"ai-title","aiTitle":"First title","sessionId":"x"}`,
		`not json but has title`,
		`{"type":"ai-title","aiTitle":"Omp fanout agent tool"}`,
		`{"type":"ai-title","aiTitle":""}`,
	}, "\n"))
	got := Detect(env(map[string]string{"CLAUDE_CODE_SESSION_ID": " " + sid + " ", "HC_TERMINAL_ID": "t4"}), home)
	want := Info{Kind: "claude", SessionID: sid, Title: "Omp fanout agent tool", Terminal: "t4"}
	if got != want {
		t.Fatalf("got %+v", got)
	}
	if got.Short() != "fc31e7ff" || got.Label() != "Omp fanout agent tool (fc31e7ff)" {
		t.Fatalf("short/label: %q %q", got.Short(), got.Label())
	}
}

func TestCustomTitleWins(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "p", sid, strings.Join([]string{
		`{"type":"custom-title","customTitle":"My rename"}`,
		`{"type":"ai-title","aiTitle":"Auto"}`,
		`{"type":"custom-title","customTitle":""}`,
	}, "\n"))
	if got := ClaudeTitle(home, sid); got != "My rename" {
		t.Fatalf("got %q", got)
	}
	writeTranscript(t, home, "p", "other", `{"type":"custom-title","title":"  Via title field  "}`+"\n"+`{"type":"ai-title","title":"ai via title"}`)
	if got := ClaudeTitle(home, "other"); got != "Via title field" {
		t.Fatalf("got %q", got)
	}
	writeTranscript(t, home, "p", "aionly", `{"type":"ai-title","title":"ai via title"}`)
	if got := ClaudeTitle(home, "aionly"); got != "ai via title" {
		t.Fatalf("got %q", got)
	}
}

func TestClaudeTitleMissing(t *testing.T) {
	home := t.TempDir()
	cases := []struct{ home, id string }{{"", sid}, {home, ""}, {home, sid}, {home, "../x"}, {home, "a*"}}
	for _, c := range cases {
		if got := ClaudeTitle(c.home, c.id); got != "" {
			t.Errorf("%+v: got %q", c, got)
		}
	}
	// A directory named like a transcript cannot be read as a file.
	if err := os.MkdirAll(filepath.Join(home, "projects", "p", "dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeTitle(home, "dir"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestClaudeTitleUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root")
	}
	home := t.TempDir()
	writeTranscript(t, home, "p", "locked", `{"type":"ai-title","aiTitle":"x"}`)
	path := filepath.Join(home, "projects", "p", "locked.jsonl")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	if got := ClaudeTitle(home, "locked"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestClaudeTitleLongLines(t *testing.T) {
	home := t.TempDir()
	big := `{"type":"assistant","text":"` + strings.Repeat("x", 200*1024) + `"}`
	writeTranscript(t, home, "p", sid, big+"\n"+`{"type":"ai-title","aiTitle":"After big line"}`)
	if got := ClaudeTitle(home, sid); got != "After big line" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectOrder(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want Info
	}{
		{map[string]string{"FANOUT_THREAD": " release prep ", "CLAUDE_CODE_SESSION_ID": sid, "HC_TERMINAL_ID": "t2"},
			Info{Kind: "env", Title: "release prep", Terminal: "t2"}},
		{map[string]string{"FANOUT_THREAD": "  ", "CLAUDE_CODE_SESSION_ID": sid},
			Info{Kind: "claude", SessionID: sid}},
		{map[string]string{"HC_TERMINAL_ID": "t9"}, Info{Kind: "hc", Title: "hc t9", Terminal: "t9"}},
		{map[string]string{}, Info{Kind: "shell", Title: "shell"}},
	}
	for i, c := range cases {
		if got := Detect(env(c.env), t.TempDir()); got != c.want {
			t.Errorf("case %d: got %+v want %+v", i, got, c.want)
		}
	}
}

func TestLabelAndShort(t *testing.T) {
	cases := []struct {
		i           Info
		short, want string
	}{
		{Info{SessionID: sid}, "fc31e7ff", "claude fc31e7ff"},
		{Info{SessionID: "abc"}, "abc", "claude abc"},
		{Info{SessionID: "12345678"}, "12345678", "claude 12345678"},
		{Info{Title: "shell"}, "", "shell"},
		{Info{SessionID: "abc", Title: "T"}, "abc", "T (abc)"},
	}
	for _, c := range cases {
		if c.i.Short() != c.short || c.i.Label() != c.want {
			t.Errorf("%+v: short %q label %q", c.i, c.i.Short(), c.i.Label())
		}
	}
}

func TestWhere(t *testing.T) {
	i := Info{SessionID: sid, Title: "Omp fanout agent tool"}
	if got := Where(i, "review  x.rs", 60); got != "◆ Omp fanout agent tool › review x.rs" {
		t.Fatalf("got %q", got)
	}
	long := Info{Title: "A very long thread title that keeps going on"}
	got := Where(long, "a description that is also quite long indeed", 50)
	if got != "◆ A very long thread title th… › a description th…" {
		t.Fatalf("got %q (%d runes)", got, len([]rune(got)))
	}
	if n := len([]rune(got)); n > 50 {
		t.Fatalf("too long: %d", n)
	}
	if got := Where(Info{SessionID: "abc"}, "d", 60); got != "◆ claude abc › d" {
		t.Fatalf("no title: %q", got)
	}
}

func TestLabels(t *testing.T) {
	got := Labels(Info{Kind: "claude", SessionID: sid, Title: "T", Terminal: "t3"})
	want := map[string]string{"fanout.thread": "T (fc31e7ff)", "claude.session": sid, "fanout.caller": "t3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got = Labels(Info{Kind: "shell", Title: "shell"})
	if !reflect.DeepEqual(got, map[string]string{"fanout.thread": "shell"}) {
		t.Fatalf("got %v", got)
	}
}

func TestClaudeTitleGuards(t *testing.T) {
	home := t.TempDir()
	// Files that a missing guard would glob: an empty id matches ".jsonl",
	// a pattern id matches "abc.jsonl".
	writeTranscript(t, home, "p", "", `{"type":"ai-title","aiTitle":"empty id"}`)
	writeTranscript(t, home, "p", "abc", `{"type":"ai-title","aiTitle":"pattern"}`)
	if got := ClaudeTitle(home, ""); got != "" {
		t.Fatalf("empty id: %q", got)
	}
	if got := ClaudeTitle(home, "a*"); got != "" {
		t.Fatalf("pattern id: %q", got)
	}
	if got := ClaudeTitle(home, "abc"); got != "pattern" {
		t.Fatalf("real id: %q", got)
	}
	// An empty home must not search the current folder.
	t.Chdir(home)
	if got := ClaudeTitle("", "abc"); got != "" {
		t.Fatalf("empty home searched cwd: %q", got)
	}
}
