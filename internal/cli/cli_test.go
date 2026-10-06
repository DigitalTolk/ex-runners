package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/runner"
)

// env is one CLI invocation's world: a temp home, captured output, a fake
// ex server, and a controllable clock/shutdown.
type env struct {
	t        *testing.T
	home     string
	mu       sync.Mutex
	out, err []string
	srv      *httptest.Server
	// server answers: path → (status, body)
	answers  map[string][2]string
	requests []string
	shutdown chan struct{}
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, home: t.TempDir(), answers: map[string][2]string{}, shutdown: make(chan struct{})}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.requests = append(e.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		ans, ok := e.answers[r.URL.Path]
		e.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		status := 200
		_ = json.Unmarshal([]byte(ans[0]), &status)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, ans[1])
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) answer(path, status, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.answers[path] = [2]string{status, body}
}

func (e *env) deps(over func(d *Deps)) Deps {
	d := Deps{
		Home:        e.home,
		Version:     "9.9.9",
		Hostname:    "Alices-Mac.local",
		Out:         func(l string) { e.mu.Lock(); e.out = append(e.out, l); e.mu.Unlock() },
		Err:         func(l string) { e.mu.Lock(); e.err = append(e.err, l); e.mu.Unlock() },
		OpenBrowser: func(string) {},
		StartRunner: func(context.Context, runner.Config) (runner.Handle, error) {
			return &fakeHandle{}, nil
		},
		MCPEntry: runner.MCPEntry{Command: "/usr/local/bin/ex-runner", Args: []string{"mcp-server"}},
		Shutdown: e.shutdown,
		Now:      func() time.Time { return t0 },
		PID:      os.Getpid(),
		Alive:    func(int) bool { return true },
	}
	if over != nil {
		over(&d)
	}
	return d
}

func (e *env) saveCreds(over func(c *Credentials)) {
	c := sampleCreds(over)
	if c.Server == "https://ex.example" {
		c.Server = e.srv.URL
	}
	if err := saveCredentials(credentialsPath(e.home), c); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) outText() string { e.mu.Lock(); defer e.mu.Unlock(); return strings.Join(e.out, "\n") }
func (e *env) errText() string { e.mu.Lock(); defer e.mu.Unlock(); return strings.Join(e.err, "\n") }
func (e *env) reqs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.requests...)
}

type fakeHandle struct {
	mu      sync.Mutex
	stopped []string
}

func (h *fakeHandle) Stop(reason string) {
	h.mu.Lock()
	h.stopped = append(h.stopped, reason)
	h.mu.Unlock()
}
func (h *fakeHandle) Status() runner.Status {
	return runner.Status{RunnerID: "r1", Harnesses: []string{"claude"}}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHelpVersionUnknown(t *testing.T) {
	e := newEnv(t)
	for _, argv := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		if Run(argv, e.deps(nil)) != 0 {
			t.Errorf("%v: nonzero", argv)
		}
	}
	if !strings.Contains(e.outText(), "Usage:") {
		t.Error("usage not printed")
	}
	for _, argv := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		Run(argv, e.deps(nil))
	}
	if strings.Count(e.outText(), "9.9.9") != 3 {
		t.Error("version not printed 3 times")
	}
	if Run([]string{"frobnicate"}, e.deps(nil)) != 2 || !strings.Contains(e.errText(), `Unknown command "frobnicate"`) {
		t.Errorf("unknown = %s", e.errText())
	}
}

// approve makes OpenBrowser hit the CLI's callback with a code, and the fake
// server's exchange endpoint return a token.
func (e *env) approve() func(string) {
	e.answer("/api/v1/runner-tokens/exchange", "200", `{"token":"new","id":"rt2","label":"Alices-Mac","expiresAt":"2026-11-04T00:00:00Z","userName":"Alice"}`)
	return func(raw string) {
		u, _ := urlParse(raw)
		q := u.Query()
		go func() {
			res, err := http.Get("http://127.0.0.1:" + q.Get("port") + "/callback?code=c&state=" + q.Get("state"))
			if err == nil {
				_ = res.Body.Close()
			}
		}()
	}
}

