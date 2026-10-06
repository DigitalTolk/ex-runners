//go:build !windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the helper processes the dev-server tests launch:
// EX_WS_HELPER_LISTEN makes this binary a slow-booting dev server,
// EX_WS_HELPER_START makes it a short-lived "MCP server" that starts a dev
// server and exits.
func TestMain(m *testing.M) {
	if spec := os.Getenv("EX_WS_HELPER_LISTEN"); spec != "" {
		helperListen(spec)
		return
	}
	if stateDir := os.Getenv("EX_WS_HELPER_START"); stateDir != "" {
		helperStart(stateDir)
		return
	}
	os.Exit(m.Run())
}

// helperListen ("<port>:<delay>") prints to both streams, waits, then listens.
func helperListen(spec string) {
	port, delay, _ := strings.Cut(spec, ":")
	d, _ := time.ParseDuration(delay)
	fmt.Println("helper booting")
	fmt.Fprintln(os.Stderr, "helper stderr")
	time.Sleep(d)
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	time.Sleep(60 * time.Second) // never outlive a broken test for long
}

// helperStart starts a dev server the way the MCP server does, prints its pid
// and exits at once.
func helperStart(stateDir string) {
	h, err := StartDevServer(context.Background(), "sleep 60", stateDir, stateDir, stateDir, "", nil, "web")
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println(h.PID)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitFor polls cond for up to 10s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("pid %d to exit", pid), func() bool { return !alive(pid) })
}

