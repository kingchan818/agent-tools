// Package hc is a typed client for the herdr-core CLI (`hc`).
//
// Every call shells out to `hc ... --json` through a Runner, so tests can
// replace the process boundary with a fake.
package hc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Runner executes the hc binary with args and reports what it printed.
// A non-zero exit is not an error: it is returned in exitCode.
type Runner interface {
	Run(ctx context.Context, args []string) (stdout, stderr []byte, exitCode int, err error)
}

// ExecRunner runs the real hc binary.
type ExecRunner struct{ Bin string }

// Run implements Runner.
func (e ExecRunner) Run(ctx context.Context, args []string) ([]byte, []byte, int, error) {
	bin := e.Bin
	if bin == "" {
		bin = "hc"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		return out.Bytes(), errb.Bytes(), -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.Bytes(), errb.Bytes(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return out.Bytes(), errb.Bytes(), -1, err
	}
	return out.Bytes(), errb.Bytes(), 0, nil
}

// Error is hc's structured error: {"error":{"kind","message","hint"}}.
type Error struct {
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Hint     string `json:"hint"`
	ExitCode int    `json:"-"`
}

func (e *Error) Error() string {
	s := "hc: " + e.Kind + ": " + e.Message
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}

// IsKind reports whether err is an hc Error of the given kind.
func IsKind(err error, kind string) bool {
	var he *Error
	return errors.As(err, &he) && he.Kind == kind
}

// IsStartupRetryable reports whether a failed send can be retried safely:
// hc refused because the program was still starting and typed nothing.
func IsStartupRetryable(err error) bool {
	var he *Error
	if !errors.As(err, &he) || he.Kind != "unavailable" {
		return false
	}
	return strings.Contains(he.Hint, "nothing was typed")
}

// Terminal is what `hc run --bg` returns.
type Terminal struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Agent   string `json:"agent"`
	Cwd     string `json:"cwd"`
	State   string `json:"state"`
	Turn    int    `json:"turn"`
	Where   string `json:"where"`
}

// AgentStatus is the status object hc reports for an agent.
type AgentStatus struct {
	App   string `json:"app"`
	State string `json:"state"`
}

// Info is what `hc get` (and `hc ls` entries) return.
type Info struct {
	Address  string            `json:"address"`
	Name     string            `json:"name"`
	Agent    string            `json:"agent"`
	State    string            `json:"state"`
	Status   json.RawMessage   `json:"status"`
	Turn     int               `json:"turn"`
	ExitCode *int              `json:"exit_code"`
	Reason   *string           `json:"reason"`
	Labels   map[string]string `json:"labels"`
	Cwd      string            `json:"cwd"`
	Where    string            `json:"where"`
	Title    string            `json:"title"`
	Parent   *string           `json:"parent"`
}

// StatusState returns the agent state ("idle", "working", ...), whether hc
// reported it as an object ({app,state}) or a bare string; "" when none.
func (i Info) StatusState() string {
	return statusString(i.Status)
}

func statusString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj AgentStatus
	if json.Unmarshal(raw, &obj) == nil {
		return obj.State
	}
	return ""
}

// WaitResult is what `hc wait` returns.
type WaitResult struct {
	Ended    bool            `json:"ended"`
	Reason   string          `json:"reason"`
	RawStat  json.RawMessage `json:"status"`
	Terminal string          `json:"terminal"`
	Turn     int             `json:"turn"`
	ExitCode *int            `json:"exit_code"`
}

// Status returns the reported status, "" when hc reported none.
func (w WaitResult) Status() string { return statusString(w.RawStat) }

// SendResult is what `hc send` returns; Wait is set with --wait.
type SendResult struct {
	Started bool            `json:"started"`
	RawStat json.RawMessage `json:"status"`
	Turn    int             `json:"turn"`
	Wait    *WaitResult     `json:"wait"`
}

// Status returns the status at the moment of sending.
func (s SendResult) Status() string { return statusString(s.RawStat) }

// LaunchOpts configures `hc run --bg`.
type LaunchOpts struct {
	Name          string
	Cwd           string
	Labels        map[string]string
	Parent        string
	NoParent      bool
	NoNotify      bool
	OutliveParent bool
	Argv          []string
}

// Client talks to hc through a Runner.
type Client struct{ R Runner }

// New returns a client for the hc binary at bin ("" means "hc" on PATH).
func New(bin string) *Client { return &Client{R: ExecRunner{Bin: bin}} }

