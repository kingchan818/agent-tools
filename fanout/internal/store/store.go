// Package store persists fan-out runs: one directory per run holding
// run.json (the manifest) and the workers' reports.
package store

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kingchan818/agent-tools/fanout/internal/thread"
)

// Worker states.
const (
	Pending  = "pending"   // not launched yet
	Starting = "starting"  // terminal launched, agent booting
	Running  = "running"   // task sent, agent working
	Done     = "done"      // turn finished and a report was written
	NoReport = "no_report" // turn finished without writing the report
	Blocked  = "blocked"   // agent needs a person (permission, dialog)
	Errored  = "error"     // agent reported an error
	Exited   = "exited"    // agent program ended
	Timeout  = "timeout"   // gave up waiting; agent still running
	Failed   = "failed"    // could not launch or send
)

// Final reports whether a worker in state s needs no more waiting.
func Final(s string) bool {
	switch s {
	case Done, NoReport, Exited, Failed:
		return true
	}
	return false
}

// NeedsAttention reports whether s means a person should look.
func NeedsAttention(s string) bool {
	switch s {
	case Blocked, Errored, Timeout, Failed, Exited, NoReport:
		return true
	}
	return false
}

// Worker is one agent working on one task.
type Worker struct {
	Index       int       `json:"index"`
	Description string    `json:"description"`
	Task        string    `json:"task"`
	Prompt      string    `json:"prompt"`
	Agent       string    `json:"agent"`
	Backend     string    `json:"backend"`
	Argv        []string  `json:"argv"`
	Cwd         string    `json:"cwd"`
	Worktree    string    `json:"worktree,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	Name        string    `json:"name"`
	Terminal    string    `json:"terminal,omitempty"`
	State       string    `json:"state"`
	SentTurn    int       `json:"sent_turn"`
	Turn        int       `json:"turn"`
	ResultPath  string    `json:"result_path"`
	ScreenPath  string    `json:"screen_path"`
	Error       string    `json:"error,omitempty"`
	Closed      bool      `json:"closed,omitempty"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	EndedAt     time.Time `json:"ended_at,omitzero"`
}

// Elapsed is how long the worker ran (until now if it has not ended).
func (w *Worker) Elapsed(now time.Time) time.Duration {
	if w.StartedAt.IsZero() {
		return 0
	}
	end := w.EndedAt
	if end.IsZero() {
		end = now
	}
	return end.Sub(w.StartedAt).Round(time.Second)
}

// Run is one fan-out.
type Run struct {
	ID        string      `json:"id"`
	CreatedAt time.Time   `json:"created_at"`
	Cwd       string      `json:"cwd"`
	Thread    thread.Info `json:"thread"`
	Keep      bool        `json:"keep,omitempty"` // keep terminals after reports
	Workers   []*Worker   `json:"workers"`
	Dir       string      `json:"-"`

	mu sync.Mutex
}

// Lock guards concurrent worker updates and saves.
func (r *Run) Lock()   { r.mu.Lock() }
func (r *Run) Unlock() { r.mu.Unlock() }

// ResultsDir holds the workers' reports.
func (r *Run) ResultsDir() string { return filepath.Join(r.Dir, "results") }

// Worker returns the worker with the given 1-based index.
func (r *Run) Worker(index int) (*Worker, error) {
	for _, w := range r.Workers {
		if w.Index == index {
			return w, nil
		}
	}
	return nil, fmt.Errorf("run %s has no worker %d (it has %d)", r.ID, index, len(r.Workers))
}

// Counts tallies workers by state.
func (r *Run) Counts() map[string]int {
	c := map[string]int{}
	for _, w := range r.Workers {
		c[w.State]++
	}
	return c
}

// Summary is a compact state tally like "2 done, 1 running".
func (r *Run) Summary() string {
	c := r.Counts()
	order := []string{Done, Running, Starting, Pending, NoReport, Blocked, Errored, Timeout, Exited, Failed}
	var parts []string
	for _, s := range order {
		if c[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c[s], s))
		}
	}
	return strings.Join(parts, ", ")
}

// Store keeps runs under Root/runs.
type Store struct{ Root string }

// DefaultRoot picks the state directory: $FANOUT_HOME, else
// $XDG_STATE_HOME/fanout, else ~/.local/state/fanout.
func DefaultRoot(getenv func(string) string, home string) string {
	if v := getenv("FANOUT_HOME"); v != "" {
		return v
	}
	if v := getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "fanout")
	}
	return filepath.Join(home, ".local", "state", "fanout")
}

func (s *Store) runsDir() string { return filepath.Join(s.Root, "runs") }

// NewID returns a fresh 6-hex-character run id.
func NewID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Create makes a new run directory with a unique id from newID.
func (s *Store) Create(newID func() string, now time.Time) (*Run, error) {
	for range 20 {
		id := newID()
		dir := filepath.Join(s.runsDir(), id)
		if err := os.MkdirAll(s.runsDir(), 0o755); err != nil {
			return nil, err
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, err
		}
		if err := os.Mkdir(filepath.Join(dir, "results"), 0o755); err != nil {
			return nil, err
		}
		return &Run{ID: id, CreatedAt: now, Dir: dir}, nil
	}
	return nil, errors.New("could not pick a unique run id")
}

// Save writes run.json atomically. Callers updating workers concurrently
// should hold the run's lock.
func (s *Store) Save(r *Run) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.Dir, ".run-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(r.Dir, "run.json"))
}

// Load reads a run by id, unique id prefix, or "last" (the newest run).
func (s *Store) Load(ref string) (*Run, error) {
	if ref == "" || ref == "last" {
		runs, err := s.List()
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			return nil, errors.New("no runs yet")
		}
		return runs[0], nil
	}
	if strings.ContainsAny(ref, `/\.`) {
		return nil, fmt.Errorf("bad run id %q", ref)
	}
	entries, err := os.ReadDir(s.runsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ref) {
			if e.Name() == ref {
				matches = []string{ref}
				break
			}
			matches = append(matches, e.Name())
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no run %q", ref)
	case 1:
		return s.read(matches[0])
	default:
		return nil, fmt.Errorf("run %q is ambiguous: %s", ref, strings.Join(matches, ", "))
	}
}

func (s *Store) read(id string) (*Run, error) {
	dir := filepath.Join(s.runsDir(), id)
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	r.Dir = dir
	return &r, nil
}

// List returns all readable runs, newest first.
func (s *Store) List() ([]*Run, error) {
	entries, err := os.ReadDir(s.runsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := s.read(e.Name())
		if err != nil {
			continue // half-created or foreign directory
		}
		runs = append(runs, r)
	}
	slices.SortFunc(runs, func(a, b *Run) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID))
	})
	return runs, nil
}

// Remove deletes a run's directory.
func (s *Store) Remove(r *Run) error {
	if r.Dir == "" || filepath.Dir(r.Dir) != s.runsDir() {
		return fmt.Errorf("refusing to remove %q: not a run directory", r.Dir)
	}
	return os.RemoveAll(r.Dir)
}
