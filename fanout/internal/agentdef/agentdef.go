// Package agentdef defines agent types: named, reusable worker definitions
// in Markdown with frontmatter, like Claude Code's .claude/agents/*.md.
//
//	---
//	name: reviewer
//	description: Read-only code reviewer
//	backend: omp            # omp | pi | opencode | codex, or use command:
//	model: deepseek-v4-flash
//	thinking: medium
//	yolo: true              # skip the agent's approval prompts
//	args: --no-lsp          # extra arguments for the backend
//	---
//	You review code. Never edit files.
package agentdef

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Backend describes how to start one interactive coding agent CLI.
type Backend struct {
	Name        string
	Cmd         []string
	ModelFlag   string   // e.g. "--model"; "" = unsupported
	ThinkFlag   string   // e.g. "--thinking"; "" = unsupported
	ThinkFormat string   // printf format for the thinking value with ThinkFlag (default "%s")
	YoloArgs    []string // arguments that skip approval prompts
	AddDirFlag  string   // flag granting access to another directory
}

// Backends are the built-in agent CLIs. All of them report their status to
// hc, which is what lets fanout wait for their turns.
var Backends = map[string]Backend{
	"omp": {Name: "omp", Cmd: []string{"omp"}, ModelFlag: "--model", ThinkFlag: "--thinking",
		YoloArgs: []string{"--auto-approve"}, AddDirFlag: "--add-dir"},
	"pi":       {Name: "pi", Cmd: []string{"pi"}, ModelFlag: "--model", ThinkFlag: "--thinking"},
	"opencode": {Name: "opencode", Cmd: []string{"opencode"}, ModelFlag: "--model"},
	"codex": {Name: "codex", Cmd: []string{"codex", "--no-daemon"}, ModelFlag: "-m",
		ThinkFlag: "-c", ThinkFormat: "model_reasoning_effort=%s",
		YoloArgs: []string{"--dangerously-bypass-approvals-and-sandbox"}, AddDirFlag: "--add-dir"},
}

// BackendNames returns the built-in backend names, sorted.
func BackendNames() []string {
	names := make([]string, 0, len(Backends))
	for n := range Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Definition is one agent type.
type Definition struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Backend      string   `json:"backend,omitempty"`
	Command      []string `json:"command,omitempty"`
	Model        string   `json:"model,omitempty"`
	Thinking     string   `json:"thinking,omitempty"`
	Yolo         bool     `json:"yolo,omitempty"`
	Args         []string `json:"args,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Source       string   `json:"source"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidName reports whether s can name an agent type.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Parse reads one definition file. fallbackName is used when the
// frontmatter has no name (normally the file name without .md).
func Parse(data []byte, fallbackName, source string) (Definition, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	d := Definition{Name: fallbackName, Source: source}
	body := text
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		front, after, closed := cutFrontmatter(rest)
		if !closed {
			return d, fmt.Errorf("%s: frontmatter is not closed with ---", source)
		}
		body = after
		if err := d.applyFront(front, source); err != nil {
			return d, err
		}
	}
	d.Instructions = strings.TrimSpace(body)
	return d, d.Validate()
}

// cutFrontmatter splits rest (the text after the opening ---) at the first
// line that is exactly ---, returning what came before and after it.
func cutFrontmatter(rest string) (front, after string, closed bool) {
	offset := 0
	for _, line := range strings.SplitAfter(rest, "\n") {
		if strings.TrimRight(line, "\n") == "---" {
			return rest[:offset], rest[offset+len(line):], true
		}
		offset += len(line)
	}
	return "", "", false
}

func (d *Definition) applyFront(front, source string) error {
	for i, line := range strings.Split(front, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			return fmt.Errorf("%s:%d: expected key: value", source, i+2)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = unquote(stripComment(strings.TrimSpace(val)))
		switch key {
		case "name":
			d.Name = val
		case "description":
			d.Description = val
		case "backend":
			d.Backend = val
		case "command":
			args, err := SplitArgs(val)
			if err != nil {
				return fmt.Errorf("%s: command: %w", source, err)
			}
			d.Command = args
		case "model":
			d.Model = val
		case "thinking":
			d.Thinking = val
		case "yolo":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("%s: yolo: want true or false, got %q", source, val)
			}
			d.Yolo = b
		case "args":
			args, err := SplitArgs(val)
			if err != nil {
				return fmt.Errorf("%s: args: %w", source, err)
			}
			d.Args = args
		default:
			// Unknown keys (tools, color, ...) are ignored so Claude Code
			// agent files can be reused as they are.
		}
	}
	return nil
}