func readPid(t *testing.T, file string) int {
	t.Helper()
	var pid int
	waitFor(t, file, func() bool {
		b, err := os.ReadFile(file)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	return pid
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stopAllOnCleanup makes sure no test leaves a dev server behind.
func stopAllOnCleanup(t *testing.T, stateDir string) {
	t.Cleanup(func() { StopDevServer(stateDir, nil, "") })
}

func TestDevServerStartsDetachedLogsAndStops(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state") // created on demand
	work := t.TempDir()
	stopAllOnCleanup(t, stateDir)
	log, lines := captureLog()
	cmd := `echo "CI=$CI FORCE_COLOR=$FORCE_COLOR"; echo to-stderr 1>&2; sleep 30 & echo $! > child.pid; wait`
	h, err := StartDevServer(context.Background(), cmd, work, stateDir, stateDir, "", log, "web/app")
	if err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(stateDir, "dev-web-app.pid")
	if h.PID <= 0 || h.LogFile != filepath.Join(stateDir, "dev-web-app.log") {
		t.Fatalf("handle = %+v", h)
	}
	if b, _ := os.ReadFile(pidPath); string(b) != strconv.Itoa(h.PID) {
		t.Fatalf("pid file = %q", b)
	}
	if l, ok := findLog(lines(), "workspace: dev server started"); !ok || l.extra["pid"] != h.PID || l.extra["cmd"] != cmd {
		t.Fatalf("log = %+v", l)
	}
	// Its own process group (and session), apart from ours.
	if pg, _ := syscall.Getpgid(h.PID); pg != h.PID || pg == syscall.Getpgrp() {
		t.Fatalf("pgid = %d (pid %d, ours %d)", pg, h.PID, syscall.Getpgrp())
	}
	grandchild := readPid(t, filepath.Join(work, "child.pid")) // ran in cwd
	waitFor(t, "log output", func() bool {
		s := TailFile(h.LogFile, 0)
		return strings.Contains(s, "CI=true FORCE_COLOR=0\n") && strings.Contains(s, "to-stderr\n")
	})

	StopDevServer(stateDir, log, "web/app")
	waitGone(t, h.PID)
	waitGone(t, grandchild) // the whole group, not just the shell
	if exists(pidPath) {
		t.Fatal("pid file left behind")
	}
	if l, ok := findLog(lines(), "workspace: dev server stopped"); !ok || l.extra["pid"] != h.PID || l.extra["file"] != "dev-web-app.pid" {
		t.Fatalf("log = %+v", l)
	}

	// A restart appends to the same log.
	h2, err := StartDevServer(context.Background(), "echo second run; sleep 30", work, stateDir, stateDir, "", nil, "web/app")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second run output", func() bool { return strings.Contains(TailFile(h2.LogFile, 0), "second run") })
	if s := TailFile(h2.LogFile, 0); !strings.HasPrefix(s, "CI=true") {
		t.Fatalf("log was truncated: %q", s)
	}
}

func TestDevServerRestartsOnlyItsOwnServerAndStopSweepsAll(t *testing.T) {
	stateDir := t.TempDir()
	stopAllOnCleanup(t, stateDir)
	ctx := context.Background()
	api, err := StartDevServer(ctx, "sleep 30", stateDir, stateDir, stateDir, "", nil, "api")
	if err != nil {
		t.Fatal(err)
	}
	web, err := StartDevServer(ctx, "sleep 30", stateDir, stateDir, stateDir, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if web.LogFile != filepath.Join(stateDir, "dev-server.log") || !exists(filepath.Join(stateDir, "dev-server.pid")) {
		t.Fatalf("default name: %+v", web)
	}
	web2, err := StartDevServer(ctx, "sleep 30", stateDir, stateDir, stateDir, "", nil, "server")
	if err != nil {
		t.Fatal(err)
	}
	waitGone(t, web.PID)
	if !alive(api.PID) || !alive(web2.PID) {
		t.Fatal("restart touched another server")
	}
	log, lines := captureLog()
	StopDevServer(stateDir, log, "")
	waitGone(t, api.PID)
	waitGone(t, web2.PID)
	n := 0
	for _, l := range lines() {
		if l.msg == "workspace: dev server stopped" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("stopped %d servers: %+v", n, lines())
	}
}

func TestDevServerWaitsForThePortToAcceptConnections(t *testing.T) {
	stateDir := t.TempDir()
	stopAllOnCleanup(t, stateDir)
	origPoll := devPollInterval
	t.Cleanup(func() { devPollInterval = origPoll })
	devPollInterval = 50 * time.Millisecond
	port := freePort(t)
	t.Setenv("EX_WS_HELPER_LISTEN", fmt.Sprintf("%d:600ms", port))
	start := time.Now()
	h, err := StartDevServer(context.Background(), shellQuote(os.Args[0])+" -test.run=^$", t.TempDir(), stateDir, stateDir, fmt.Sprintf("http://localhost:%d/bookings", port), nil, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Fatalf("returned after %v, before the server listened", d)
	}
	if !portOpen(port) {
		t.Fatal("port not open")
	}
	if s := TailFile(h.LogFile, 0); !strings.Contains(s, "helper booting") || !strings.Contains(s, "helper stderr") {
		t.Fatalf("log = %q", s)
	}
}

func TestDevServerReadinessTimeoutCarriesTheLogTail(t *testing.T) {
	stateDir := t.TempDir()
	stopAllOnCleanup(t, stateDir)
	origTimeout, origPoll := devReadyTimeout, devPollInterval
	t.Cleanup(func() { devReadyTimeout, devPollInterval = origTimeout, origPoll })
	devReadyTimeout, devPollInterval = 300*time.Millisecond, 50*time.Millisecond
	port := freePort(t)
	cmd := "printf 'x%.0s' $(seq 1 2000); echo; echo booting; sleep 30"
	h, err := StartDevServer(context.Background(), cmd, t.TempDir(), stateDir, stateDir, fmt.Sprintf("http://127.0.0.1:%d", port), nil, "api")
	if err == nil {
		t.Fatal("want a readiness timeout")
	}
	tail := TailFile(h.LogFile, 1500)
	if err.Error() != fmt.Sprintf("dev server did not start listening on port %d within 0.3s. Log tail:\n%s", port, tail) || !strings.HasSuffix(tail, "booting\n") || len(tail) != 1500 {
		t.Fatalf("err = %q", err)
	}
	// The server keeps running (the TS left it too); stop sweeps it.
	if !alive(h.PID) {
		t.Fatal("server died")
	}
	StopDevServer(stateDir, nil, "api")
	waitGone(t, h.PID)
	// Production wording.
	if got := notListeningError(5273, 90*time.Second, "tail").Error(); got != "dev server did not start listening on port 5273 within 90s. Log tail:\ntail" {
		t.Fatalf("got %q", got)
	}
}

func TestDevServerWaitEndsWithTheContext(t *testing.T) {
	stateDir := t.TempDir()
	stopAllOnCleanup(t, stateDir)
	origPoll := devPollInterval
	t.Cleanup(func() { devPollInterval = origPoll })
	devPollInterval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	h, err := StartDevServer(ctx, "sleep 30", t.TempDir(), stateDir, stateDir, fmt.Sprintf("http://127.0.0.1:%d", freePort(t)), nil, "api")
	if !errors.Is(err, context.DeadlineExceeded) || h.PID <= 0 {
		t.Fatalf("got %+v, %v", h, err)
	}
}

func TestDevServerStartFailures(t *testing.T) {
	ctx := context.Background()
	// The working directory does not exist: the shell cannot start.
	stateDir := t.TempDir()
	_, err := StartDevServer(ctx, "sleep 30", filepath.Join(stateDir, "missing"), stateDir, stateDir, "", nil, "web")
	if err == nil || err.Error() != "dev server failed to start" || errors.Unwrap(err) == nil {
		t.Fatalf("err = %v", err)
	}
	if exists(filepath.Join(stateDir, "dev-web.pid")) {
		t.Fatal("pid file written for a server that never started")
	}
	// The state dir cannot be made.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	if _, err := StartDevServer(ctx, "sleep 30", t.TempDir(), filepath.Join(file, "state"), filepath.Join(file, "state"), "", nil, "web"); err == nil {
		t.Fatal("want mkdir error")
	}
	// The log cannot be opened.
	logDir := t.TempDir()
	_ = os.Mkdir(filepath.Join(logDir, "dev-web.log"), 0o755)
	if _, err := StartDevServer(ctx, "sleep 30", t.TempDir(), logDir, t.TempDir(), "", nil, "web"); err == nil {
		t.Fatal("want log open error")
	}
	// The pid cannot be recorded: the server is killed, not orphaned.
	pidDir := t.TempDir()
	_ = os.Mkdir(filepath.Join(pidDir, "dev-web.pid"), 0o755)
	marker := fmt.Sprintf("sleep 31.%d", time.Now().UnixNano()%1000000)
	if _, err := StartDevServer(ctx, marker, t.TempDir(), t.TempDir(), pidDir, "", nil, "web"); err == nil {
		t.Fatal("want pid write error")
	}
	waitFor(t, "the unrecorded server to die", func() bool {
		return exec.Command("pgrep", "-f", marker).Run() != nil
	})
}

func TestStopDevServerSweepIgnoresJunk(t *testing.T) {
	// No state dir at all: nothing to do.
	StopDevServer(filepath.Join(t.TempDir(), "missing"), nil, "")
	// A state "dir" that is a file.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	StopDevServer(file, nil, "")

	stateDir := t.TempDir()
	write := func(name, content string) { _ = os.WriteFile(filepath.Join(stateDir, name), []byte(content), 0o644) }
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	write("dev-garbage.pid", "not a pid")
	write("dev-zero.pid", "0")
	write("dev-.pid", " \n")
	write("dev-dead.pid", strconv.Itoa(dead.Process.Pid)+"\n")
	write("other.pid", "123")
	write("dev-keep.log", "log")
	_ = os.Mkdir(filepath.Join(stateDir, "dev-dir.pid"), 0o755)
	log, lines := captureLog()
	StopDevServer(stateDir, log, "")
	for _, gone := range []string{"dev-garbage.pid", "dev-zero.pid", "dev-.pid", "dev-dead.pid"} {
		if exists(filepath.Join(stateDir, gone)) {
			t.Errorf("%s not removed", gone)
		}
	}
	for _, kept := range []string{"other.pid", "dev-keep.log", "dev-dir.pid"} {
		if !exists(filepath.Join(stateDir, kept)) {
			t.Errorf("%s removed", kept)
		}
	}
	if len(lines()) != 0 {
		t.Fatalf("nothing was running, yet: %+v", lines())
	}
	// A named stop of a server that was never started.
	StopDevServer(stateDir, nil, "nope")
}

func TestDevServerOutlivesTheProcessThatStartedIt(t *testing.T) {
	stateDir := t.TempDir()
	stopAllOnCleanup(t, stateDir)
	starter := exec.Command(os.Args[0], "-test.run=^$")
	starter.Env = append(os.Environ(), "EX_WS_HELPER_START="+stateDir)
	out, err := starter.Output()
	if err != nil {
		t.Fatalf("starter: %v\n%s", err, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("starter said %q", out)
	}
	// The starter is gone; the server is not.
	time.Sleep(100 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("dev server died with its starter")
	}
	// A different process (this one) stops it through the pid file.
	StopDevServer(stateDir, nil, "web")
	waitGone(t, pid)
}

func TestDevServerGetsNoRunnerSecretsAndRefusesASymlinkedLog(t *testing.T) {
	// The MCP server that starts dev servers holds these in its environment.
	t.Setenv("EX_RUN_TOKEN", "run-s3cret")
	t.Setenv("EX_CONNECTORS", `[{"token":"conn-s3cret"}]`)
	logDir, pidDir := t.TempDir(), t.TempDir()
	t.Cleanup(func() { StopDevServer(pidDir, nil, "") })
	h, err := StartDevServer(context.Background(), "env; sleep 30", t.TempDir(), logDir, pidDir, "", nil, "web")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the env dump", func() bool { return strings.Contains(TailFile(h.LogFile, 100000), "CI=true") })
	if out := TailFile(h.LogFile, 100000); strings.Contains(out, "s3cret") {
		t.Fatal("the dev server inherited the runner's EX_* secrets")
	}
	// The pid file lives only in the private dir.
	if !exists(filepath.Join(pidDir, "dev-web.pid")) || exists(filepath.Join(logDir, "dev-web.pid")) {
		t.Fatal("pid file not confined to pidDir")
	}
	if st, _ := os.Stat(h.LogFile); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v", st.Mode().Perm())
	}
	// A log path planted as a symlink is refused, not followed.
	StopDevServer(pidDir, nil, "web")
	_ = os.Remove(h.LogFile)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.Symlink(victim, h.LogFile); err != nil {
		t.Fatal(err)
	}
	if _, err := StartDevServer(context.Background(), "sleep 30", t.TempDir(), logDir, pidDir, "", nil, "web"); err == nil {
		t.Fatal("followed a symlinked log")
	}
	if exists(victim) {
		t.Fatal("wrote through the symlink")
	}
}