func TestLoginCommand(t *testing.T) {
	e := newEnv(t)
	if Run([]string{"login"}, e.deps(nil)) != 2 || Run([]string{"login", "ftp://nope"}, e.deps(nil)) != 2 {
		t.Error("bad server must exit 2")
	}

	// Fresh login: saves the token, labels the machine from its hostname.
	open := e.approve()
	var label string
	code := Run([]string{"login", e.srv.URL}, e.deps(func(d *Deps) {
		d.OpenBrowser = func(raw string) {
			u, _ := urlParse(raw)
			label = u.Query().Get("name")
			open(raw)
		}
	}))
	if code != 0 || label != "Alices-Mac" {
		t.Fatalf("login = %d (label %q), err=%s", code, label, e.errText())
	}
	c, _ := loadCredentials(credentialsPath(e.home))
	if c.Token != "new" || c.Server != e.srv.URL {
		t.Errorf("saved = %+v", c)
	}
	if !strings.Contains(e.outText(), "Connected as Alice on "+e.srv.URL) {
		t.Errorf("out = %s", e.outText())
	}

	// Re-login retires the token it replaces.
	e.requests = nil
	if Run([]string{"login", e.srv.URL}, e.deps(func(d *Deps) { d.OpenBrowser = e.approve() })) != 0 {
		t.Fatal(e.errText())
	}
	found := false
	for _, r := range e.reqs() {
		if r == "POST /api/v1/agent/runner/revoke Bearer new" {
			found = true
		}
	}
	if !found {
		t.Errorf("old token not revoked: %v", e.reqs())
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t)
	// Cancelled in the browser: a user-facing loginError, no credentials.
	code := Run([]string{"login", e.srv.URL}, e.deps(func(d *Deps) {
		d.OpenBrowser = func(raw string) {
			u, _ := urlParse(raw)
			go func() {
				res, err := http.Get("http://127.0.0.1:" + u.Query().Get("port") + "/callback?error=access_denied&state=" + u.Query().Get("state"))
				if err == nil {
					_ = res.Body.Close()
				}
			}()
		}
	}))
	if code != 1 || !strings.Contains(e.errText(), "Sign-in was cancelled in the browser") {
		t.Errorf("cancel = %d %s", code, e.errText())
	}
	// A non-login error (the exchange can't reach the server).
	e2 := newEnv(t)
	dead := "http://127.0.0.1:1"
	code = Run([]string{"login", dead}, e2.deps(func(d *Deps) {
		d.OpenBrowser = func(raw string) {
			u, _ := urlParse(raw)
			go func() {
				res, err := http.Get("http://127.0.0.1:" + u.Query().Get("port") + "/callback?code=c&state=" + u.Query().Get("state"))
				if err == nil {
					_ = res.Body.Close()
				}
			}()
		}
	}))
	if code != 1 || !strings.HasPrefix(e2.errText(), "Sign-in failed: ") {
		t.Errorf("transport = %d %s", code, e2.errText())
	}
	// Ctrl-C during login.
	e3 := newEnv(t)
	close(e3.shutdown)
	if Run([]string{"login", e3.srv.URL}, e3.deps(nil)) != 1 || !strings.Contains(e3.errText(), "interrupted") {
		t.Errorf("interrupt = %s", e3.errText())
	}
	// The token can't be saved (home is a file).
	e4 := newEnv(t)
	blocker := filepath.Join(e4.home, "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	e4.home = blocker
	if Run([]string{"login", e4.srv.URL}, e4.deps(func(d *Deps) { d.OpenBrowser = e4.approve() })) != 1 || !strings.Contains(e4.errText(), "Couldn't save the runner token") {
		t.Errorf("save failure = %s", e4.errText())
	}
}

func TestStartRefusals(t *testing.T) {
	e := newEnv(t)
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "not connected to ex yet") {
		t.Errorf("no creds = %s", e.errText())
	}
	e.saveCreds(func(c *Credentials) { c.ExpiresAt = t0.Add(-time.Millisecond).Format(time.RFC3339Nano) })
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "ex-runner login "+e.srv.URL) {
		t.Errorf("expired = %s", e.errText())
	}
	e.saveCreds(nil)
	_ = os.WriteFile(lockPath(e.home), []byte("4242\n"), 0o600)
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "already running on this computer (pid 4242)") {
		t.Errorf("locked = %s", e.errText())
	}
	_ = os.Remove(lockPath(e.home))
	_ = os.MkdirAll(lockPath(e.home), 0o700) // a directory where the pid file goes
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "Couldn't take the runner lock") {
		t.Errorf("lock error = %s", e.errText())
	}
}

func TestStartRunsUntilShutdown(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(nil)
	h := &fakeHandle{}
	var cfg runner.Config
	done := make(chan int)
	go func() {
		done <- Run([]string{"start"}, e.deps(func(d *Deps) {
			d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) {
				cfg = c
				return h, nil
			}
		}))
	}()
	waitFor(t, func() bool { return strings.Contains(e.outText(), "ex-runner is online") })
	if cfg.BaseURL != e.srv.URL || cfg.Token() != "tok" || cfg.StateDir != StateDir(e.home) || cfg.MCPEntry.Args[0] != "mcp-server" {
		t.Errorf("cfg = %+v", cfg)
	}
	if _, err := os.Stat(lockPath(e.home)); err != nil {
		t.Error("lock not held while running")
	}
	cfg.Log("registered", map[string]any{"runnerID": "r1"})
	cfg.Log("plain", nil)
	if !strings.Contains(e.outText(), `2026-10-05T00:00:00.000Z registered {"runnerID":"r1"}`) || !strings.Contains(e.outText(), "2026-10-05T00:00:00.000Z plain") {
		t.Errorf("log lines = %s", e.outText())
	}
	close(e.shutdown)
	if code := <-done; code != 0 {
		t.Errorf("exit = %d", code)
	}
	if len(h.stopped) != 1 || h.stopped[0] != "runner_stopped" {
		t.Errorf("stopped = %v", h.stopped)
	}
	if _, err := os.Stat(lockPath(e.home)); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock not released")
	}
}

