package agentdef

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseFull(t *testing.T) {
	src := "---\r\nname: reviewer\r\ndescription: \"Reviews code\" # trailing\r\nbackend: omp\r\nmodel: 'deepseek'\r\nthinking: high\r\nyolo: true\r\nargs: --no-lsp --tools \"read,grep\"\r\n# a comment\r\n\r\ntools: Read, Grep\r\n---\r\n\r\nYou review code.\r\nNever edit.\r\n"
	d, err := Parse([]byte(src), "file", "x.md")
	if err != nil {
		t.Fatal(err)
	}
	want := Definition{Name: "reviewer", Description: "Reviews code", Backend: "omp", Model: "deepseek",
		Thinking: "high", Yolo: true, Args: []string{"--no-lsp", "--tools", "read,grep"},
		Instructions: "You review code.\nNever edit.", Source: "x.md"}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("got  %+v\nwant %+v", d, want)
	}
}

func TestParseNoFrontmatterUsesFallback(t *testing.T) {
	d, err := Parse([]byte("  just instructions \n"), "helper", "helper.md")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "helper" || d.Instructions != "just instructions" || d.BackendName() != "omp" {
		t.Fatalf("got %+v", d)
	}
}

func TestParseFrontmatterOnly(t *testing.T) {
	d, err := Parse([]byte("---\nname: a\n---"), "f", "f.md")
	if err != nil || d.Instructions != "" || d.Name != "a" {
		t.Fatalf("got %+v %v", d, err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unclosed":    "---\nname: a\n",
		"no colon":    "---\nname a\n---\n",
		"bad yolo":    "---\nyolo: maybe\n---\n",
		"bad name":    "---\nname: Bad Name\n---\n",
		"bad backend": "---\nbackend: nope\n---\n",
		"both":        "---\nbackend: omp\ncommand: foo\n---\n",
		"bad args":    "---\nargs: \"open\n---\n",
		"bad command": "---\ncommand: 'open\n---\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src), "ok", name); err == nil {
			t.Errorf("%s: want error", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error should name its source: %v", name, err)
		}
	}
}

func TestParseErrorLineNumber(t *testing.T) {
	_, err := Parse([]byte("---\nname: a\noops\n---\n"), "a", "f.md")
	if err == nil || !strings.Contains(err.Error(), "f.md:3:") {
		t.Fatalf("got %v", err)
	}
}

func TestCommandDefinition(t *testing.T) {
	d, err := Parse([]byte("---\ncommand: my-agent --fast \"a b\"\nargs: --x\n---\n"), "mine", "m.md")
	if err != nil {
		t.Fatal(err)
	}
	if d.BackendName() != "command" {
		t.Fatalf("backend %q", d.BackendName())
	}
	argv, err := d.Argv(Overrides{AddDirs: []string{"/r"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv, []string{"my-agent", "--fast", "a b", "--x"}) {
		t.Fatalf("argv %q", argv)
	}
	for _, o := range []Overrides{{Model: "m"}, {Thinking: "t"}, {Yolo: true}} {
		if _, err := d.Argv(o); err == nil {
			t.Errorf("override %+v should fail on custom command", o)
		}
	}
	// Argv must not alias the definition's slices.
	argv[0] = "changed"
	if d.Command[0] != "my-agent" {
		t.Fatal("Argv aliased Command")
	}
}

func TestArgvBackends(t *testing.T) {
	cases := []struct {
		def  Definition
		o    Overrides
		want []string
	}{
		{Definition{Name: "a"}, Overrides{}, []string{"omp"}},
		{Definition{Name: "a", Backend: "omp", Model: "m1", Thinking: "low", Args: []string{"--no-lsp"}},
			Overrides{AddDirs: []string{"/d1", "/d2"}},
			[]string{"omp", "--model", "m1", "--thinking", "low", "--add-dir", "/d1", "--add-dir", "/d2", "--no-lsp"}},
		{Definition{Name: "a", Backend: "omp", Model: "m1", Thinking: "low"},
			Overrides{Model: "m2", Thinking: "high", Yolo: true},
			[]string{"omp", "--model", "m2", "--thinking", "high", "--auto-approve"}},
		{Definition{Name: "a", Backend: "omp", Yolo: true}, Overrides{}, []string{"omp", "--auto-approve"}},
		{Definition{Name: "a", Backend: "pi", Model: "openai/x", Thinking: "medium"}, Overrides{AddDirs: []string{"/d"}},
			[]string{"pi", "--model", "openai/x", "--thinking", "medium"}},
		{Definition{Name: "a", Backend: "opencode", Model: "p/m"}, Overrides{}, []string{"opencode", "--model", "p/m"}},
		{Definition{Name: "a", Backend: "codex", Model: "gpt", Thinking: "high", Yolo: true}, Overrides{AddDirs: []string{"/d"}},
			[]string{"codex", "--no-daemon", "-m", "gpt", "-c", "model_reasoning_effort=high",
				"--dangerously-bypass-approvals-and-sandbox", "--add-dir", "/d"}},
	}
	for i, c := range cases {
		got, err := c.def.Argv(c.o)
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d:\n got  %q\n want %q", i, got, c.want)
		}
	}
}

func TestArgvUnsupported(t *testing.T) {
	cases := []struct {
		def Definition
		o   Overrides
		msg string
	}{
		{Definition{Name: "a", Backend: "opencode"}, Overrides{Thinking: "high"}, "no thinking flag"},
		{Definition{Name: "a", Backend: "pi"}, Overrides{Yolo: true}, "no approval-skipping flag"},
		{Definition{Name: "a", Backend: "nope"}, Overrides{}, "unknown backend"},
	}
	for _, c := range cases {
		_, err := c.def.Argv(c.o)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%+v: got %v, want %q", c.def, err, c.msg)
		}
	}
	saved := Backends["pi"]
	b := saved
	b.ModelFlag = ""
	Backends["pi"] = b
	defer func() { Backends["pi"] = saved }()
	if _, err := (Definition{Name: "a", Backend: "pi"}).Argv(Overrides{Model: "x"}); err == nil || !strings.Contains(err.Error(), "no model flag") {
		t.Fatalf("got %v", err)
	}
}

func TestArgvDoesNotMutateBackend(t *testing.T) {
	d := Definition{Name: "a", Backend: "codex", Model: "x"}
	if _, err := d.Argv(Overrides{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(Backends["codex"].Cmd, []string{"codex", "--no-daemon"}) {
		t.Fatalf("backend mutated: %q", Backends["codex"].Cmd)
	}
}

func TestSplitArgs(t *testing.T) {
	cases := map[string][]string{
		"":              nil,
		"   ":           nil,
		"a b  c":        {"a", "b", "c"},
		"a\tb\nc":       {"a", "b", "c"},
		`"a b" 'c d'`:   {"a b", "c d"},
		`x"y z"w`:       {"xy zw"},
		`a\ b`:          {"a b"},
		`'it\s' "q\"x"`: {`it\s`, `q"x`},
		`""`:            {""},
		`''`:            {""},
		`a ""`:          {"a", ""},
		`trailing\`:     {"trailing\\"},
	}
	for in, want := range cases {
		got, err := SplitArgs(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	for _, bad := range []string{`"open`, `'open`, `a "b`} {
		if _, err := SplitArgs(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestStripCommentAndUnquote(t *testing.T) {
	cases := map[string]string{
		"value # c":   "value",
		"value\t# c":  "value",
		"a#b":         "a#b",
		"#start":      "#start",
		`"a # b" # c`: `"a # b"`,
		`'a # b'`:     `'a # b'`,
		"plain":       "plain",
		"a #b":        "a",
		"a# b":        "a# b",
	}
	for in, want := range cases {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q) = %q want %q", in, got, want)
		}
	}
	uq := map[string]string{`"x"`: "x", `'x'`: "x", `"x'`: `"x'`, `"`: `"`, "x": "x", `""`: ""}
	for in, want := range uq {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q want %q", in, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "omp", "re-view_2", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-a", "_a", "A", "a b", strings.Repeat("a", 33), "a.b"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if !reflect.DeepEqual(r.Names(), []string{"codex", "omp", "opencode", "pi"}) {
		t.Fatalf("names %q", r.Names())
	}
	d, err := r.Get("omp")
	if err != nil || d.Source != "builtin" || d.Backend != "omp" || d.Description == "" {
		t.Fatalf("omp: %+v %v", d, err)
	}
	if _, err := r.Get("missing"); err == nil || !strings.Contains(err.Error(), "codex, omp, opencode, pi") {
		t.Fatalf("missing: %v", err)
	}
	r.Add(Definition{Name: "omp", Backend: "pi", Source: "override"})
	if d, _ := r.Get("omp"); d.Source != "override" {
		t.Fatal("Add should replace")
	}
	all := r.All()
	if len(all) != 4 || all[0].Name != "codex" || all[3].Name != "pi" {
		t.Fatalf("All: %+v", all)
	}
}

func TestBackendNamesSorted(t *testing.T) {
	if !reflect.DeepEqual(BackendNames(), []string{"codex", "omp", "opencode", "pi"}) {
		t.Fatal(BackendNames())
	}
	for _, d := range Builtins() {
		if err := d.Validate(); err != nil {
			t.Errorf("builtin %s invalid: %v", d.Name, err)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirsAndPrecedence(t *testing.T) {
	user, project := t.TempDir(), t.TempDir()
	write(t, user, "reviewer.md", "---\ndescription: user\n---\nUser body")
	write(t, user, "notes.txt", "ignored")
	if err := os.Mkdir(filepath.Join(user, "sub.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, project, "reviewer.md", "---\ndescription: project\nbackend: pi\n---\nProject body")
	write(t, project, "fixer.md", "Fix things")
	r, err := Load("", user, project, filepath.Join(project, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := r.Get("reviewer")
	if d.Description != "project" || d.Backend != "pi" || d.Instructions != "Project body" || d.Source != filepath.Join(project, "reviewer.md") {
		t.Fatalf("precedence: %+v", d)
	}
	if f, err := r.Get("fixer"); err != nil || f.Instructions != "Fix things" {
		t.Fatalf("fixer: %+v %v", f, err)
	}
	if _, err := r.Get("sub"); err == nil {
		t.Fatal("directories must be skipped")
	}
}

func TestLoadReportsBadFilesButKeepsGood(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.md", "ok")
	write(t, dir, "bad.md", "---\nbackend: nope\n---\n")
	r, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "bad.md") {
		t.Fatalf("want error naming bad.md, got %v", err)
	}
	if _, err := r.Get("good"); err != nil {
		t.Fatal("good definition should still load")
	}
}

func TestLoadDirNotADir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	write(t, dir, "f", "x")
	if err := NewRegistry().LoadDir(file); err == nil {
		t.Fatal("want error reading a file as dir")
	}
}

func TestLoadDirUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "locked.md", "x")
	if err := os.Chmod(filepath.Join(dir, "locked.md"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "locked.md"), 0o644)
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	if err := NewRegistry().LoadDir(dir); err == nil {
		t.Fatal("want read error")
	}
}

func TestParseEmptyFrontmatter(t *testing.T) {
	for _, src := range []string{"---\n---\nbody\n", "---\n---", "---\n\n---\nbody"} {
		d, err := Parse([]byte(src), "empty", "e.md")
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if d.Name != "empty" {
			t.Fatalf("%q: name %q", src, d.Name)
		}
	}
	d, _ := Parse([]byte("---\n---\nbody\n"), "empty", "e.md")
	if d.Instructions != "body" {
		t.Fatalf("instructions %q", d.Instructions)
	}
}

func TestParseClosingLineMustBeExact(t *testing.T) {
	// "----" and "--- x" do not close the frontmatter; "---" with CRLF does.
	d, err := Parse([]byte("---\nname: a\n----\n---\nbody"), "f", "f.md")
	if err == nil {
		t.Fatalf("---- is not a closing line, should fail on it as a key: %+v", d)
	}
	d, err = Parse([]byte("---\r\nname: a\r\n---\r\nbody"), "f", "f.md")
	if err != nil || d.Name != "a" || d.Instructions != "body" {
		t.Fatalf("crlf: %+v %v", d, err)
	}
}

func TestCommentsAndBlankLinesDoNotStopParsing(t *testing.T) {
	d, err := Parse([]byte("---\n# first a comment\n\nname: late\n# another\nmodel: m\n---\n"), "f", "f.md")
	if err != nil || d.Name != "late" || d.Model != "m" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestLoadDirKeepsGoingAfterSkippedEntries(t *testing.T) {
	dir := t.TempDir()
	// ReadDir sorts by name: the skipped entries come before the good one.
	write(t, dir, "a-notes.txt", "x")
	if err := os.Mkdir(filepath.Join(dir, "b-dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "c-bad.md", "---\nbackend: nope\n---\n")
	write(t, dir, "d-good.md", "ok")
	r := NewRegistry()
	err := r.LoadDir(dir)
	if err == nil {
		t.Fatal("bad file should be reported")
	}
	if _, err := r.Get("d-good"); err != nil {
		t.Fatal("entries after skipped ones must load")
	}
}

func TestLoadDirKeepsGoingAfterReadError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root")
	}
	dir := t.TempDir()
	write(t, dir, "a-locked.md", "x")
	write(t, dir, "b-good.md", "ok")
	os.Chmod(filepath.Join(dir, "a-locked.md"), 0)
	defer os.Chmod(filepath.Join(dir, "a-locked.md"), 0o644)
	r := NewRegistry()
	if err := r.LoadDir(dir); err == nil {
		t.Fatal("want read error")
	}
	if _, err := r.Get("b-good"); err != nil {
		t.Fatal("files after an unreadable one must load")
	}
}
