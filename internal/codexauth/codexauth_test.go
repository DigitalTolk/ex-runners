package codexauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type files struct{ dir, user, thread string }

func setup(t *testing.T) files {
	dir := t.TempDir()
	return files{dir: dir, user: filepath.Join(dir, "user-auth.json"), thread: filepath.Join(dir, "thread-auth.json")}
}

func write(t *testing.T, file string, lastRefreshAt any, token string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"last_refresh": lastRefreshAt, "tokens": map[string]any{"refresh_token": token}})
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func token(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Tokens struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Tokens.RefreshToken
}

func mode(t *testing.T, file string) os.FileMode {
	t.Helper()
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// --- LastRefresh ---

func TestLastRefreshReadsTimestampAndIsZeroForAnythingUnusable(t *testing.T) {
	f := setup(t)
	write(t, f.user, "2026-09-30T09:43:58.744667Z", "a")
	if got := LastRefresh(f.user); got != 1790761438744 { // Date.parse value
		t.Errorf("LastRefresh = %d", got)
	}
	if got := LastRefresh(filepath.Join(f.dir, "missing.json")); got != 0 {
		t.Errorf("missing = %d", got)
	}
	if err := os.WriteFile(f.thread, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LastRefresh(f.thread); got != 0 {
		t.Errorf("not json = %d", got)
	}
	write(t, f.thread, "not a date", "b")
	if got := LastRefresh(f.thread); got != 0 {
		t.Errorf("not a date = %d", got)
	}
	write(t, f.thread, 12345, "c")
	if got := LastRefresh(f.thread); got != 0 {
		t.Errorf("number = %d", got)
	}
	for _, body := range []string{"null", `"x"`, "[1]", "{}"} {
		if err := os.WriteFile(f.thread, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := LastRefresh(f.thread); got != 0 {
			t.Errorf("%s = %d", body, got)
		}
	}
}

func TestParseDateMatchesJavaScript(t *testing.T) {
	// Expected values are Date.parse results from Node 24.
	for in, want := range map[string]int64{
		"2026-09-30T09:43:58.744667Z":     1790761438744,
		"2026-09-30T09:43:58Z":            1790761438000,
		"2026-09-30T09:43:58+02:00":       1790754238000,
		"2026-09-30":                      1790726400000,
		"2026-09":                         1788220800000,
		"2026":                            1767225600000,
		"2026-09-30 09:43:58Z":            1790761438000,
		"2026-09-30t09:43:58z":            1790761438000,
		"2026-09-30T09:43Z":               1790761380000,
		"2026-09-30T09:43:58.1234567891Z": 1790761438123,
		"not a date":                      0,
		"":                                0,
	} {
		if got := parseDate(in); got != want {
			t.Errorf("parseDate(%q) = %d, want %d", in, got, want)
		}
	}
	// Date-times without an offset are LOCAL time, as in JavaScript.
	if got, want := parseDate("2026-09-30T09:43:58"), time.Date(2026, 9, 30, 9, 43, 58, 0, time.Local).UnixMilli(); got != want {
		t.Errorf("local seconds = %d, want %d", got, want)
	}
	if got, want := parseDate("2026-09-30T09:43"), time.Date(2026, 9, 30, 9, 43, 0, 0, time.Local).UnixMilli(); got != want {
		t.Errorf("local minutes = %d, want %d", got, want)
	}
}

// --- SyncAuthIn ---

func TestSyncAuthInSeedsNewThreadOwnerOnly(t *testing.T) {
	f := setup(t)
	write(t, f.user, "2026-09-07T12:46:44Z", "user-token")
	if !SyncAuthIn(f.user, f.thread) {
		t.Fatal("SyncAuthIn = false")
	}
	if got := token(t, f.thread); got != "user-token" {
		t.Errorf("token = %s", got)
	}
	if got := mode(t, f.thread); got != 0o600 {
		t.Errorf("mode = %o", got)
	}
}

func TestSyncAuthInKeepsFresherThreadCopy(t *testing.T) {
	f := setup(t)
	write(t, f.user, "2026-09-07T12:46:44Z", "spent")
	write(t, f.thread, "2026-09-30T09:43:58Z", "live")
	if !SyncAuthIn(f.user, f.thread) {
		t.Fatal("SyncAuthIn = false")
	}
	if got := token(t, f.thread); got != "live" {
		t.Errorf("token = %s", got)
	}
}

func TestSyncAuthInReplacesOlderThreadCopy(t *testing.T) {
	f := setup(t)
	write(t, f.user, "2026-09-30T10:00:00Z", "newer")
	write(t, f.thread, "2026-09-30T09:00:00Z", "older")
	SyncAuthIn(f.user, f.thread)
	if got := token(t, f.thread); got != "newer" {
		t.Errorf("token = %s", got)
	}
}

func TestSyncAuthInReportsWhenUserHasNoCredentials(t *testing.T) {
	f := setup(t)
	if SyncAuthIn(f.user, f.thread) {
		t.Fatal("SyncAuthIn = true")
	}
	if _, err := os.Stat(f.thread); err == nil {
		t.Fatal("thread file created")
	}
}

func TestSyncAuthInCopyFailuresReportFalse(t *testing.T) {
	f := setup(t)
	// The user "file" is a directory: it exists but cannot be read.
	if err := os.Mkdir(f.user, 0o755); err != nil {
		t.Fatal(err)
	}
	if SyncAuthIn(f.user, f.thread) {
		t.Error("unreadable source reported true")
	}
	// The thread's home does not exist: the temp file cannot be written.
	g := setup(t)
	write(t, g.user, "2026-09-07T12:46:44Z", "u")
	if SyncAuthIn(g.user, filepath.Join(g.dir, "missing-home", "auth.json")) {
		t.Error("unwritable destination reported true")
	}
}

// --- SyncAuthOut ---

func TestSyncAuthOutHandsRefreshBackToUser(t *testing.T) {
	f := setup(t)
	write(t, f.user, "2026-09-07T12:46:44Z", "spent")
	write(t, f.thread, "2026-09-30T09:43:58Z", "rotated")
	if !SyncAuthOut(f.thread, f.user) {
		t.Fatal("SyncAuthOut = false")
	}
	if got := token(t, f.user); got != "rotated" {
		t.Errorf("token = %s", got)
	}
	if got := mode(t, f.user); got != 0o600 {
		t.Errorf("mode = %o", got)
	}
}

func TestSyncAuthOutLeavesUserAloneWithoutRefresh(t *testing.T) {
	f := setup(t)
	write(t, f.user, "2026-09-30T09:43:58Z", "same")
	write(t, f.thread, "2026-09-30T09:43:58Z", "same-copy")
	if SyncAuthOut(f.thread, f.user) {
		t.Fatal("SyncAuthOut = true")
	}
	if got := token(t, f.user); got != "same" {
		t.Errorf("token = %s", got)
	}
	if SyncAuthOut(filepath.Join(f.dir, "missing.json"), f.user) {
		t.Fatal("missing thread file reported true")
	}
}

func TestSyncAuthOutRenameFailureReportsFalse(t *testing.T) {
	f := setup(t)
	write(t, f.thread, "2026-09-30T09:43:58Z", "rotated")
	// The user's auth.json is a non-empty directory: rename cannot replace it.
	if err := os.MkdirAll(filepath.Join(f.user, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if SyncAuthOut(f.thread, f.user) {
		t.Fatal("SyncAuthOut = true")
	}
	// The caller can tell a failed write-back apart: the thread still reads fresher.
	if LastRefresh(f.thread) <= LastRefresh(f.user) {
		t.Fatal("thread no longer fresher")
	}
}

func TestCopyAtomicChmodFailure(t *testing.T) {
	f := setup(t)
	write(t, f.thread, "2026-09-30T09:43:58Z", "rotated")
	chmod = func(string, os.FileMode) error { return errors.New("chmod denied") }
	t.Cleanup(func() { chmod = os.Chmod })
	if SyncAuthOut(f.thread, f.user) {
		t.Fatal("SyncAuthOut = true despite chmod failure")
	}
	if _, err := os.Stat(f.user); err == nil {
		t.Fatal("user file written")
	}
}

func TestCopyAtomicResetsLeftoverTempPermissions(t *testing.T) {
	f := setup(t)
	write(t, f.thread, "2026-09-30T09:43:58Z", "rotated")
	// A leftover temp from a crashed run keeps its old mode through
	// WriteFile; the explicit chmod is what makes the result owner-only.
	tmp := filepath.Join(f.dir, fmt.Sprintf(".auth.json.%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		t.Fatal(err)
	}
	if !SyncAuthOut(f.thread, f.user) {
		t.Fatal("SyncAuthOut = false")
	}
	if got := mode(t, f.user); got != 0o600 {
		t.Errorf("mode = %o", got)
	}
}
