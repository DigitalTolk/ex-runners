// Package runner is the ex-runner main loop: register → claim long-poll →
// execute → heartbeat. The CLI starts it with a server, a token getter and
// paths; it has no UI of its own.
package runner

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/detect"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/sessions"
)

// Timing mirrors the backend's expectations: a missed heartbeat is how it
// detects a closed laptop; the claim long-poll stays under the server's 20s
// cap and the 30s request timeout.
var (
	heartbeatEvery = 10 * time.Second
	claimWaitSec   = 20
	claimBackoff   = 5 * time.Second
	busyPause      = time.Second
)

const maxConcurrent = 2

// seams for tests: harness detection, host facts, and the executor (so a
// test can put fake harnesses behind a real claim loop).
var (
	detectHarnesses   = detect.DetectHarnesses
	loginShellPath    = detect.LoginShellPath
	hostname          = os.Hostname
	customizeExecutor = func(*executor) {}
)

// platformName reports the OS the way the TypeScript runner did
// (process.platform), so the server sees the same values from either.
func platformName(goos string) string {
	if goos == "windows" {
		return "win32"
	}
	return goos
}

// stableRunnerID persists one ID per install so the backend sees the same
// runner across restarts (multiple machines = multiple IDs, by design).
func stableRunnerID(stateDir string) (string, error) {
	file := filepath.Join(stateDir, "runner-id")
	if raw, err := os.ReadFile(file); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

type liveRunner struct {
	cfg       Config
	log       protocol.Logger
	client    *api.Client
	identity  api.Identity
	harnesses []detect.Harness
	exec      executor

	ctx    context.Context // cancelled by Stop: aborts an in-flight claim
	cancel context.CancelFunc

	mu      sync.Mutex
	stopped bool
	active  map[string]func(reason string) // runID → kill switch (nil until the harness exists)
	runs    sync.WaitGroup
	loops   sync.WaitGroup
}

// Start registers with the server and runs the claim + heartbeat loops in
// the background. Registration errors are returned as-is: the CLI retries
// network failures and gives up on a 401.
func Start(ctx context.Context, cfg Config) (Handle, error) {
	log := cfg.Log
	if log == nil {
		log = protocol.NopLogger
	}
	runnerID, err := stableRunnerID(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	// Earlier versions wrote mcp.json (run token, connector credentials) into
	// the thread workdir the agent reads; it now lives in the private dir.
	// Sweep the old copies.
	old, _ := filepath.Glob(filepath.Join(cfg.StateDir, "threads", "*", "mcp.json"))
	for _, f := range old {
		_ = os.Remove(f)
	}
	searchPath := loginShellPath(log)
	harnesses := detectHarnesses(log)
	host, _ := hostname()
	identity := api.Identity{RunnerID: runnerID, Host: host, OS: platformName(runtime.GOOS)}
	for _, h := range harnesses {
		identity.Harnesses = append(identity.Harnesses, h.RunnerHarness)
	}
	client := api.New(cfg.BaseURL, cfg.Token)
	reg, err := client.Register(ctx, identity)
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(reg.Agents))
	for _, a := range reg.Agents {
		slugs = append(slugs, a.Slug)
	}
	log("registered", map[string]any{"runnerID": runnerID, "agents": slugs})

	runCtx, cancel := context.WithCancel(context.Background())
	r := &liveRunner{
		cfg: cfg, log: log, client: client, identity: identity, harnesses: harnesses,
		ctx: runCtx, cancel: cancel, active: map[string]func(string){},
	}
	r.exec = executor{
		api:           client,
		runnerID:      runnerID,
		baseURL:       cfg.BaseURL,
		workspaceRoot: cfg.WorkspaceRoot,
		harnesses:     harnesses,
		searchPath:    searchPath,
		mcpEntry:      cfg.MCPEntry,
		stateDir:      cfg.StateDir,
		sessions:      sessions.Open(cfg.StateDir),
		log:           log,
		onHarness:     r.setKill,
	}
	customizeExecutor(&r.exec)
	// Once, before any run starts: runs execute concurrently and share it.
	r.exec.withDefaults()
	r.loops.Add(2)
	go r.heartbeatLoop()
	go r.claimLoop()
	return r, nil
}

func (r *liveRunner) setKill(runID string, kill func(reason string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, live := r.active[runID]; live {
		r.active[runID] = kill
	}
}

func (r *liveRunner) activeIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.active))
	for id := range r.active {
		ids = append(ids, id)
	}
	return ids
}

