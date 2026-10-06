//go:build windows

package harness

import (
	"os/exec"
	"strconv"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// KillTree ends the child and every descendant so nothing survives a cancel
// mid-tool-call (plan-v2 §6). Windows has no process groups to signal:
// taskkill /T walks the tree and /F forces it, so there is no graceful TERM
// to wait out and no KILL follow-up. pid <= 0 is a no-op.
func KillTree(pid int, log protocol.Logger) {
	if pid <= 0 {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err != nil {
		orNop(log)("SIGTERM failed", map[string]any{"pid": pid, "error": err.Error()})
	}
}

// setProcAttr: the TS runner spawned without `detached` on Windows (there it
// means a separate console, not a process group); taskkill /T finds the tree
// by parentage instead.
func setProcAttr(*exec.Cmd) {}
