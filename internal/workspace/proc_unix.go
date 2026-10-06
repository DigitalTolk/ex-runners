//go:build !windows

package workspace

import (
	"os"
	"os/exec"
	"syscall"
)

// shellCommand runs a dev command the way Node's spawn(cmd, {shell: true,
// detached: true}) does: /bin/sh -c in a NEW SESSION (setsid, which is what
// Node's detached does). The shell becomes the leader of its own session and
// process group, so the server survives the process that started it and its
// terminal's hangups, and killGroup can signal everything it spawned.
func shellCommand(cmd string) *exec.Cmd {
	c := exec.Command("/bin/sh", "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c
}

// killGroup sends SIGTERM to the dev server's whole process group (-pid).
func killGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// terminate stops a timed-out git with SIGTERM, like Node's execFile timeout,
// so git can remove its lock files on the way out.
func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// envKeyEqual compares environment keys (case-sensitive here).
func envKeyEqual(a, b string) bool { return a == b }

// oNoFollow makes OpenFile fail on a symlink instead of following it.
const oNoFollow = syscall.O_NOFOLLOW
