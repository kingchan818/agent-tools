// Command fanout runs non-Claude coding agents as parallel subagents in hc
// terminals. See `fanout help`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/kingchan818/agent-tools/fanout/internal/cli"
	"github.com/kingchan818/agent-tools/fanout/internal/hc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	hcBin := os.Getenv("FANOUT_HC")
	if hcBin == "" {
		hcBin = "hc"
	}
	env := &cli.Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin,
		Getenv: os.Getenv, Home: home, Cwd: cwd,
		HC: hc.New(hcBin),
		Exec: func(argv []string) error {
			path, err := exec.LookPath(hcBin)
			if err != nil {
				return err
			}
			argv[0] = path
			return syscall.Exec(path, argv, os.Environ())
		},
	}
	code := cli.Main(ctx, os.Args[1:], env)
	if ctx.Err() != nil && code != 0 {
		fmt.Fprintln(os.Stderr, "fanout: interrupted; workers keep running (fanout status / fanout wait)")
	}
	os.Exit(code)
}
