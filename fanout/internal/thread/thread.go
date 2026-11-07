// Package thread identifies the conversation that started a fan-out, so
// every worker terminal can say which Claude thread it belongs to when you
// type `hc` or `hc ls`.
package thread

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/kingchan818/agent-tools/fanout/internal/task"
)

// Info describes the calling thread.
type Info struct {
	Kind      string `json:"kind"` // claude | hc | env | shell
	SessionID string `json:"session_id,omitempty"`
	Title     string `json:"title"`
	Terminal  string `json:"terminal,omitempty"` // caller's hc terminal, if any
}

// Short is a compact id: the first 8 characters of the session id.
func (i Info) Short() string {
	return i.SessionID[:min(8, len(i.SessionID))]
}

// Label is the human name of the thread, e.g. "Fix login bug (fc31e7ff)".
func (i Info) Label() string {
	if i.SessionID == "" {
		return i.Title
	}
	if i.Title == "" {
		return "claude " + i.Short()
	}
	return i.Title + " (" + i.Short() + ")"
}

// Detect identifies the caller from its environment. claudeHome is the
// Claude Code config dir (~/.claude), used to read the thread's title.
//
// Order: FANOUT_THREAD (explicit) > CLAUDE_CODE_SESSION_ID > HC_TERMINAL_ID > shell.
func Detect(getenv func(string) string, claudeHome string) Info {
	term := getenv("HC_TERMINAL_ID")
	if t := strings.TrimSpace(getenv("FANOUT_THREAD")); t != "" {
		return Info{Kind: "env", Title: t, Terminal: term}
	}
	if id := strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID")); id != "" {
		return Info{Kind: "claude", SessionID: id, Title: ClaudeTitle(claudeHome, id), Terminal: term}
	}
	if term != "" {
		return Info{Kind: "hc", Title: "hc " + term, Terminal: term}
	}
	return Info{Kind: "shell", Title: "shell"}
}

// ClaudeTitle finds the session transcript under claudeHome/projects and
// returns its latest custom title (from /rename), else its latest AI title,
// else "".
func ClaudeTitle(claudeHome, sessionID string) string {
	if claudeHome == "" || sessionID == "" || strings.ContainsAny(sessionID, `/\*?[`) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(claudeHome, "projects", "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return ""
	}
	defer f.Close()
	return titleFrom(f)
}

func titleFrom(f *os.File) string {
	var custom, ai string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20) // tool results make some lines large
	for sc.Scan() {
		line := sc.Bytes()
		// Cheap filter: titles are rare among large transcript lines.
		if !strings.Contains(string(line), "title") {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			AITitle     string `json:"aiTitle"`
			CustomTitle string `json:"customTitle"`
			Title       string `json:"title"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "custom-title":
			if t := firstNonEmpty(e.CustomTitle, e.Title); t != "" {
				custom = t
			}
		case "ai-title":
			if t := firstNonEmpty(e.AITitle, e.Title); t != "" {
				ai = t
			}
		}
	}
	return strings.TrimSpace(firstNonEmpty(custom, ai))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Where builds the text hc shows in its WHERE column for a worker:
// "◆ <thread> › <description>", at most max runes.
func Where(i Info, description string, max int) string {
	thread := i.Title
	if thread == "" {
		thread = i.Label()
	}
	thread = task.Truncate(strings.Join(strings.Fields(thread), " "), 28)
	return task.Truncate("◆ "+thread+" › "+strings.Join(strings.Fields(description), " "), max)
}

// Labels are the hc labels that tie a worker to its thread.
func Labels(i Info) map[string]string {
	l := map[string]string{"fanout.thread": i.Label()}
	if i.SessionID != "" {
		l["claude.session"] = i.SessionID
	}
	if i.Terminal != "" {
		l["fanout.caller"] = i.Terminal
	}
	return l
}
