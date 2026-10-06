// Command ex-runner runs your ex agents (Claude Code, Codex) on this
// computer. This file only binds the CLI to the real process — argv,
// stdout/stderr, signals, the browser, the runner main loop — and serves the
// per-run MCP server when a harness spawns it as `ex-runner mcp-server`.
// All behavior lives in internal/cli, internal/runner and internal/mcp.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/DigitalTolk/ex-runners/internal/cli"
	"github.com/DigitalTolk/ex-runners/internal/mcp"
	"github.com/DigitalTolk/ex-runners/internal/runner"
)

// version is stamped at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp-server" {
		cwd, _ := os.Getwd()
		os.Exit(mcp.Main(os.Getenv, cwd, os.Stdin, os.Stdout, os.Stderr))
	}

	// The first Ctrl-C / SIGTERM stops gracefully; a second means "now".
	shutdown := make(chan struct{})
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		close(shutdown)
		<-sig
		os.Exit(130)
	}()

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	host, _ := os.Hostname()
	os.Exit(cli.Run(os.Args[1:], cli.Deps{
		Home:     cli.Home(os.Getenv, cli.UserHome()),
		Version:  version,
		Hostname: host,
		Out:      func(line string) { _, _ = fmt.Fprintln(os.Stdout, line) },
		Err:      func(line string) { fmt.Fprintln(os.Stderr, line) },
		OpenBrowser: func(url string) {
			name, args := cli.BrowserCommand(runtime.GOOS, url)
			cmd := exec.Command(name, args...)
			if cmd.Start() == nil {
				go func() { _ = cmd.Wait() }()
			}
		},
		StartRunner: runner.Start,
		// The harness CLIs spawn our MCP server as this same binary.
		MCPEntry: runner.MCPEntry{Command: exe, Args: []string{"mcp-server"}},
		Shutdown: shutdown,
		PID:      os.Getpid(),
		Alive:    processAlive,
	}))
}
