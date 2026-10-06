// Package codexauth syncs Codex credentials between the user's ~/.codex and
// each thread's CODEX_HOME.
//
// Codex rotates its refresh token on every refresh and writes the new pair
// into the CODEX_HOME it ran with. Every thread runs in its own home holding a
// COPY of auth.json, so a refresh never reached the user's file — the next new
// thread copied the stale, already-spent token and codex failed with
// "refresh_token_reused" until the user signed in again. Freshness is read
// from auth.json's own last_refresh (file times are useless: a copy is new).
package codexauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LastRefresh returns the file's last_refresh as epoch ms; 0 when the file is
// missing, unreadable or carries no usable timestamp.
func LastRefresh(file string) int64 {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	var raw any
	if json.Unmarshal(b, &raw) != nil {
		return 0
	}
	obj, _ := raw.(map[string]any)
	s, ok := obj["last_refresh"].(string)
	if !ok {
		return 0
	}
	return parseDate(s)
}

// isoLayouts is the ECMAScript date-time string format that Date.parse
// accepts (and that codex writes: RFC 3339 with up to nanosecond fractions).
// As in JavaScript, date-only forms are UTC and date-times without an offset
// are local time. Fractional seconds of any length are accepted after the
// seconds field and truncated to the millisecond.
var isoLayouts = []struct {
	layout string
	loc    *time.Location
}{
	{"2006-01-02T15:04:05Z07:00", time.UTC},
	{"2006-01-02T15:04Z07:00", time.UTC},
	{"2006-01-02T15:04:05", time.Local},
	{"2006-01-02T15:04", time.Local},
	{"2006-01-02", time.UTC},
	{"2006-01", time.UTC},
	{"2006", time.UTC},
}

// parseDate is Date.parse for the ISO forms, returning 0 where JavaScript
// yields NaN. V8 also takes a space or lowercase "t" for the separator and a
// lowercase "z"; those are normalised first.
func parseDate(s string) int64 {
	if len(s) > 10 && (s[10] == ' ' || s[10] == 't') {
		s = s[:10] + "T" + s[11:]
	}
	if strings.HasSuffix(s, "z") {
		s = s[:len(s)-1] + "Z"
	}
	for _, l := range isoLayouts {
		if t, err := time.ParseInLocation(l.layout, s, l.loc); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// chmod is os.Chmod; tests swap it to exercise the failure path.
var chmod = os.Chmod

// copyAtomic writes src over dst via a temp file + rename, owner-only, so a
// reader never sees a half-written credential.
func copyAtomic(src, dst string) error {
	tmp := filepath.Join(filepath.Dir(dst), fmt.Sprintf(".auth.json.%d.tmp", os.Getpid()))
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// SyncAuthIn seeds a thread's auth.json from the user's — unless the thread
// already holds a fresher one (it refreshed on an earlier turn). Returns false
// when the user has no auth.json to give, or when it cannot be copied (the TS
// version threw there, and its only caller treated that exactly like false).
func SyncAuthIn(userFile, threadFile string) bool {
	if _, err := os.Stat(userFile); err != nil {
		return false
	}
	if LastRefresh(threadFile) > LastRefresh(userFile) {
		return true
	}
	return copyAtomic(userFile, threadFile) == nil
}

// SyncAuthOut hands a refresh made during the run back to the user's file, so
// the next thread starts from the live token. Returns whether it wrote: false
// when the run did not refresh, and also when the copy failed (TS threw) —
// the thread file then still reads fresher than the user's, which is how a
// caller tells the two apart.
func SyncAuthOut(threadFile, userFile string) bool {
	if LastRefresh(threadFile) <= LastRefresh(userFile) {
		return false
	}
	return copyAtomic(threadFile, userFile) == nil
}
