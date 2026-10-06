// Package cli is the ex-runner command line. Everything process-shaped
// (argv, stdout, signals, spawning a browser, the runner's main loop)
// arrives through Deps, so each command is testable end to end without a
// real process.
package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/runner"
)

// Deps is everything a command needs from the outside world.
type Deps struct {
	Home        string
	Version     string
	Hostname    string
	Out, Err    func(line string)
	OpenBrowser func(url string)
	StartRunner func(ctx context.Context, cfg runner.Config) (runner.Handle, error)
	MCPEntry    runner.MCPEntry
	// Shutdown is closed on the first SIGINT/SIGTERM.
	Shutdown <-chan struct{}
	Now      func() time.Time
	PID      int
	// Alive reports whether a pid is a running process (the runner lock).
	Alive func(pid int) bool
	// Backoff bounds while the server is unreachable at start, and the
	// renewal re-check interval (tests shrink them).
	StartRetryMin, StartRetryMax, RenewCheck time.Duration
	LoginTimeout                             time.Duration
	HTTP                                     *http.Client
}

// Usage is the help text.
const Usage = `ex-runner — runs your ex agents (Claude Code, Codex) on this computer.

Usage:
  ex-runner login <server-url>   Connect this computer to your ex account
  ex-runner start                Bring your agents online here (Ctrl-C to stop)
  ex-runner status               Show the account and server this runner uses
  ex-runner logout               Disconnect this computer and forget its token
  ex-runner --version            Print the version

Files live in $EX_RUNNER_HOME (default: ~/.ex-runner).`

const notSignedIn = "This computer is not connected to ex yet. Run: ex-runner login <server-url>"

// A server that can't be reached at start (a laptop logging in before
// Wi-Fi is up, the server mid-deploy) is retried with backoff instead of
// ending the process — under `brew services` an exit would leave agents
// offline until the next login.
const (
	startRetryMin = 2 * time.Second
	startRetryMax = 60 * time.Second
)

func disconnected(server string) string {
	return "This runner was disconnected — it was removed on the Runners page, its account was deactivated, or its sign-in expired.\n" +
		"Connect it again with: ex-runner login " + server
}

