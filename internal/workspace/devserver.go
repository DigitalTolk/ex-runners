package workspace

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// Dev-server supervisor -----------------------------------------------------

// DevServerHandle is a started dev server.
type DevServerHandle struct {
	PID     int    `json:"pid"`
	LogFile string `json:"logFile"`
}

var (
	// devReadyTimeout and devPollInterval pace the wait for the test URL's
	// port; portProbeTimeout bounds one connection attempt.
	devReadyTimeout  = 90 * time.Second
	devPollInterval  = 1500 * time.Millisecond
	portProbeTimeout = time.Second
)

// One dev server per repo (backend + frontend); pid files are named so a
// re-publish restarts only its own server and a stop sweeps them all. (TS
// fell back to "server" for an empty name; StartDevServer defaults the name
// before it gets here and StopDevServer sweeps on "", so it never is.)
func devFileBase(name string) string {
	return "dev-" + replaceUnsafe(name)
}

func pidFile(pidDir, name string) string {
	return filepath.Join(pidDir, devFileBase(name)+".pid")
}

// devServerStartError is a dev command that could not be launched. Its
// message is the TS runner's; the cause stays reachable via errors.Unwrap.
type devServerStartError struct{ cause error }

func (e *devServerStartError) Error() string { return "dev server failed to start" }
func (e *devServerStartError) Unwrap() error { return e.cause }

// StartDevServer launches the project's dev command detached (own session and
// process group) so it outlives the run — and the process that started it —
// while the requester tests; stdout/err go to a log in logDir (which the agent
// reads). The pid file goes to pidDir, which must be private to the runner:
// StopDevServer signals whatever group a pid file names. A previous server
// under the same name is stopped first. When url names a port, waits (up to
// 90s) for it to accept connections. name "" is "server"; url "" means no
// wait. On a readiness timeout or ctx end the server keeps running and its
// handle comes back with the error.
func StartDevServer(ctx context.Context, cmd, cwd, logDir, pidDir, url string, log protocol.Logger, name string) (DevServerHandle, error) {
	log = logger(log)
	if name == "" {
		name = "server"
	}
	StopDevServer(pidDir, log, name)
	for _, dir := range []string{logDir, pidDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return DevServerHandle{}, err
		}
	}
	logFile := filepath.Join(logDir, devFileBase(name)+".log")
	// No-follow: the log dir is agent-writable, and a planted symlink would
	// aim the server's output at any file the user can write.
	out, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND|oNoFollow, 0o600)
	if err != nil {
		return DevServerHandle{}, err
	}
	child := shellCommand(cmd)
	child.Dir = cwd
	child.Stdout, child.Stderr = out, out // stdin: /dev/null
	child.Env = setEnv(setEnv(withoutExEnv(os.Environ()), "CI", "true"), "FORCE_COLOR", "0")
	err = child.Start()
	_ = out.Close()
	if err != nil {
		return DevServerHandle{}, &devServerStartError{cause: err}
	}
	// Reap it should it exit while we live; it keeps running after we exit.
	go func() { _ = child.Wait() }()
	pid := child.Process.Pid
	if err := os.WriteFile(pidFile(pidDir, name), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		_ = killGroup(pid) // an unrecorded server could never be stopped
		return DevServerHandle{}, err
	}
	log("workspace: dev server started", map[string]any{"pid": pid, "cmd": cmd})
	h := DevServerHandle{PID: pid, LogFile: logFile}

	port := PortOf(url)
	if port == 0 {
		return h, nil
	}
	deadline := time.Now().Add(devReadyTimeout)
	for time.Now().Before(deadline) {
		if portOpen(port) {
			return h, nil
		}
		select {
		case <-ctx.Done():
			return h, ctx.Err()
		case <-time.After(devPollInterval):
		}
	}
	return h, notListeningError(port, devReadyTimeout, TailFile(logFile, 1500))
}

// notListeningError is the readiness timeout ("… within 90s. Log tail: …").
func notListeningError(port int, timeout time.Duration, tail string) error {
	return fmt.Errorf("dev server did not start listening on port %d within %ss. Log tail:\n%s",
		port, strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64), tail)
}

// devPidFileRe is the TS /^dev-.*\.pid$/ ("." stops at JS line terminators).
var devPidFileRe = regexp.MustCompile(`^dev-[^\n\r\x{2028}\x{2029}]*\.pid$`)

// StopDevServer kills the recorded dev server(s) — the whole process group —
// one by name, or every dev-*.pid in pidDir when name is "".
func StopDevServer(pidDir string, log protocol.Logger, name string) {
	log = logger(log)
	var files []string
	if name != "" {
		files = []string{pidFile(pidDir, name)}
	} else {
		entries, err := os.ReadDir(pidDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if devPidFileRe.MatchString(e.Name()) {
				files = append(files, filepath.Join(pidDir, e.Name()))
			}
		}
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		_ = os.Remove(file)
		pid, _ := strconv.Atoi(textutil.JSTrim(string(data)))
		// TS skipped only 0/NaN; a pid ≤ 1 is never a dev server we started,
		// and signalling group -1 would hit every process we may signal.
		if pid <= 1 {
			continue
		}
		if killGroup(pid) == nil {
			log("workspace: dev server stopped", map[string]any{"pid": pid, "file": filepath.Base(file)})
		} // else: already gone
	}
}

// withoutExEnv drops the credentials the MCP server that starts dev servers
// holds in its environment (the run token, connector and git credentials);
// the project's server has no business with them.
func withoutExEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "EX_RUN_TOKEN" && k != "EX_CONNECTORS" && k != "EX_GIT_TOKEN" {
			out = append(out, kv)
		}
	}
	return out
}

// PortOf reads the port out of a test URL: the explicit port, else 443 for
// https and 80 otherwise; 0 for "" or an unparsable URL.
func PortOf(url string) int {
	u, ok := parseWebURL(url)
	if !ok {
		return 0
	}
	if u.port != "" {
		n, _ := strconv.Atoi(u.port) // validated by parseWebURL
		return n
	}
	if u.protocol == "https:" {
		return 443
	}
	return 80
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), portProbeTimeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// TailFile returns the end of a file — the last `bytes` characters (TS:
// data.slice(-bytes), so 0 means all of it) — or "" when unreadable.
func TailFile(file string, bytes int) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	s := string(data)
	if bytes > 0 {
		return textutil.Tail(s, bytes)
	}
	return textutil.Tail(s, textutil.Len(s)+bytes) // slice(-0) is everything; slice(n) drops n
}
