package task

import (
	"reflect"
	"strings"
	"testing"
)

func prompts(specs []Spec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Prompt)
	}
	return out
}

func TestFromArgs(t *testing.T) {
	got := FromArgs([]string{" a ", "", "  ", "b c"})
	if !reflect.DeepEqual(prompts(got), []string{"a", "b c"}) {
		t.Fatalf("got %+v", got)
	}
	if FromArgs(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

func TestParseLines(t *testing.T) {
	got, err := Parse([]byte("# header\nfirst task\r\n\n  second task  \n#skip\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prompts(got), []string{"first task", "second task"}) {
		t.Fatalf("got %q", prompts(got))
	}
}

func TestParseSeparated(t *testing.T) {
	src := "Review a.go\n# keep this line\n\n---\n\n---\nFix b.go\nthen test\n  ---  \nlast"
	got, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Review a.go\n# keep this line", "Fix b.go\nthen test", "last"}
	if !reflect.DeepEqual(prompts(got), want) {
		t.Fatalf("got %q", prompts(got))
	}
}

func TestParseJSON(t *testing.T) {
	src := `  [ "plain", {"description":"d","prompt":"p","agent":"pi","model":"m","cwd":"/c"} ]`
	got, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Spec{{Prompt: "plain"}, {Description: "d", Prompt: "p", Agent: "pi", Model: "m", Cwd: "/c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseJSONErrors(t *testing.T) {
	for _, src := range []string{`[`, `[{"prompt":"x","bogus":1}]`, `[1]`} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: want error", src)
		}
	}
	_, err := Parse([]byte(`["ok", {"nope":1}]`))
	if err == nil || !strings.Contains(err.Error(), "task 2") {
		t.Fatalf("error should name task 2: %v", err)
	}
}

func TestParseEmpty(t *testing.T) {
	got, err := Parse([]byte("\n\n# only comments\n"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestExpand(t *testing.T) {
	got, err := Expand("Review {} carefully; only {}", []string{"a.go", " ", " b.go "})
	if err != nil {
		t.Fatal(err)
	}
	want := []Spec{
		{Prompt: "Review a.go carefully; only a.go", Description: "a.go"},
		{Prompt: "Review b.go carefully; only b.go", Description: "b.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if _, err := Expand("no placeholder", []string{"x"}); err == nil {
		t.Fatal("want error")
	}
}

func TestValidate(t *testing.T) {
	if _, err := Validate(nil); err == nil {
		t.Fatal("empty list must fail")
	}
	_, err := Validate([]Spec{{Prompt: "ok"}, {Prompt: "  "}})
	if err == nil || !strings.Contains(err.Error(), "task 2") {
		t.Fatalf("got %v", err)
	}
	in := []Spec{{Prompt: "  Review the parser\nmore  "}, {Prompt: "x", Description: "kept"}}
	got, err := Validate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Prompt != "Review the parser\nmore" || got[0].Description != "Review the parser" || got[1].Description != "kept" {
		t.Fatalf("got %+v", got)
	}
	if in[0].Description != "" {
		t.Fatal("Validate must not mutate its input")
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"\n\n  hello   world \nsecond", 48, "hello world"},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abcde…"},
		{"ab cdefg", 4, "ab…"},
		{"日本語のテキスト", 4, "日本語…"},
		{"", 10, ""},
		{"x", 0, ""},
	}
	for _, c := range cases {
		if got := Describe(c.in, c.max); got != c.want {
			t.Errorf("Describe(%q,%d) = %q want %q", c.in, c.max, got, c.want)
		}
	}
	if Truncate("ab", 1) != "…" || Truncate("a", 1) != "a" || Truncate("abc", -1) != "" {
		t.Fatal("Truncate edges")
	}
}
