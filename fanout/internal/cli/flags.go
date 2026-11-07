package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// flags wraps flag.FlagSet with long/short aliases and interspersed
// parsing, so `fanout run "task" --agent pi` works like `fanout run --agent pi "task"`.
type flags struct {
	fs    *flag.FlagSet
	usage string
}

func newFlags(name, usage string) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &flags{fs: fs, usage: usage}
}

func names(spec string) []string { return strings.Split(spec, ",") }

func (f *flags) str(spec, def string) *string {
	p := new(string)
	for _, n := range names(spec) {
		f.fs.StringVar(p, n, def, "")
	}
	return p
}

func (f *flags) boolean(spec string) *bool {
	p := new(bool)
	for _, n := range names(spec) {
		f.fs.BoolVar(p, n, false, "")
	}
	return p
}

func (f *flags) integer(spec string, def int) *int {
	p := new(int)
	for _, n := range names(spec) {
		f.fs.IntVar(p, n, def, "")
	}
	return p
}

func (f *flags) duration(spec string, def time.Duration) *time.Duration {
	p := new(time.Duration)
	for _, n := range names(spec) {
		f.fs.DurationVar(p, n, def, "")
	}
	return p
}

// errHelp asks the caller to print usage and succeed.
type errHelp struct{ usage string }

func (e errHelp) Error() string { return "help requested" }

// usageErr is a command-line mistake (exit 2).
type usageErr struct{ msg, usage string }

func (e usageErr) Error() string { return e.msg }

// parse parses args allowing flags after positional arguments. Everything
// after a literal "--" is positional.
func (f *flags) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.fs.Parse(args); err != nil {
			if err == flag.ErrHelp {
				return nil, errHelp{f.usage}
			}
			return nil, usageErr{err.Error(), f.usage}
		}
		rest := f.fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func wantArgs(pos []string, min, max int, usage string) error {
	if len(pos) < min {
		return usageErr{"missing arguments", usage}
	}
	if max >= 0 && len(pos) > max {
		return usageErr{fmt.Sprintf("unexpected argument %q", pos[max]), usage}
	}
	return nil
}
