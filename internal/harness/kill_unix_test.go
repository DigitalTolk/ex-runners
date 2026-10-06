//go:build unix

package harness

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startGroup runs script as its own session/process-group leader (as the
// adapters spawn the CLIs) and returns it with the pid of the background
// grandchild it reports on its first stdout line.
func startGroup(t *testing.T, script string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	setProcAttr(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	// Own process group: the leader's pgid is its pid, the grandchild shares it.
	if pgid, _ := syscall.Getpgid(cmd.Process.Pid); pgid != cmd.Process.Pid {
		t.Fatalf("leader pgid = %d, pid = %d", pgid, cmd.Process.Pid)
	}
	if pgid, _ := syscall.Getpgid(child); pgid != cmd.Process.Pid {
		t.Fatalf("grandchild pgid = %d", pgid)
	}
	return cmd, child
}

func TestKillTreeTermsTheWholeGroup(t *testing.T) {
	cmd, child := startGroup(t, `sleep 300 & echo $!; wait`)
	logs := &logRec{}
	KillTree(cmd.Process.Pid, logs.log)
	_ = cmd.Wait()
	waitDead(t, child)
	if msgs := logs.msgs(); len(msgs) != 0 {
		t.Fatalf("logs = %v", msgs)
	}
}

func TestKillTreeKillsAfterGraceWhenTermIsIgnored(t *testing.T) {
	old := killGrace
	killGrace = 300 * time.Millisecond
	t.Cleanup(func() { killGrace = old })
	// SIG_IGN is inherited: neither the shell nor its sleep reacts to TERM.
	cmd, child := startGroup(t, `trap '' TERM; sleep 300 & echo $!; wait`)
	start := time.Now()
	KillTree(cmd.Process.Pid, nil)
	_ = cmd.Wait()
	if waited := time.Since(start); waited < killGrace {
		t.Fatalf("died after %s, before the KILL grace", waited)
	}
	waitDead(t, child)
}

func TestKillTreeLogsWhenTheGroupIsGone(t *testing.T) {
	logs := &logRec{}
	const gone = 1 << 30 // above every pid_max: no such process group
	KillTree(gone, logs.log)
	extra, ok := logs.find("SIGTERM failed")
	if !ok || extra["pid"] != gone || extra["error"] != syscall.ESRCH.Error() {
		t.Fatalf("logs = %v %v", logs.msgs(), extra)
	}
	KillTree(gone, nil) // nil logger is fine
}

func TestKillTreeIgnoresMissingPid(t *testing.T) {
	logs := &logRec{}
	KillTree(0, logs.log)
	KillTree(-1, logs.log) // never kill(1, …) / every process
	if msgs := logs.msgs(); len(msgs) != 0 {
		t.Fatalf("logs = %v", msgs)
	}
}
