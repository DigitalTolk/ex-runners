// Package cli is the ex-runner command line. Everything process-shaped
// (argv, stdout, signals, spawning a browser, the runner's main loop)
// arrives through Deps, so each command is testable end to end without a
// real process.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/devicekey"
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
                                 (--no-browser: print the link, don't open it)
  ex-runner start                Bring your agents online here (Ctrl-C to stop)
  ex-runner status               Show the account and server this runner uses
                                 (--json: the same as one JSON object)
  ex-runner logout               Disconnect this computer and forget its token
  ex-runner --version            Print the version

Files live in $EX_RUNNER_HOME (default: ~/.ex-runner).`

const notSignedIn = "This computer is not connected to ex yet. Run: ex-runner login <server-url>"

const loginUsage = "Usage: ex-runner login [--no-browser] <server-url>    e.g. ex-runner login https://ex.example.com"

// A server that can't be reached at start (a laptop logging in before
// Wi-Fi is up, the server mid-deploy) is retried with backoff instead of
// ending the process — under `brew services` an exit would leave agents
// offline until the next login.
const (
	startRetryMin = 2 * time.Second
	startRetryMax = 60 * time.Second
)

func disconnected(server string) string {
	return "This runner was disconnected — it was removed on the Runners page, its account was deactivated, its sign-in expired, " +
		"or ex disconnected it for safety because its sign-in was used from another computer.\n" +
		"Connect it again with: ex-runner login " + server
}

func upgradeRequired(server string) string {
	return "ex only accepts runners whose sign-in is locked to the computer they run on, and this one's isn't.\n" +
		"Update ex-runner to the latest version, then connect it again with: ex-runner login " + server
}

func deviceKeyLost(server, why string) string {
	return "This runner's sign-in is locked to a device key this computer can't use (" + why + ").\n" +
		"Connect it again with: ex-runner login " + server
}

// lostAuth explains a 401 that ends the runner.
func lostAuth(server string, err error) string {
	if api.IsUpgradeRequired(err) {
		return upgradeRequired(server)
	}
	return disconnected(server)
}

// newDeviceKey makes this computer's device key: in the Secure Enclave when
// there is one, in software otherwise (a seam: tests use software keys).
var newDeviceKey = devicekey.New

// loadDeviceKey reads device-key.json; (nil, nil) when there is none.
func loadDeviceKey(home string) (*devicekey.Key, error) {
	k, err := devicekey.Load(deviceKeyPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return k, err
}

// keyKind is the device key as status reports it: "hardware", "software",
// or "" for none. Never the key itself.
func keyKind(k *devicekey.Key) string {
	switch {
	case k == nil:
		return ""
	case k.Hardware():
		return "hardware"
	default:
		return "software"
	}
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
		flags, args, ok := parseArgs(argv[1:], "--no-browser")
		if flags["--help"] {
			d.Out(loginUsage)
			return 0
		}
		if !ok || len(args) != 1 {
			d.Err(loginUsage)
			return 2
		}
		return cmdLogin(args[0], flags["--no-browser"], &d)
	case "start":
		return cmdStart(&d)
	case "status":
		flags, args, ok := parseArgs(argv[1:], "--json")
		if !ok || len(args) > 0 {
			d.Err("Usage: ex-runner status [--json]")
			return 2
		}
		if flags["--json"] {
			return cmdStatusJSON(&d)
		}
		return cmdStatus(&d)
	case "logout":
		return cmdLogout(&d)
	default:
		d.Err(fmt.Sprintf("Unknown command %q.\n\n%s", cmd, Usage))
		return 2
	}
}

// client talks to c's server with its token, signing with key when there
// is one.
func (d *Deps) client(c *Credentials, key *devicekey.Key) *api.Client {
	cl := api.New(c.Server, func() string { return c.Token })
	cl.HTTP = d.HTTP
	if key != nil {
		cl.Prover = devicekey.NewProver(key, c.TokenID, d.Now)
	}
	return cl
}

// parseArgs splits a command's arguments into the flags it knows (plus
// --help/-h, always accepted) and positional arguments. ok is false when an
// unknown flag appears — a typo must not become a server URL.
func parseArgs(argv []string, known ...string) (flags map[string]bool, args []string, ok bool) {
	flags = map[string]bool{}
	allowed := map[string]bool{"--help": true}
	for _, k := range known {
		allowed[k] = true
	}
	ok = true
	for _, a := range argv {
		switch {
		case a == "-h":
			flags["--help"] = true
		case strings.HasPrefix(a, "-"):
			if !allowed[a] {
				ok = false
			}
			flags[a] = true
		default:
			args = append(args, a)
		}
	}
	return flags, args, ok
}

func cmdLogin(arg string, noBrowser bool, d *Deps) int {
	server, ok := normalizeServer(arg)
	if !ok {
		d.Err(loginUsage)
		return 2
	}
	openBrowser := d.OpenBrowser
	if noBrowser {
		// Something else shows the link (the ex desktop app approves it
		// in-app): print it, open nothing.
		openBrowser = func(string) {}
	}
	file := credentialsPath(d.Home)
	previous, hadPrevious := loadCredentials(file)
	// The key the previous sign-in is locked to: retiring that token below
	// must be signed with it, or the server reads the revoke as a stolen
	// token.
	previousKey, _ := loadDeviceKey(d.Home)
	// A fresh key for every sign-in, made here in ex-runner (also under
	// `--no-browser`, when the desktop app drives the login).
	key, err := newDeviceKey(d.Now())
	if err != nil {
		d.Err("Couldn't make this computer's device key: " + err.Error())
		return 1
	}
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
		openBrowser: openBrowser,
		out:         d.Out,
		label:       machineLabel(d.Hostname),
		timeout:     d.LoginTimeout,
		client:      d.HTTP,
		deviceKey:   &api.DeviceKey{PublicKey: key.PublicKey(), Hardware: key.Hardware()},
	})
	if err != nil {
		if le, ok := err.(*loginError); ok {
			d.Err(le.msg)
		} else {
			d.Err("Sign-in failed: " + err.Error())
		}
		return 1
	}
	if err := key.Save(deviceKeyPath(d.Home)); err != nil {
		d.Err("Couldn't save this computer's device key: " + err.Error())
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
		_ = d.client(previous, previousKey).Revoke(context.Background())
	}
	d.Out(fmt.Sprintf("Connected as %s on %s. This computer shows as \"%s\" on your Runners page.", creds.User, creds.Server, creds.Label))
	if key.Hardware() {
		d.Out("The connection is locked to this Mac's Secure Enclave.")
	} else {
		d.Out("The connection is locked to a key on this computer.")
	}
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
	// Every request with the runner token is signed with the device key —
	// the renewals here and the runner's own. The token ID never changes on
	// renewal, so one prover serves the whole process. A sign-in from before
	// device keys runs unsigned until the next `ex-runner login`.
	var prover api.Prover
	if creds.DeviceKey {
		key, err := loadDeviceKey(d.Home)
		if err == nil && key == nil {
			err = errors.New("device-key.json is missing")
		}
		if err == nil {
			err = key.Check()
		}
		if err != nil {
			d.Err(deviceKeyLost(creds.Server, err.Error()))
			return 1
		}
		prover = devicekey.NewProver(key, creds.TokenID, d.Now)
	} else {
		d.Out("This runner's sign-in isn't locked to this computer. To lock it, run: ex-runner login " + creds.Server)
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
	client.Prover = prover

	// renew returns an error only when the server says the token is dead;
	// any other failure (network, 5xx) keeps the current token and retries
	// on the next check — it is still valid for days.
	renew := func() error {
		cur := tokens.get()
		if !needsRenewal(cur, d.Now()) {
			return nil
		}
		fresh, err := client.Renew(context.Background())
		if err != nil {
			if api.IsUnauthorized(err) {
				return err
			}
			d.Err(fmt.Sprintf("Couldn't renew the runner's sign-in yet (%s); will retry later.", err.Error()))
			return nil
		}
		next := *cur
		next.Token, next.ExpiresAt = fresh.Token, fresh.ExpiresAt
		tokens.set(&next)
		if err := saveCredentials(file, &next); err != nil {
			d.Err("Renewed the runner's sign-in but couldn't save it: " + err.Error())
		}
		d.Out(fmt.Sprintf("Renewed this runner's sign-in (valid until %s).", next.ExpiresAt))
		return nil
	}
	if err := renew(); err != nil {
		d.Err(lostAuth(creds.Server, err))
		return 1
	}

	unauthorized := make(chan struct{})
	var unauthOnce sync.Once
	var unauthErr error
	cfg := runner.Config{
		BaseURL:  creds.Server,
		Token:    tokens.token,
		Prover:   prover,
		StateDir: StateDir(d.Home),
		MCPEntry: d.MCPEntry,
		Log: func(msg string, extra map[string]any) {
			line := d.Now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + msg
			if extra != nil {
				line += " " + jsonLine(extra)
			}
			d.Out(line)
		},
		OnUnauthorized: func(err error) { unauthOnce.Do(func() { unauthErr = err; close(unauthorized) }) },
		Version:        d.Version,
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
			d.Err(lostAuth(creds.Server, err))
			return 1
		}
		if api.IsClockSkew(err) {
			d.Err(fmt.Sprintf("%s in %s.", api.ClockSkewHint, seconds(delay)))
		} else {
			d.Err(fmt.Sprintf("Can't reach %s yet (%s); retrying in %s.", creds.Server, err.Error(), seconds(delay)))
		}
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
			// A dead token also fails the runner's next claim, which ends
			// the process through OnUnauthorized.
			_ = renew()
		case <-unauthorized:
			d.Err(lostAuth(creds.Server, unauthErr))
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
	d.Out("Locked:   " + lockedTo(creds, d.Home))
	d.Out("Home:     " + d.Home)
	if expired {
		return 1
	}
	return 0
}

// lockedTo describes what the sign-in is locked to, for `status`.
func lockedTo(creds *Credentials, home string) string {
	key, _ := loadDeviceKey(home)
	switch kind := keyKind(key); {
	case !creds.DeviceKey:
		return "no — to lock it to this computer, run: ex-runner login " + creds.Server
	case kind == "hardware":
		return "this Mac's Secure Enclave"
	case kind == "software":
		return "a key on this computer"
	default:
		return "a device key that is missing here — run: ex-runner login " + creds.Server
	}
}

// statusJSON is `ex-runner status --json`: what a program driving the runner
// (the ex desktop app) needs, never the token itself.
type statusJSON struct {
	Version   string `json:"version"`
	Home      string `json:"home"`
	Connected bool   `json:"connected"`
	Server    string `json:"server,omitempty"`
	User      string `json:"user,omitempty"`
	Label     string `json:"label,omitempty"`
	TokenID   string `json:"tokenId,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Expired   bool   `json:"expired"`
	// DeviceKey is what the sign-in is locked to: "hardware" (Secure
	// Enclave), "software", or "" (none). Never the key itself.
	DeviceKey string `json:"deviceKey"`
	// Running is whether a runner holds this home's lock, and its pid.
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

// cmdStatusJSON always exits 0: "not connected" is an answer, not an error.
func cmdStatusJSON(d *Deps) int {
	st := statusJSON{Version: d.Version, Home: d.Home}
	if creds, ok := loadCredentials(credentialsPath(d.Home)); ok {
		st.Connected = true
		st.Server, st.User, st.Label, st.TokenID, st.ExpiresAt = creds.Server, creds.User, creds.Label, creds.TokenID, creds.ExpiresAt
		st.Expired = isExpired(creds, d.Now())
		if creds.DeviceKey {
			key, _ := loadDeviceKey(d.Home)
			st.DeviceKey = keyKind(key)
		}
	}
	if pid := readPid(lockPath(d.Home)); pid > 0 && d.Alive(pid) {
		st.Running, st.PID = true, pid
	}
	raw, _ := json.Marshal(st) // plain strings, bools and ints always marshal
	d.Out(string(raw))
	return 0
}

func cmdLogout(d *Deps) int {
	file := credentialsPath(d.Home)
	creds, ok := loadCredentials(file)
	key, _ := loadDeviceKey(d.Home)
	// The device key goes with the sign-in, whatever the server says.
	defer func() { _ = devicekey.Remove(deviceKeyPath(d.Home)) }()
	if !ok {
		removeCredentials(file) // clear an unreadable leftover, if any
		d.Out("Not connected — nothing to do.")
		return 0
	}
	if err := d.client(creds, key).Revoke(context.Background()); err != nil && !api.IsUnauthorized(err) {
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
