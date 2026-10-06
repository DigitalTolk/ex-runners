package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func sampleCreds(over func(c *Credentials)) *Credentials {
	c := &Credentials{
		Server:    "https://ex.example",
		Token:     "tok",
		TokenID:   "rt1",
		Label:     "mac",
		User:      "Alice",
		ExpiresAt: t0.Add(20 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	if over != nil {
		over(c)
	}
	return c
}

func TestHomeAndPaths(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if got := Home(env("/custom"), "/home/u"); got != "/custom" {
		t.Errorf("Home(override) = %q", got)
	}
	if got := Home(env(""), "/home/u"); got != "/home/u/.ex-runner" {
		t.Errorf("Home = %q", got)
	}
	if credentialsPath("/h") != "/h/credentials.json" || StateDir("/h") != "/h/state" || lockPath("/h") != "/h/runner.pid" {
		t.Error("paths")
	}
	if UserHome() == "" {
		t.Error("UserHome empty")
	}
	t.Setenv("HOME", "")
	if UserHome() != "." {
		t.Errorf("UserHome without HOME = %q", UserHome())
	}
}

func TestParseCredentials(t *testing.T) {
	good := `{"server":"https://ex.example","token":"tok","tokenId":"rt1","label":"mac","user":"Alice","expiresAt":"2026-11-04T00:00:00Z","extra":"x"}`
	c, ok := parseCredentials([]byte(good))
	if !ok || c.Token != "tok" || c.TokenID != "rt1" {
		t.Fatalf("good = %+v %v", c, ok)
	}
	for _, bad := range []string{
		`{not json`, `null`, `7`,
		`{"server":"s","token":"","tokenId":"rt1","label":"mac","user":"A","expiresAt":"2026-11-04T00:00:00Z"}`,
		`{"server":"s","token":"t","tokenId":5,"label":"mac","user":"A","expiresAt":"2026-11-04T00:00:00Z"}`,
		`{"server":"s","token":"t","tokenId":"r","label":"mac","user":"A","expiresAt":"soon"}`,
	} {
		if _, ok := parseCredentials([]byte(bad)); ok {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCredentialsRoundTripOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "nested", "credentials.json")
	if _, ok := loadCredentials(file); ok {
		t.Fatal("missing file loaded")
	}
	if err := saveCredentials(file, sampleCreds(nil)); err != nil {
		t.Fatal(err)
	}
	c, ok := loadCredentials(file)
	if !ok || *c != *sampleCreds(nil) {
		t.Fatalf("round trip = %+v", c)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(file)); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(file + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Error("tmp file left behind")
	}
	if !removeCredentials(file) || removeCredentials(file) {
		t.Error("removeCredentials must report whether a file was there")
	}
}

func TestSaveCredentialsFailures(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if err := saveCredentials(filepath.Join(blocker, "creds.json"), sampleCreds(nil)); err == nil {
		t.Error("expected mkdir failure under a file")
	}
	// The tmp path is a directory → the write fails.
	file := filepath.Join(dir, "c.json")
	_ = os.MkdirAll(file+".tmp", 0o700)
	if err := saveCredentials(file, sampleCreds(nil)); err == nil {
		t.Error("expected write failure")
	}
	// The target is a non-empty directory → the rename fails.
	target := filepath.Join(dir, "d.json")
	_ = os.MkdirAll(filepath.Join(target, "x"), 0o700)
	if err := saveCredentials(target, sampleCreds(nil)); err == nil {
		t.Error("expected rename failure")
	}
}

func TestRenewalWindow(t *testing.T) {
	c := sampleCreds(func(c *Credentials) { c.ExpiresAt = t0.Format(time.RFC3339) })
	if !isExpired(c, t0) || isExpired(c, t0.Add(-time.Millisecond)) {
		t.Error("isExpired boundary")
	}
	if needsRenewal(c, t0.Add(-renewWithin)) || !needsRenewal(c, t0.Add(-renewWithin+time.Millisecond)) {
		t.Error("needsRenewal boundary")
	}
}

func TestPKCE(t *testing.T) {
	// RFC 7636 appendix B test vector.
	if got := challengeFor("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("challenge = %q", got)
	}
	v, err := randomB64(32)
	if err != nil || len(v) != 43 {
		t.Errorf("verifier = %q %v", v, err)
	}
	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() { randRead = orig }()
	if _, err := randomB64(16); err == nil {
		t.Error("expected entropy failure")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "home", "runner.pid")
	alive := func(int) bool { return true }
	dead := func(int) bool { return false }

	first, _, err := acquireLock(file, 100, alive)
	if err != nil || first == nil {
		t.Fatalf("first = %v %v", first, err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "100\n" {
		t.Errorf("lock content = %q", raw)
	}
	if lk, holder, _ := acquireLock(file, 200, alive); lk != nil || holder != 100 {
		t.Errorf("held lock: lk=%v holder=%d", lk, holder)
	}
	// Stale (dead holder), garbled, and our own pid are all taken over.
	if lk, _, _ := acquireLock(file, 200, dead); lk == nil {
		t.Error("stale lock not taken over")
	}
	_ = os.WriteFile(file, []byte("garbage"), 0o600)
	if lk, _, _ := acquireLock(file, 300, alive); lk == nil {
		t.Error("garbled lock not taken over")
	}
	mine, _, _ := acquireLock(file, 300, alive)
	if mine == nil {
		t.Fatal("own pid not taken over")
	}
	// Release removes only a lock that is still ours.
	_, _, _ = acquireLock(file, 400, dead) // someone presumed us dead
	mine.release()
	if readPid(file) != 400 {
		t.Error("released someone else's lock")
	}
	other := &lock{file: file, pid: 400}
	other.release()
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Error("own lock not released")
	}
	blocker := filepath.Join(dir, "blocker")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if _, _, err := acquireLock(filepath.Join(blocker, "runner.pid"), 1, alive); err == nil {
		t.Error("expected mkdir failure")
	}
	_ = os.MkdirAll(filepath.Join(dir, "asdir", "runner.pid"), 0o700)
	if _, _, err := acquireLock(filepath.Join(dir, "asdir", "runner.pid"), 1, alive); err == nil {
		t.Error("expected write failure")
	}
}

func TestBrowserCommand(t *testing.T) {
	url := "https://ex.example/runner/connect?a=1&b=2"
	if c, a := BrowserCommand("darwin", url); c != "open" || a[0] != url {
		t.Errorf("darwin = %s %v", c, a)
	}
	if c, a := BrowserCommand("windows", url); c != "rundll32" || a[0] != "url.dll,FileProtocolHandler" || a[1] != url {
		t.Errorf("windows = %s %v", c, a)
	}
	if c, a := BrowserCommand("linux", url); c != "xdg-open" || a[0] != url {
		t.Errorf("linux = %s %v", c, a)
	}
}

func TestTokenBoxAndJSONLine(t *testing.T) {
	b := newTokenBox(sampleCreds(nil))
	if b.token() != "tok" {
		t.Error("token")
	}
	b.set(sampleCreds(func(c *Credentials) { c.Token = "tok2" }))
	if b.token() != "tok2" {
		t.Error("set")
	}
	if got := jsonLine(map[string]any{"b": 1, "a": "x"}); got != `{"a":"x","b":1}` {
		t.Errorf("jsonLine = %s", got)
	}
	if got := jsonLine(map[string]any{"c": make(chan int)}); got != "{}" {
		t.Errorf("jsonLine(unmarshalable) = %s", got)
	}
	if seconds(2*time.Second) != "2s" || seconds(500*time.Millisecond) != "0.5s" || seconds(2*time.Millisecond) != "0.002s" {
		t.Errorf("seconds = %s %s %s", seconds(2*time.Second), seconds(500*time.Millisecond), seconds(2*time.Millisecond))
	}
}
