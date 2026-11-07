// Package task turns command-line input into the list of tasks to fan out.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Spec is one unit of work for one worker. Empty fields fall back to the
// run's defaults.
type Spec struct {
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
}

// FromArgs makes one task per non-blank argument.
func FromArgs(args []string) []Spec {
	var out []Spec
	for _, a := range args {
		if strings.TrimSpace(a) == "" {
			continue
		}
		out = append(out, Spec{Prompt: strings.TrimSpace(a)})
	}
	return out
}

// Parse reads a task file. Three forms are accepted:
//   - a JSON array of strings or of Spec objects;
//   - text with tasks separated by lines containing only "---";
//   - otherwise, one task per non-blank line ("#" lines are comments).
func Parse(data []byte) ([]Spec, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") {
		return parseJSON([]byte(trimmed))
	}
	lines := strings.Split(text, "\n")
	hasSep := slices.ContainsFunc(lines, func(l string) bool { return strings.TrimSpace(l) == "---" })
	var out []Spec
	if hasSep {
		var cur []string
		flush := func() {
			p := strings.TrimSpace(strings.Join(cur, "\n"))
			if p != "" {
				out = append(out, Spec{Prompt: p})
			}
			cur = nil
		}
		for _, l := range lines {
			if strings.TrimSpace(l) == "---" {
				flush()
				continue
			}
			cur = append(cur, l)
		}
		flush()
		return out, nil
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, Spec{Prompt: l})
	}
	return out, nil
}

func parseJSON(data []byte) ([]Spec, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("task file: %w", err)
	}
	var out []Spec
	for i, r := range raw {
		var s string
		if json.Unmarshal(r, &s) == nil {
			out = append(out, Spec{Prompt: s})
			continue
		}
		var spec Spec
		dec := json.NewDecoder(strings.NewReader(string(r)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("task %d: %w", i+1, err)
		}
		out = append(out, spec)
	}
	return out, nil
}

// Expand fills template once per item, replacing every "{}" with the item.
func Expand(template string, items []string) ([]Spec, error) {
	if !strings.Contains(template, "{}") {
		return nil, errors.New(`--each template needs a {} placeholder`)
	}
	var out []Spec
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		out = append(out, Spec{Prompt: strings.ReplaceAll(template, "{}", it), Description: it})
	}
	return out, nil
}

// Validate checks every task has a prompt and gives each a description.
func Validate(specs []Spec) ([]Spec, error) {
	if len(specs) == 0 {
		return nil, errors.New("no tasks given")
	}
	out := make([]Spec, len(specs))
	for i, s := range specs {
		s.Prompt = strings.TrimSpace(s.Prompt)
		if s.Prompt == "" {
			return nil, fmt.Errorf("task %d has an empty prompt", i+1)
		}
		if strings.TrimSpace(s.Description) == "" {
			s.Description = Describe(s.Prompt, 48)
		}
		out[i] = s
	}
	return out, nil
}

// Describe makes a one-line label from the first non-blank line of prompt,
// collapsing whitespace and truncating to max runes with an ellipsis.
func Describe(prompt string, max int) string {
	line := ""
	for _, l := range strings.Split(prompt, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = strings.Join(strings.Fields(line), " ")
	return Truncate(line, max)
}

// Truncate shortens s to at most max runes, ending in "…" when cut.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}
