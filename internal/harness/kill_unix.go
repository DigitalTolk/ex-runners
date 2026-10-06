//go:build unix

package harness

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// killGrace is how long a tree gets to exit on SIGTERM before SIGKILL.
var killGrace = 5 * time.Second

// KillTree signals the child's process group (TERM, then KILL after grace)
// so nothing survives a cancel mid-tool-call — CLIs spawn children, and a
// stray MCP server or helper outliving the run is a leak (plan-v2 §6). The
// KILL timer never keeps the runner alive. pid <= 0 is a no-op.
func KillTree(pid int, log protocol.Logger) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		orNop(log)("SIGTERM failed", map[string]any{"pid": pid, "error": err.Error()})
	}
	time.AfterFunc(killGrace, func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL) // already gone — the normal case
	})
}

// setProcAttr starts the child as the leader of a new session — and so of
// its own process group — exactly as Node's `detached: true` did (libuv
// calls setsid()). KillTree's group signal then reaches the whole tree, and
// the child has no controlling terminal to stop on.
func setProcAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
