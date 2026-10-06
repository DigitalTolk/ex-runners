//go:build windows

package workspace

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the server gets its own hidden console,
// so neither it nor anything it spawns pops a window or dies with ours.
const createNoWindow = 0x08000000

// shellCommand runs a dev command the way Node's spawn(cmd, {shell: true})
// does on Windows — `%ComSpec% /d /s /c "<cmd>"`, verbatim — in a new process
// group with its own console, so it outlives the process that started it.
func shellCommand(cmd string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	c := exec.Command(comspec)
	c.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       comspec + ` /d /s /c "` + cmd + `"`,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
		HideWindow:    true,
	}
	return c
}

// killGroup ends the dev server and every process it spawned.
func killGroup(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// terminate stops a timed-out git (Windows has no SIGTERM).
func terminate(p *os.Process) error {
	return p.Kill()
}

// envKeyEqual compares environment keys; Windows treats them
// case-insensitively.
func envKeyEqual(a, b string) bool { return strings.EqualFold(a, b) }

// oNoFollow: Windows symlinks need privileges to create; nothing to add.
const oNoFollow = 0