func (c *Client) call(ctx context.Context, args []string, out any) error {
	stdout, stderr, code, err := c.R.Run(ctx, args)
	if err != nil {
		return err
	}
	if code != 0 {
		return parseError(stderr, stdout, code)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), out); err != nil {
		return fmt.Errorf("hc %s: bad json %q: %w", args[0], truncate(string(stdout), 200), err)
	}
	return nil
}

func parseError(stderr, stdout []byte, code int) error {
	var env struct {
		Error *Error `json:"error"`
	}
	for _, b := range [][]byte{stderr, stdout} {
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			continue
		}
		if json.Unmarshal(b, &env) == nil && env.Error != nil {
			env.Error.ExitCode = code
			return env.Error
		}
	}
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(stdout))
	}
	return &Error{Kind: "exit", Message: fmt.Sprintf("exit %d: %s", code, truncate(msg, 300)), ExitCode: code}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func labelArgs(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}

// LaunchArgs builds the argument list for a background `hc run`.
func LaunchArgs(o LaunchOpts) []string {
	args := []string{"run", "--json", "--bg"}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	if o.Cwd != "" {
		args = append(args, "--cwd", o.Cwd)
	}
	args = append(args, labelArgs(o.Labels)...)
	if o.NoParent {
		args = append(args, "--no-parent")
	} else if o.Parent != "" {
		args = append(args, "--parent", o.Parent)
	}
	if o.NoNotify {
		args = append(args, "--no-notify")
	}
	if o.OutliveParent {
		args = append(args, "--outlive-parent")
	}
	args = append(args, "--")
	return append(args, o.Argv...)
}

// Launch starts a program in a new background terminal.
func (c *Client) Launch(ctx context.Context, o LaunchOpts) (Terminal, error) {
	if len(o.Argv) == 0 {
		return Terminal{}, errors.New("hc launch: empty argv")
	}
	var t Terminal
	err := c.call(ctx, LaunchArgs(o), &t)
	return t, err
}

func timeoutArgs(sec int) []string {
	if sec <= 0 {
		return nil
	}
	return []string{"--timeout", strconv.Itoa(sec)}
}

// Wait waits until terminal t stops working, exits, or timeoutSec passes.
func (c *Client) Wait(ctx context.Context, t string, timeoutSec int) (WaitResult, error) {
	args := append([]string{"wait", "--json"}, timeoutArgs(timeoutSec)...)
	args = append(args, "--", t)
	var r WaitResult
	err := c.call(ctx, args, &r)
	return r, err
}

// Send pastes text into t and presses enter; with wait it waits for the turn.
func (c *Client) Send(ctx context.Context, t, text string, wait bool, timeoutSec int) (SendResult, error) {
	args := []string{"send", "--json"}
	if wait {
		args = append(args, "--wait")
		args = append(args, timeoutArgs(timeoutSec)...)
	}
	args = append(args, "--", t, text)
	var r SendResult
	err := c.call(ctx, args, &r)
	return r, err
}

// Get returns one terminal's state.
func (c *Client) Get(ctx context.Context, t string) (Info, error) {
	var i Info
	err := c.call(ctx, []string{"get", "--json", "--", t}, &i)
	return i, err
}

// List returns terminals, optionally finished ones too, filtered by labels.
func (c *Client) List(ctx context.Context, all bool, labels map[string]string) ([]Info, error) {
	args := []string{"ls", "--json"}
	if all {
		args = append(args, "--all")
	}
	args = append(args, labelArgs(labels)...)
	var r struct {
		Terminals []Info `json:"terminals"`
	}
	err := c.call(ctx, args, &r)
	return r.Terminals, err
}

// Read returns the screen plus up to lines of history as text.
func (c *Client) Read(ctx context.Context, t string, lines int) (string, error) {
	stdout, stderr, code, err := c.R.Run(ctx, []string{"read", "--lines", strconv.Itoa(lines), "--", t})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", parseError(stderr, stdout, code)
	}
	return string(stdout), nil
}

// Kill ends t's program. Killing a finished terminal is not an error.
func (c *Client) Kill(ctx context.Context, t string) error {
	err := c.call(ctx, []string{"kill", "--json", "--", t}, nil)
	if IsKind(err, "finished") {
		return nil
	}
	return err
}

// Rm removes a finished terminal. Removing a missing one is not an error.
func (c *Client) Rm(ctx context.Context, t string) error {
	err := c.call(ctx, []string{"rm", "--json", "--", t}, nil)
	if IsKind(err, "not_found") {
		return nil
	}
	return err
}

// Label sets labels on t; an empty value removes that label.
func (c *Client) Label(ctx context.Context, t string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	args := []string{"label", "--json", "--", t}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, k+"="+labels[k])
	}
	return c.call(ctx, args, nil)
}
