package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Everything ex-runner keeps lives under one directory: $EX_RUNNER_HOME, or
// ~/.ex-runner. credentials.json holds the runner token (mode 0600); state/
// holds the stable runner ID, warm-session pins and per-thread work dirs.
// Same layout as the TypeScript ex-runner, so either binary reads the other's
// files.

// Home resolves the ex-runner home directory.
func Home(getenv func(string) string, userHome string) string {
	if h := getenv("EX_RUNNER_HOME"); h != "" {
		return h
	}
	return filepath.Join(userHome, ".ex-runner")
}

func credentialsPath(home string) string { return filepath.Join(home, "credentials.json") }

// StateDir is where the runner keeps its state under a home.
func StateDir(home string) string { return filepath.Join(home, "state") }

func lockPath(home string) string { return filepath.Join(home, "runner.pid") }

// Credentials is what `ex-runner login` writes and `ex-runner start` reads.
// The token is a long-lived machine credential, so the file is owner-only
// (0600) inside an owner-only directory (0700), replaced atomically (write +
// rename) so a crash mid-write never leaves a half-written token behind.
type Credentials struct {
	Server    string `json:"server"`    // ex server origin, e.g. https://ex.example.com
	Token     string `json:"token"`     // runner-scoped JWT
	TokenID   string `json:"tokenId"`   // its ID — what the Runners page revokes
	Label     string `json:"label"`     // this machine's name on the Runners page
	User      string `json:"user"`      // display name of the account it runs for
	ExpiresAt string `json:"expiresAt"` // RFC 3339; renewed automatically while running
}

// parseCredentials accepts only a complete record with a parseable expiry;
// anything else reads as "not signed in" rather than half-working.
func parseCredentials(raw []byte) (*Credentials, bool) {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	for _, k := range []string{"server", "token", "tokenId", "label", "user", "expiresAt"} {
		if s, ok := fields[k].(string); !ok || s == "" {
			return nil, false
		}
	}
	var c Credentials
	_ = json.Unmarshal(raw, &c) // every field was just checked to be a string
	if _, err := time.Parse(time.RFC3339Nano, c.ExpiresAt); err != nil {
		return nil, false
	}
	return &c, true
}

func loadCredentials(file string) (*Credentials, bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	return parseCredentials(raw)
}

func saveCredentials(file string, c *Credentials) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ") // a struct of strings always marshals
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// removeCredentials reports whether there was anything to remove.
func removeCredentials(file string) bool {
	err := os.Remove(file)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

func expiry(c *Credentials) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, c.ExpiresAt) // validated by parseCredentials
	return t
}

// Runner tokens live 30 days. A running ex-runner renews once the token is
// within a week of expiry, and checks twice a day — so a machine that is on
// at least once a week never needs a fresh `ex-runner login`.
const (
	renewWithin = 7 * 24 * time.Hour
	renewCheck  = 12 * time.Hour
)

func isExpired(c *Credentials, now time.Time) bool { return !now.Before(expiry(c)) }

func needsRenewal(c *Credentials, now time.Time) bool { return expiry(c).Sub(now) < renewWithin }

// PKCE (RFC 7636, S256) for `ex-runner login`. The verifier never leaves
// this process; the browser only ever carries the challenge and, afterwards,
// a one-time code. Redeeming that code needs the verifier, so a code that
// leaks from the browser (history, an extension, a proxy log) is worthless.

var randRead = rand.Read

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// One runner per ex-runner home. Two would share a runner ID and state dir,
// claim the same work and trample each other's warm sessions — easy to cause
// by typing `ex-runner start` while `brew services` already runs one. The
// lock is a pid file; a stale one (its process is gone) is taken over. Two
// runners started in the very same instant could both pass the check; that
// window is microseconds wide and the cost is a duplicate runner row, so a
// plain check-then-write is enough.

type lock struct {
	file string
	pid  int
}

func acquireLock(file string, pid int, alive func(int) bool) (*lock, int, error) {
	if holder := readPid(file); holder > 0 && holder != pid && alive(holder) {
		return nil, holder, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, 0, err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return nil, 0, err
	}
	return &lock{file: file, pid: pid}, 0, nil
}

// release removes only OUR lock: a runner that took over a lock we thought
// was ours (we were presumed dead) keeps it.
func (l *lock) release() {
	if readPid(l.file) == l.pid {
		_ = os.Remove(l.file)
	}
}

func readPid(file string) int {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// BrowserCommand picks the platform's "open this URL" command. When it fails
// the URL is already printed, so the user can open it by hand (SSH
// sessions, minimal Linux installs).
func BrowserCommand(goos, url string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		// Not `cmd /c start`: cmd re-parses the line and would split the URL
		// at every `&` in its query string. rundll32 takes it as one argument.
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return "xdg-open", []string{url}
	}
}
