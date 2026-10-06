package runner

import "github.com/DigitalTolk/ex-runners/internal/protocol"

// MCPEntry is how the harness CLIs spawn our MCP server: the ex-runner
// binary itself with the hidden `mcp-server` subcommand.
type MCPEntry struct {
	Command string
	Args    []string
}

// Config starts one runner.
type Config struct {
	BaseURL string // chat server origin
	// Token is the runner-scoped JWT from `ex-runner login`; a getter lets
	// the CLI swap in a renewed token without restarting the runner.
	Token    func() string
	StateDir string // runner ID, warm-session pins, per-thread work dirs
	// WorkspaceRoot for coding tasks ("" → EX_WORKSPACE_ROOT or ~/ex-workspace).
	WorkspaceRoot string
	MCPEntry      MCPEntry
	Log           protocol.Logger
	// OnUnauthorized is called once when the server rejects the runner
	// token (revoked from the Runners page, account deactivated, or
	// expired): the runner has stopped claiming, and only a fresh
	// `ex-runner login` brings it back.
	OnUnauthorized func()
}

// Status is a snapshot of a live runner.
type Status struct {
	RunnerID   string
	Harnesses  []string
	ActiveRuns int
}

// Handle controls a started runner.
type Handle interface {
	// Stop ends claiming and waits for in-flight runs. With a reason, it
	// first kills every running harness and fails those runs with that
	// reason — a shutdown must not leave orphaned agents on a dead lease.
	Stop(reason string)
	Status() Status
}