// stripComment removes a trailing " # comment" outside quotes.
func stripComment(v string) string {
	inQuote := byte(0)
	for i := 0; i < len(v); i++ {
		c := v[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
		} else if c == '"' || c == '\'' {
			inQuote = c
		} else if c == '#' && i > 0 && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Validate checks that the definition can be launched.
func (d Definition) Validate() error {
	if !ValidName(d.Name) {
		return fmt.Errorf("%s: invalid agent name %q (use 1-32 of a-z 0-9 - _)", d.Source, d.Name)
	}
	if len(d.Command) > 0 && d.Backend != "" {
		return fmt.Errorf("%s: set backend or command, not both", d.Source)
	}
	if len(d.Command) == 0 {
		if _, ok := Backends[d.BackendName()]; !ok {
			return fmt.Errorf("%s: unknown backend %q (have %s)", d.Source, d.Backend, strings.Join(BackendNames(), ", "))
		}
	}
	return nil
}

// BackendName is the backend this definition runs, "omp" when unset.
func (d Definition) BackendName() string {
	if len(d.Command) > 0 {
		return "command"
	}
	if d.Backend == "" {
		return "omp"
	}
	return d.Backend
}

// Overrides adjust a definition for one launch.
type Overrides struct {
	Model    string
	Thinking string
	Yolo     bool
	AddDirs  []string
}

// Argv builds the command line that starts this agent interactively.
// The task itself is sent after the agent is ready, never on the command line.
func (d Definition) Argv(o Overrides) ([]string, error) {
	model := firstNonEmpty(o.Model, d.Model)
	thinking := firstNonEmpty(o.Thinking, d.Thinking)
	yolo := o.Yolo || d.Yolo
	if len(d.Command) > 0 {
		if model != "" || thinking != "" || yolo {
			return nil, fmt.Errorf("agent %q uses a custom command: put model/thinking/yolo flags in its command", d.Name)
		}
		return append(append([]string{}, d.Command...), d.Args...), nil
	}
	b, ok := Backends[d.BackendName()]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", d.BackendName())
	}
	argv := append([]string{}, b.Cmd...)
	if model != "" {
		if b.ModelFlag == "" {
			return nil, fmt.Errorf("backend %s has no model flag", b.Name)
		}
		argv = append(argv, b.ModelFlag, model)
	}
	if thinking != "" {
		if b.ThinkFlag == "" {
			return nil, fmt.Errorf("backend %s has no thinking flag", b.Name)
		}
		format := b.ThinkFormat
		if format == "" {
			format = "%s"
		}
		argv = append(argv, b.ThinkFlag, fmt.Sprintf(format, thinking))
	}
	if yolo {
		if len(b.YoloArgs) == 0 {
			return nil, fmt.Errorf("backend %s has no approval-skipping flag (it may not ask for approval at all)", b.Name)
		}
		argv = append(argv, b.YoloArgs...)
	}
	if b.AddDirFlag != "" {
		for _, dir := range o.AddDirs {
			argv = append(argv, b.AddDirFlag, dir)
		}
	}
	return append(argv, d.Args...), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SplitArgs splits a command line on spaces, honouring '...' and "..."
// quotes and backslash escapes outside single quotes.
func SplitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		} else if c == '\\' && i+1 < len(s) {
			i++
			cur.WriteByte(s[i])
			inArg = true
		} else if quote == '"' {
			if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		} else if c == '\'' || c == '"' {
			quote = c
			inArg = true
		} else if c == ' ' || c == '\t' || c == '\n' {
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		} else {
			cur.WriteByte(c)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// Builtins returns one definition per backend, named after it.
func Builtins() []Definition {
	desc := map[string]string{
		"omp":      "Oh My Pi coding agent (default)",
		"pi":       "pi coding agent",
		"opencode": "opencode coding agent",
		"codex":    "OpenAI Codex CLI",
	}
	var defs []Definition
	for _, n := range BackendNames() {
		defs = append(defs, Definition{Name: n, Backend: n, Description: desc[n], Source: "builtin"})
	}
	return defs
}

// Registry holds agent types by name; later sources override earlier ones.
type Registry struct {
	defs map[string]Definition
}

// NewRegistry returns a registry holding the built-ins.
func NewRegistry() *Registry {
	r := &Registry{defs: map[string]Definition{}}
	for _, d := range Builtins() {
		r.Add(d)
	}
	return r
}

// Add registers d, replacing any definition with the same name.
func (r *Registry) Add(d Definition) { r.defs[d.Name] = d }

// Get looks up an agent type.
func (r *Registry) Get(name string) (Definition, error) {
	d, ok := r.defs[name]
	if !ok {
		return Definition{}, fmt.Errorf("unknown agent %q (have %s)", name, strings.Join(r.Names(), ", "))
	}
	return d, nil
}

// Names returns all agent names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.defs))
	for n := range r.defs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// All returns all definitions sorted by name.
func (r *Registry) All() []Definition {
	var out []Definition
	for _, n := range r.Names() {
		out = append(out, r.defs[n])
	}
	return out
}

// LoadDir adds every *.md definition in dir. A missing dir is not an error.
func (r *Registry) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		d, err := Parse(data, strings.TrimSuffix(e.Name(), ".md"), path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r.Add(d)
	}
	return errors.Join(errs...)
}

// Load builds the registry: built-ins, then each dir in order (user config,
// then project), so project definitions win.
func Load(dirs ...string) (*Registry, error) {
	r := NewRegistry()
	var errs []error
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := r.LoadDir(d); err != nil {
			errs = append(errs, err)
		}
	}
	return r, errors.Join(errs...)
}