func (r *liveRunner) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// heartbeatLoop refreshes the runner lease + every active run's lease, and
// learns which runs to kill (terminal server-side). A missed heartbeat is
// exactly how the backend detects a closed laptop — never block it on run
// work.
func (r *liveRunner) heartbeatLoop() {
	defer r.loops.Done()
	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		res, err := r.client.Heartbeat(ctx, r.identity, r.activeIDs())
		cancel()
		if err != nil {
			if r.ctx.Err() == nil {
				r.log("heartbeat failed", map[string]any{"error": err.Error()})
			}
			continue
		}
		for _, runID := range res.Kill {
			r.log("server says kill", map[string]any{"runID": runID})
			// Actually terminate the harness tree — a silent harness (no
			// event batches → no abort response) would otherwise keep
			// burning until its deadline.
			r.mu.Lock()
			kill := r.active[runID]
			r.mu.Unlock()
			if kill != nil {
				kill("server_kill")
			}
		}
	}
}

// claimLoop parks on the server's long-poll and executes what it hands out.
func (r *liveRunner) claimLoop() {
	defer r.loops.Done()
	for !r.isStopped() {
		slots := maxConcurrent - len(r.activeIDs())
		if slots <= 0 {
			if !r.sleep(busyPause) {
				return
			}
			continue
		}
		assignments, err := r.client.Claim(r.ctx, r.identity, slots, claimWaitSec)
		if err != nil {
			if r.isStopped() {
				return
			}
			if api.IsUnauthorized(err) {
				// Token revoked/expired: surface loudly and stop — only a fresh
				// `ex-runner login` helps, so never spin on 401s.
				r.log("runner token rejected; stopping", map[string]any{"error": err.Error()})
				r.mu.Lock()
				r.stopped = true
				r.mu.Unlock()
				r.cancel()
				if r.cfg.OnUnauthorized != nil {
					r.cfg.OnUnauthorized()
				}
				return
			}
			r.log("claim failed; backing off", map[string]any{"error": err.Error()})
			if !r.sleep(claimBackoff) {
				return
			}
			continue
		}
		for _, a := range assignments {
			r.log("claimed run", map[string]any{"runID": a.RunID, "agent": a.AgentName, "harness": a.Harness})
			r.mu.Lock()
			r.active[a.RunID] = nil
			r.mu.Unlock()
			r.runs.Add(1)
			go func(a protocol.Assignment) {
				defer r.runs.Done()
				defer func() {
					r.mu.Lock()
					delete(r.active, a.RunID)
					r.mu.Unlock()
				}()
				r.exec.execute(a)
			}(a)
		}
	}
}

// sleep waits d unless the runner stops first; false = stopped.
func (r *liveRunner) sleep(d time.Duration) bool {
	select {
	case <-r.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (r *liveRunner) Stop(reason string) {
	r.mu.Lock()
	r.stopped = true
	kills := make([]func(string), 0, len(r.active))
	for _, k := range r.active {
		if k != nil {
			kills = append(kills, k)
		}
	}
	r.mu.Unlock()
	r.cancel()
	if reason != "" {
		for _, kill := range kills {
			kill(reason)
		}
	}
	r.runs.Wait()
	r.loops.Wait()
}

func (r *liveRunner) Status() Status {
	names := make([]string, 0, len(r.harnesses))
	for _, h := range r.harnesses {
		names = append(names, h.Name)
	}
	return Status{RunnerID: r.identity.RunnerID, Harnesses: names, ActiveRuns: len(r.activeIDs())}
}