func TestStartStopsOnLostAuth(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(nil)
	h := &fakeHandle{}
	code := Run([]string{"start"}, e.deps(func(d *Deps) {
		d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) {
			go func() { c.OnUnauthorized(); c.OnUnauthorized() }() // idempotent
			return h, nil
		}
	}))
	if code != 1 || !strings.Contains(e.errText(), "This runner was disconnected") || len(h.stopped) != 1 || h.stopped[0] != "" {
		t.Errorf("lost auth = %d %v %s", code, h.stopped, e.errText())
	}
}

func TestStartRetriesUnreachableServer(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(nil)
	calls := 0
	code := 0
	done := make(chan struct{})
	go func() {
		code = Run([]string{"start"}, e.deps(func(d *Deps) {
			d.StartRetryMin, d.StartRetryMax = 2*time.Millisecond, 4*time.Millisecond
			d.StartRunner = func(context.Context, runner.Config) (runner.Handle, error) {
				calls++
				if calls < 4 {
					return nil, errors.New("connection refused")
				}
				return &fakeHandle{}, nil
			}
		}))
		close(done)
	}()
	waitFor(t, func() bool { return strings.Contains(e.outText(), "ex-runner is online") })
	close(e.shutdown)
	<-done
	want := []string{
		"Can't reach " + e.srv.URL + " yet (connection refused); retrying in 0.002s.",
		"Can't reach " + e.srv.URL + " yet (connection refused); retrying in 0.004s.",
		"Can't reach " + e.srv.URL + " yet (connection refused); retrying in 0.004s.",
	}
	if code != 0 || strings.Join(e.err, "|") != strings.Join(want, "|") {
		t.Errorf("code=%d err=%q", code, e.err)
	}

	// A dead token at start gives up at once.
	e2 := newEnv(t)
	e2.saveCreds(nil)
	if Run([]string{"start"}, e2.deps(func(d *Deps) {
		d.StartRunner = func(context.Context, runner.Config) (runner.Handle, error) {
			return nil, &api.Error{Status: 401, Code: "unauthorized", Message: "invalid token"}
		}
	})) != 1 || !strings.Contains(e2.errText(), "This runner was disconnected") {
		t.Errorf("401 at start = %s", e2.errText())
	}

	// Ctrl-C while waiting to retry.
	e3 := newEnv(t)
	e3.saveCreds(nil)
	starts := 0
	go func() {
		waitFor(t, func() bool { return strings.Contains(e3.errText(), "retrying in 60s") })
		close(e3.shutdown)
	}()
	if Run([]string{"start"}, e3.deps(func(d *Deps) {
		d.StartRetryMin, d.StartRetryMax = time.Minute, time.Minute
		d.StartRunner = func(context.Context, runner.Config) (runner.Handle, error) {
			starts++
			return nil, errors.New("down")
		}
	})) != 0 || starts != 1 {
		t.Errorf("shutdown mid-retry: starts=%d", starts)
	}
}

