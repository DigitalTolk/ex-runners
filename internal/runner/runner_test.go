package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/detect"
	"github.com/DigitalTolk/ex-runners/internal/harness"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// withLoopSeams makes the loop fast and puts fake harnesses behind it.
func withLoopSeams(t *testing.T, h *fakeHarness) {
	t.Helper()
	origHB, origBackoff, origBusy, origWait := heartbeatEvery, claimBackoff, busyPause, claimWaitSec
	origDetect, origPath, origHost, origCustom := detectHarnesses, loginShellPath, hostname, customizeExecutor
	heartbeatEvery, claimBackoff, busyPause = 20*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	detectHarnesses = func(protocol.Logger) []detect.Harness {
		return []detect.Harness{{RunnerHarness: protocol.RunnerHarness{Name: "claude", Version: "2.0", Authed: true}, Path: "/bin/claude"}}
	}
	loginShellPath = func(protocol.Logger) string { return "/usr/bin" }
	hostname = func() (string, error) { return "test-mac", nil }
	customizeExecutor = func(e *executor) { e.runClaude, e.runCodex = h.run, h.run }
	t.Cleanup(func() {
		heartbeatEvery, claimBackoff, busyPause, claimWaitSec = origHB, origBackoff, origBusy, origWait
		detectHarnesses, loginShellPath, hostname, customizeExecutor = origDetect, origPath, origHost, origCustom
	})
}

// claimQueue hands out the queued assignments one claim at a time, then 204s.
func claimQueue(f *fakeServer, queue ...protocol.Assignment) {
	var mu sync.Mutex
	f.on("/api/v1/agent/runner/claim", func(map[string]any) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) == 0 {
			time.Sleep(5 * time.Millisecond)
			return 204, ""
		}
		a := queue[0]
		queue = queue[1:]
		raw := `{"assignments":[` + mustJSON(a) + `]}`
		return 200, raw
	})
}

func startRunner(t *testing.T, f *fakeServer, onUnauth func()) Handle {
	t.Helper()
	f.on("/api/v1/agent/runner/register", func(map[string]any) (int, string) {
		return 200, `{"runnerID":"x","agents":[{"id":"a-gg","displayName":"gg","slug":"gg"}],"leaseSec":60}`
	})
	var logs []string
	var mu sync.Mutex
	h, err := Start(context.Background(), Config{
		BaseURL:  f.srv.URL,
		Token:    func() string { return "runner-tok" },
		StateDir: t.TempDir(),
		MCPEntry: MCPEntry{Command: "/opt/ex-runner", Args: []string{"mcp-server"}},
		Log: func(msg string, _ map[string]any) {
			mu.Lock()
			logs = append(logs, msg)
			mu.Unlock()
		},
		OnUnauthorized: onUnauth,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLoopClaimsExecutesAndStops(t *testing.T) {
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("4")}}
	withLoopSeams(t, h)
	f := newFakeServer(t)
	claimQueue(f, assignment(nil))
	r := startRunner(t, f, nil)

	eventually(t, "the run to complete", func() bool { return len(f.callsTo("/api/v1/agent/runner/runs/run1/complete")) == 1 })
	reg := f.callsTo("/api/v1/agent/runner/register")[0]
	if reg.body["host"] != "test-mac" || reg.body["os"] == "" || reg.auth != "Bearer runner-tok" {
		t.Errorf("register = %+v", reg)
	}
	claim := f.callsTo("/api/v1/agent/runner/claim")[0]
	if claim.body["max"] != float64(2) || claim.body["harnesses"].([]any)[0] != "claude" {
		t.Errorf("claim = %+v", claim.body)
	}
	eventually(t, "a heartbeat", func() bool { return len(f.callsTo("/api/v1/agent/runner/heartbeat")) > 0 })
	st := r.Status()
	if st.RunnerID == "" || st.Harnesses[0] != "claude" {
		t.Errorf("status = %+v", st)
	}
	r.Stop("")
}

func TestServerKillAndShutdownKill(t *testing.T) {
	hang := func(harness.EventSink, *fakeRun, harness.RunOptions) {}
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){hang, hang}}
	withLoopSeams(t, h)
	f := newFakeServer(t)
	claimQueue(f, assignment(nil), assignment(func(a *protocol.Assignment) { a.RunID = "run2" }))
	killed := false
	var mu sync.Mutex
	f.on("/api/v1/agent/runner/heartbeat", func(body map[string]any) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		active, _ := body["activeRunIDs"].([]any)
		for _, id := range active {
			if id == "run1" && !killed {
				killed = true
				return 200, `{"kill":["run1","unknown-run"]}`
			}
		}
		return 200, `{"kill":null}`
	})
	r := startRunner(t, f, nil)
	eventually(t, "the server kill", func() bool { return len(f.callsTo("/api/v1/agent/runner/runs/run1/fail")) == 1 })
	if f.callsTo("/api/v1/agent/runner/runs/run1/fail")[0].body["reason"] != "server_kill" {
		t.Errorf("run1 fail = %+v", f.callsTo("/api/v1/agent/runner/runs/run1/fail"))
	}
	eventually(t, "run2 to start", func() bool { return len(h.got()) == 2 })
	r.Stop("runner_stopped")
	if c := f.callsTo("/api/v1/agent/runner/runs/run2/fail"); len(c) != 1 || c[0].body["reason"] != "runner_stopped" {
		t.Errorf("run2 fail = %+v", c)
	}
}