func (d *Deps) withDefaults() {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.StartRetryMin == 0 {
		d.StartRetryMin, d.StartRetryMax = startRetryMin, startRetryMax
	}
	if d.RenewCheck == 0 {
		d.RenewCheck = renewCheck
	}
	if d.LoginTimeout == 0 {
		d.LoginTimeout = loginTimeout
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
}

// Run executes one command line and returns the process exit code.
func Run(argv []string, d Deps) int {
	d.withDefaults()
	cmd := ""
	if len(argv) > 0 {
		cmd = argv[0]
	}
	switch cmd {
	case "", "help", "--help", "-h":
		d.Out(Usage)
		return 0
	case "version", "--version", "-v":
		d.Out(d.Version)
		return 0
	case "login":
		arg := ""
		if len(argv) > 1 {
			arg = argv[1]
		}
		return cmdLogin(arg, &d)
	case "start":
		return cmdStart(&d)
	case "status":
		return cmdStatus(&d)
	case "logout":
		return cmdLogout(&d)
	default:
		d.Err(fmt.Sprintf("Unknown command %q.\n\n%s", cmd, Usage))
		return 2
	}
}

func (d *Deps) client(server, token string) *api.Client {
	c := api.New(server, func() string { return token })
	c.HTTP = d.HTTP
	return c
}

func cmdLogin(arg string, d *Deps) int {
	server, ok := normalizeServer(arg)
	if !ok {
		d.Err("Usage: ex-runner login <server-url>    e.g. ex-runner login https://ex.example.com")
		return 2
	}
	file := credentialsPath(d.Home)
	previous, hadPrevious := loadCredentials(file)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-d.Shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	creds, err := login(ctx, server, loginDeps{
		openBrowser: d.OpenBrowser,
		out:         d.Out,
		label:       machineLabel(d.Hostname),
		timeout:     d.LoginTimeout,
		client:      d.HTTP,
	})
	if err != nil {
		if le, ok := err.(*loginError); ok {
			d.Err(le.msg)
		} else {
			d.Err("Sign-in failed: " + err.Error())
		}
		return 1
	}
	if err := saveCredentials(file, creds); err != nil {
		d.Err("Couldn't save the runner token: " + err.Error())
		return 1
	}
	// The new token replaces the old one on this machine; retire the old one
	// so it doesn't linger on the Runners page. Best effort — it may already
	// be revoked, expired, or on a server that's gone.
	if hadPrevious {
		_ = d.client(previous.Server, previous.Token).Revoke(context.Background())
	}
	d.Out(fmt.Sprintf("Connected as %s on %s. This computer shows as \"%s\" on your Runners page.", creds.User, creds.Server, creds.Label))
	d.Out("Bring your agents online with: ex-runner start")
	return 0
}

func cmdStart(d *Deps) int {
	file := credentialsPath(d.Home)
	loaded, ok := loadCredentials(file)
	if !ok {
		d.Err(notSignedIn)
		return 1
	}
	creds := loaded
	if isExpired(creds, d.Now()) {
		d.Err(disconnected(creds.Server))
		return 1
	}
	lk, holder, err := acquireLock(lockPath(d.Home), d.PID, d.Alive)
	if err != nil {
		d.Err("Couldn't take the runner lock: " + err.Error())
		return 1
	}
	if lk == nil {
		d.Err(fmt.Sprintf("ex-runner is already running on this computer (pid %d).", holder))
		return 1
	}
	defer lk.release()

	// One mutable token, read by every request through the getter. Renewal
	// runs on a timer goroutine while the runner's own goroutines read it.
	tokens := newTokenBox(creds)
	client := api.New(creds.Server, tokens.token)
	client.HTTP = d.HTTP

	// renew answers false only when the server says the token is dead; any
	// other failure (network, 5xx) keeps the current token and retries on
	// the next check — it is still valid for days.
	renew := func() bool {
		cur := tokens.get()
		if !needsRenewal(cur, d.Now()) {
			return true
		}
		fresh, err := client.Renew(context.Background())
		if err != nil {
			if api.IsUnauthorized(err) {
				return false
			}
			d.Err(fmt.Sprintf("Couldn't renew the runner's sign-in yet (%s); will retry later.", err.Error()))
			return true
		}
		next := *cur
		next.Token, next.ExpiresAt = fresh.Token, fresh.ExpiresAt
		tokens.set(&next)
		if err := saveCredentials(file, &next); err != nil {
			d.Err("Renewed the runner's sign-in but couldn't save it: " + err.Error())
		}
		d.Out(fmt.Sprintf("Renewed this runner's sign-in (valid until %s).", next.ExpiresAt))
		return true
	}
	if !renew() {
		d.Err(disconnected(creds.Server))
		return 1
	}

	unauthorized := make(chan struct{})
	var unauthOnce sync.Once
	cfg := runner.Config{
		BaseURL:  creds.Server,
		Token:    tokens.token,
		StateDir: StateDir(d.Home),
		MCPEntry: d.MCPEntry,
		Log: func(msg string, extra map[string]any) {
			line := d.Now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + msg
			if extra != nil {
				line += " " + jsonLine(extra)
			}
			d.Out(line)
		},
		OnUnauthorized: func() { unauthOnce.Do(func() { close(unauthorized) }) },
	}

	var handle runner.Handle
	delay := d.StartRetryMin
	for handle == nil {
		h, err := d.StartRunner(context.Background(), cfg)
		if err == nil {
			handle = h
			break
		}
		if api.IsUnauthorized(err) {
			d.Err(disconnected(creds.Server))
			return 1
		}
		d.Err(fmt.Sprintf("Can't reach %s yet (%s); retrying in %s.", creds.Server, err.Error(), seconds(delay)))
		select {
		case <-d.Shutdown:
			return 0
		case <-time.After(delay):
		}
		delay = min(delay*2, d.StartRetryMax)
	}
	d.Out(fmt.Sprintf("ex-runner is online for %s on %s. Press Ctrl-C to stop.", creds.User, creds.Server))

	ticker := time.NewTicker(d.RenewCheck)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			renew()
		case <-unauthorized:
			d.Err(disconnected(creds.Server))
			handle.Stop("")
			return 1
		case <-d.Shutdown:
			d.Out("Stopping — agents on this computer go offline.")
			handle.Stop("runner_stopped")
			return 0
		}
	}
}

// seconds renders a delay like the TS CLI did ("2s", "0.5s").
func seconds(d time.Duration) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", d.Seconds()), "0"), ".") + "s"
}

func cmdStatus(d *Deps) int {
	creds, ok := loadCredentials(credentialsPath(d.Home))
	if !ok {
		d.Out(notSignedIn)
		return 1
	}
	expired := isExpired(creds, d.Now())
	d.Out("Server:   " + creds.Server)
	d.Out("Account:  " + creds.User)
	d.Out("Machine:  " + creds.Label)
	if expired {
		d.Out(fmt.Sprintf("Sign-in:  expired on %s — run: ex-runner login %s", creds.ExpiresAt, creds.Server))
	} else {
		d.Out(fmt.Sprintf("Sign-in:  valid until %s (renews automatically while running)", creds.ExpiresAt))
	}
	d.Out("Home:     " + d.Home)
	if expired {
		return 1
	}
	return 0
}

func cmdLogout(d *Deps) int {
	file := credentialsPath(d.Home)
	creds, ok := loadCredentials(file)
	if !ok {
		removeCredentials(file) // clear an unreadable leftover, if any
		d.Out("Not connected — nothing to do.")
		return 0
	}
	if err := d.client(creds.Server, creds.Token).Revoke(context.Background()); err != nil && !api.IsUnauthorized(err) {
		removeCredentials(file)
		d.Err(fmt.Sprintf("Couldn't reach %s to disconnect this runner (%s). Its token is forgotten here — remove \"%s\" on the Runners page to finish.",
			creds.Server, err.Error(), creds.Label))
		return 1
	}
	// A 401 means already revoked or expired server-side: nothing to undo.
	removeCredentials(file)
	d.Out(fmt.Sprintf("Disconnected \"%s\" from %s.", creds.Label, creds.Server))
	return 0
}

// UserHome is os.UserHomeDir with a "." fallback.
func UserHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