func TestStartRenewal(t *testing.T) {
	// Near expiry: renewed before starting, persisted, and used by the runner.
	e := newEnv(t)
	e.saveCreds(func(c *Credentials) { c.ExpiresAt = t0.Add(2 * 24 * time.Hour).Format(time.RFC3339Nano) })
	e.answer("/api/v1/agent/runner/renew", "200", `{"token":"renewed","expiresAt":"2026-11-04T00:00:00Z"}`)
	var cfg runner.Config
	go func() {
		waitFor(t, func() bool { return strings.Contains(e.outText(), "ex-runner is online") })
		close(e.shutdown)
	}()
	if Run([]string{"start"}, e.deps(func(d *Deps) {
		d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) { cfg = c; return &fakeHandle{}, nil }
	})) != 0 {
		t.Fatal(e.errText())
	}
	if cfg.Token() != "renewed" || !strings.Contains(e.outText(), "Renewed this runner's sign-in") {
		t.Errorf("renewal not applied: token=%s out=%s", cfg.Token(), e.outText())
	}
	if c, _ := loadCredentials(credentialsPath(e.home)); c.Token != "renewed" {
		t.Errorf("renewal not saved: %+v", c)
	}

	// The server says the token is dead → give up before starting.
	e2 := newEnv(t)
	e2.saveCreds(func(c *Credentials) { c.ExpiresAt = t0.Add(24 * time.Hour).Format(time.RFC3339Nano) })
	e2.answer("/api/v1/agent/runner/renew", "401", `{"error":{"code":"runner_disconnected","message":"x"}}`)
	if Run([]string{"start"}, e2.deps(nil)) != 1 || !strings.Contains(e2.errText(), "This runner was disconnected") {
		t.Errorf("dead token = %s", e2.errText())
	}

	// A transient failure keeps the current token; the periodic re-check
	// renews later; a save failure after renewal is reported, not fatal.
	e3 := newEnv(t)
	e3.saveCreds(func(c *Credentials) { c.ExpiresAt = t0.Add(24 * time.Hour).Format(time.RFC3339Nano) })
	e3.answer("/api/v1/agent/runner/renew", "500", `{}`)
	var cfg3 runner.Config
	done := make(chan int)
	go func() {
		done <- Run([]string{"start"}, e3.deps(func(d *Deps) {
			d.RenewCheck = 5 * time.Millisecond
			d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) { cfg3 = c; return &fakeHandle{}, nil }
		}))
	}()
	waitFor(t, func() bool { return strings.Contains(e3.errText(), "Couldn't renew the runner's sign-in yet (500)") })
	waitFor(t, func() bool { return strings.Contains(e3.outText(), "ex-runner is online") })
	_ = os.Chmod(e3.home, 0o500) // the renewed token can't be written back
	e3.answer("/api/v1/agent/runner/renew", "200", `{"token":"later","expiresAt":"2026-11-04T00:00:00Z"}`)
	waitFor(t, func() bool { return cfg3.Token() == "later" })
	waitFor(t, func() bool { return strings.Contains(e3.errText(), "couldn't save it") })
	_ = os.Chmod(e3.home, 0o700)
	close(e3.shutdown)
	if code := <-done; code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestStatusCommand(t *testing.T) {
	e := newEnv(t)
	if Run([]string{"status"}, e.deps(nil)) != 1 || !strings.Contains(e.outText(), "not connected to ex yet") {
		t.Error("status without creds")
	}
	e.saveCreds(nil)
	if Run([]string{"status"}, e.deps(nil)) != 0 || !strings.Contains(e.outText(), "Server:   "+e.srv.URL) || !strings.Contains(e.outText(), "valid until") || !strings.Contains(e.outText(), "Home:     "+e.home) {
		t.Errorf("status = %s", e.outText())
	}
	e.saveCreds(func(c *Credentials) { c.ExpiresAt = t0.Add(-time.Hour).Format(time.RFC3339Nano) })
	if Run([]string{"status"}, e.deps(nil)) != 1 || !strings.Contains(e.outText(), "expired on") {
		t.Error("expired status")
	}
	e.saveCreds(func(c *Credentials) { c.ExpiresAt = "2999-01-01T00:00:00Z" })
	if Run([]string{"status"}, e.deps(func(d *Deps) { d.Now = nil })) != 0 {
		t.Error("status with the real clock")
	}
}

func TestLogoutCommand(t *testing.T) {
	e := newEnv(t)
	_ = os.WriteFile(credentialsPath(e.home), []byte("{broken"), 0o600)
	if Run([]string{"logout"}, e.deps(nil)) != 0 || !strings.Contains(e.outText(), "Not connected — nothing to do.") {
		t.Error("logout without creds")
	}
	if _, err := os.Stat(credentialsPath(e.home)); !errors.Is(err, os.ErrNotExist) {
		t.Error("unreadable leftover not cleared")
	}

	e.saveCreds(nil)
	if Run([]string{"logout"}, e.deps(nil)) != 0 || !strings.Contains(e.outText(), `Disconnected "mac" from `+e.srv.URL) {
		t.Errorf("logout = %s", e.outText())
	}
	if r := e.reqs(); r[len(r)-1] != "POST /api/v1/agent/runner/revoke Bearer tok" {
		t.Errorf("revoke not called: %v", r)
	}

	e.saveCreds(nil)
	e.answer("/api/v1/agent/runner/revoke", "401", `{}`)
	if Run([]string{"logout"}, e.deps(nil)) != 0 {
		t.Error("an already-dead token still logs out cleanly")
	}

	e.saveCreds(func(c *Credentials) { c.Server = "http://127.0.0.1:1" })
	if Run([]string{"logout"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), `remove "mac" on the Runners page`) {
		t.Errorf("unreachable = %s", e.errText())
	}
	if _, err := os.Stat(credentialsPath(e.home)); !errors.Is(err, os.ErrNotExist) {
		t.Error("token not forgotten locally")
	}
}

func urlParse(s string) (*url.URL, error) { return url.Parse(s) }