func TestLoopStopsOnUnauthorizedAndBacksOff(t *testing.T) {
	withLoopSeams(t, &fakeHarness{})
	f := newFakeServer(t)
	calls := 0
	var mu sync.Mutex
	f.on("/api/v1/agent/runner/claim", func(map[string]any) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 500, `{}` // transient: backed off and retried
		}
		return 401, `{"error":{"code":"runner_disconnected","message":"runner: this runner was disconnected"}}`
	})
	f.on("/api/v1/agent/runner/heartbeat", func(map[string]any) (int, string) { return 500, `{}` })
	lost := make(chan struct{})
	r := startRunner(t, f, func() { close(lost) })
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("OnUnauthorized never called")
	}
	r.Stop("")
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("claim calls = %d", calls)
	}
}

func TestLoopWaitsWhenSlotsAreFull(t *testing.T) {
	hang := func(harness.EventSink, *fakeRun, harness.RunOptions) {}
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){hang, hang}}
	withLoopSeams(t, h)
	f := newFakeServer(t)
	claimQueue(f, assignment(nil), assignment(func(a *protocol.Assignment) { a.RunID = "run2" }))
	r := startRunner(t, f, nil)
	eventually(t, "both runs to start", func() bool { return len(h.got()) == 2 })
	n := len(f.callsTo("/api/v1/agent/runner/claim"))
	time.Sleep(60 * time.Millisecond)
	if more := len(f.callsTo("/api/v1/agent/runner/claim")); more != n {
		t.Errorf("claimed while full: %d → %d", n, more)
	}
	r.Stop("runner_stopped")
}

func TestStartFailures(t *testing.T) {
	withLoopSeams(t, &fakeHarness{})
	f := newFakeServer(t)
	f.on("/api/v1/agent/runner/register", func(map[string]any) (int, string) { return 401, `{}` })
	if _, err := Start(context.Background(), Config{BaseURL: f.srv.URL, Token: func() string { return "t" }, StateDir: t.TempDir()}); err == nil {
		t.Error("expected register failure")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if _, err := Start(context.Background(), Config{BaseURL: f.srv.URL, Token: func() string { return "t" }, StateDir: filepath.Join(blocker, "state")}); err == nil {
		t.Error("expected state dir failure")
	}
}

func TestStableRunnerIDAndPlatform(t *testing.T) {
	dir := t.TempDir()
	a, err := stableRunnerID(dir)
	if err != nil || len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("id = %q %v", a, err)
	}
	if b, _ := stableRunnerID(dir); b != a {
		t.Error("id not stable")
	}
	_ = os.WriteFile(filepath.Join(dir, "runner-id"), []byte("  \n"), 0o600)
	if c, _ := stableRunnerID(dir); c == a || c == "" {
		t.Error("blank id file must be replaced")
	}
	ro := t.TempDir()
	_ = os.Chmod(ro, 0o500)
	defer func() { _ = os.Chmod(ro, 0o700) }()
	if _, err := stableRunnerID(ro); err == nil {
		t.Error("expected write failure")
	}
	if platformName("windows") != "win32" || platformName("darwin") != "darwin" {
		t.Error("platformName")
	}
}

func mustJSON(v any) string {
	raw, err := jsonMarshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestHeartbeatFailureIsLoggedAndClaimBackoffStops(t *testing.T) {
	withLoopSeams(t, &fakeHarness{})
	claimBackoff = time.Hour // Stop must cut the backoff short
	f := newFakeServer(t)
	f.on("/api/v1/agent/runner/heartbeat", func(map[string]any) (int, string) { return 500, `{}` })
	f.on("/api/v1/agent/runner/claim", func(map[string]any) (int, string) { return 500, `{}` })
	r := startRunner(t, f, nil)
	eventually(t, "two heartbeats", func() bool { return len(f.callsTo("/api/v1/agent/runner/heartbeat")) >= 2 })
	eventually(t, "a failed claim", func() bool { return len(f.callsTo("/api/v1/agent/runner/claim")) >= 1 })
	done := make(chan struct{})
	go func() { r.Stop(""); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung in the claim backoff")
	}
}

func TestStartSweepsMCPConfigsLeftInWorkdirs(t *testing.T) {
	withLoopSeams(t, &fakeHarness{})
	state := t.TempDir()
	stale := filepath.Join(state, "threads", "abc", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(stale), 0o700)
	_ = os.WriteFile(stale, []byte(`{"env":{"EX_RUN_TOKEN":"old"}}`), 0o600)
	keep := filepath.Join(state, "threads", "abc", "notes.md")
	_ = os.WriteFile(keep, []byte("x"), 0o600)
	f := newFakeServer(t)
	f.on("/api/v1/agent/runner/register", func(map[string]any) (int, string) { return 200, `{"runnerID":"x","agents":[]}` })
	h, err := Start(context.Background(), Config{BaseURL: f.srv.URL, Token: func() string { return "t" }, StateDir: state, MCPEntry: MCPEntry{Command: "/x"}})
	if err != nil {
		t.Fatal(err)
	}
	h.Stop("")
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale mcp.json survived start")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("swept more than mcp.json")
	}
}
