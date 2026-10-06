package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

func assignment(over func(*protocol.Assignment)) protocol.Assignment {
	a := protocol.Assignment{
		RunID:         "r1",
		AgentID:       "agent-gg",
		AgentName:     "gg",
		InvokerID:     "u1",
		InvokerName:   "Alice",
		ParentID:      "chan1",
		ParentType:    "channel",
		ThreadRootID:  "m1",
		MessageID:     "m2",
		Harness:       "claude",
		Persona:       "be helpful",
		Prompt:        "do the thing",
		ContextBundle: "# Task",
		MCPToken:      "t",
	}
	if over != nil {
		over(&a)
	}
	return a
}

func withRoot(root string) protocol.Assignment {
	return assignment(func(a *protocol.Assignment) { a.ThreadRootID = root })
}

// memStore is the TS module before initSessions: nothing persists.
func memStore() *Store { return Open("") }

// clock is a settable fake clock (vi.useFakeTimers / setSystemTime).
type clock struct{ ms int64 }

func (c *clock) set(t string) {
	tm, err := time.Parse(time.RFC3339, t)
	if err != nil {
		panic(err)
	}
	c.ms = tm.UnixMilli()
}

func fakeClock(s *Store, start string) *clock {
	c := &clock{}
	c.set(start)
	s.now = func() int64 { return c.ms }
	return c
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func sessionID(e *Entry) string {
	if e == nil {
		return "<nil>"
	}
	return e.SessionID
}

// --- session cache ---

func TestResumesSameAgentThreadAndIsolatesOthers(t *testing.T) {
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-1", "/tmp/x")
	if got := sessionID(s.Get(a)); got != "sess-1" {
		t.Fatalf("Get = %s", got)
	}
	// Different thread → no session.
	if e := s.Get(withRoot("other")); e != nil {
		t.Errorf("other thread resumed %+v", e)
	}
	// Different agent, same thread → no session.
	if e := s.Get(assignment(func(a *protocol.Assignment) { a.AgentID = "agent-qib" })); e != nil {
		t.Errorf("other agent resumed %+v", e)
	}
}

func TestDropsSessionWhenPersonaOrModelChanges(t *testing.T) {
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-1", "/tmp/x")
	if e := s.Get(assignment(func(a *protocol.Assignment) { a.Persona = "be RUTHLESS" })); e != nil {
		t.Fatalf("persona change resumed %+v", e)
	}
	// The stale entry is evicted, not resurrected for the old persona.
	if e := s.Get(a); e != nil {
		t.Fatalf("old persona resurrected %+v", e)
	}
	s.Put(a, "sess-2", "/tmp/x")
	if e := s.Get(assignment(func(a *protocol.Assignment) { a.Model = "opus" })); e != nil {
		t.Fatalf("model change resumed %+v", e)
	}
}

func TestTopLevelThreadsKeyOnInvokingMessage(t *testing.T) {
	s := memStore()
	a := withRoot("")
	if !strings.Contains(ThreadKey(a), "#m2") {
		t.Fatalf("ThreadKey = %q", ThreadKey(a))
	}
	if got := ThreadKey(assignment(nil)); got != "agent-gg#chan1#m1" {
		t.Fatalf("ThreadKey = %q", got)
	}
	s.Put(a, "sess-2", "/tmp/y")
	if got := sessionID(s.Get(withRoot(""))); got != "sess-2" {
		t.Fatalf("Get = %s", got)
	}
}

func TestDropForgetsExplicitly(t *testing.T) {
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-1", "/tmp/x")
	s.Drop(ThreadKey(a))
	if e := s.Get(a); e != nil {
		t.Fatalf("dropped session resumed %+v", e)
	}
}

// Regression: the sweep used the ASYNC fs.rm callback form in TS. When Get
// dropped a stale-and-fat session, the runner immediately recreated the same
// path for the cold start and the pending rm landed afterwards, deleting the
// directory the run was about to use. The removal must be finished when Drop
// returns.
func TestDropRemovesWorkDirSynchronously(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-sweep", dir)
	s.Drop(ThreadKey(a))
	// No waiting: if the sweep were async this would still exist.
	if exists(dir) {
		t.Fatal("work dir survived Drop")
	}
}

func TestStaleAndFatDropLeavesNoPendingDelete(t *testing.T) {
	// The exact production path: the guard inside Get drops the session, and
	// the caller then recreates that same directory. Recreating it after the
	// drop must be safe.
	dir := t.TempDir()
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-stale", dir)
	s.AddCost(ThreadKey(a), 60_000) // fat history
	if s.Get(a) == nil {
		t.Fatal("fresh fat session must still resume")
	}
	if !exists(dir) {
		t.Fatal("dir gone too early")
	}
	s.Drop(ThreadKey(a))
	if exists(dir) {
		t.Fatal("dir survived Drop")
	}
	// Recreating the path after the drop stays created — nothing sweeps it later.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "mcp.json")) {
		t.Fatal("recreated file vanished")
	}
}

func TestAddCostAccumulatesAndIgnoresNoOps(t *testing.T) {
	s := memStore()
	a := assignment(nil)
	key := ThreadKey(a)
	s.AddCost(key, 100) // no entry yet: silently ignored
	s.Put(a, "sess-cost", "/tmp/x")
	s.AddCost(key, 0)   // non-positive: ignored
	s.AddCost(key, -10) // non-positive: ignored
	s.AddCost(key, 100)
	s.AddCost(key, 50)
	e := s.Get(a)
	// Still resumable — 150 fresh tokens is nowhere near the stale-fat bar.
	if sessionID(e) != "sess-cost" || e.HistoryTokens != 150 {
		t.Fatalf("Get = %+v", e)
	}
}

func TestPutStartsCostOver(t *testing.T) {
	// TS putSession writes a brand-new entry: the history estimate resets.
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-1", "/tmp/x")
	s.AddCost(ThreadKey(a), 30_000)
	s.Put(a, "sess-1", "/tmp/x")
	if e := s.Get(a); e == nil || e.HistoryTokens != 0 {
		t.Fatalf("Get = %+v", e)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	s := memStore()
	a := assignment(nil)
	s.Put(a, "sess-1", "/tmp/x")
	s.Get(a).SessionID = "mutated"
	if got := sessionID(s.Get(a)); got != "sess-1" {
		t.Fatalf("store mutated through Get: %s", got)
	}
}

func TestEvictsLeastRecentlyUsedPastTheCap(t *testing.T) {
	s := memStore()
	// Every put lands in the same millisecond (the common case in TS too):
	// the tie breaks by insertion order, so root-0 is the one evicted.
	fakeClock(s, "2026-09-22T10:00:00Z")
	var dirs []string
	for i := range 41 {
		dir := t.TempDir()
		dirs = append(dirs, dir)
		s.Put(withRoot(fmt.Sprintf("root-%d", i)), fmt.Sprintf("sess-%d", i), dir)
	}
	// maxSessions is 40: the oldest pin (root-0) is gone, dir swept; the rest live.
	if e := s.Get(withRoot("root-0")); e != nil {
		t.Fatalf("root-0 survived eviction: %+v", e)
	}
	if exists(dirs[0]) {
		t.Fatal("evicted dir not swept")
	}
	if got := sessionID(s.Get(withRoot("root-40"))); got != "sess-40" {
		t.Fatalf("root-40 = %s", got)
	}
	if got := sessionID(s.Get(withRoot("root-1"))); got != "sess-1" {
		t.Fatalf("root-1 = %s", got)
	}
}

func TestEvictionOrdersByLastUsedBeforeInsertion(t *testing.T) {
	s := memStore()
	c := fakeClock(s, "2026-09-22T10:00:00Z")
	for i := range 40 {
		s.Put(withRoot(fmt.Sprintf("root-%d", i)), fmt.Sprintf("sess-%d", i), t.TempDir())
	}
	// Re-pin root-0 later: it keeps its insertion slot but is now the
	// freshest, so root-1 becomes the least recently used.
	c.ms += 1000
	s.Put(withRoot("root-0"), "sess-0b", t.TempDir())
	c.ms += 1000
	s.Put(withRoot("root-40"), "sess-40", t.TempDir())
	if e := s.Get(withRoot("root-1")); e != nil {
		t.Fatalf("root-1 survived: %+v", e)
	}
	if got := sessionID(s.Get(withRoot("root-0"))); got != "sess-0b" {
		t.Fatalf("root-0 = %s", got)
	}
}

// --- session persistence across restarts ---

func readPersisted(t *testing.T, stateDir string) map[string]Entry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]Entry
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("sessions.json: %v (%s)", err, b)
	}
	return m
}

func TestRoundTripsPinsDroppingDeadEntriesOnLoad(t *testing.T) {
	tmp := t.TempDir()
	liveDir := t.TempDir()
	now := time.Now().UnixMilli()
	raw, _ := json.Marshal(map[string]any{
		"live":      map[string]any{"sessionId": "s-live", "personaHash": "h", "workDir": liveDir, "lastUsed": now},
		"expired":   map[string]any{"sessionId": "s-old", "personaHash": "h", "workDir": liveDir, "lastUsed": now - 8*24*60*60*1000},
		"sweptDir":  map[string]any{"sessionId": "s-gone", "personaHash": "h", "workDir": filepath.Join(tmp, "nope"), "lastUsed": now},
		"malformed": map[string]any{"sessionId": 42, "workDir": liveDir, "lastUsed": now},
		"noWorkDir": map[string]any{"sessionId": "s-nwd", "lastUsed": now},
		"noTime":    map[string]any{"sessionId": "s-nt", "workDir": liveDir},
		"nullEntry": nil,
		"scalar":    7,
	})
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(tmp)
	// Only the live pin survived the load: persist it back and reload to prove
	// the save path wrote exactly that survivor set.
	s.Put(withRoot("persisted"), "s-new", liveDir)
	s2 := Open(tmp)
	var ids []string
	for _, e := range readPersisted(t, tmp) {
		ids = append(ids, e.SessionID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "s-live,s-new" {
		t.Fatalf("persisted ids = %v", ids)
	}
	if got := sessionID(s2.Get(withRoot("persisted"))); got != "s-new" {
		t.Fatalf("reloaded pin = %s", got)
	}
}

func TestStartsEmptyOnFirstRunAndSurvivesUnwritableStateDir(t *testing.T) {
	tmp := t.TempDir()
	s := Open(tmp) // no sessions.json yet
	// Make the persist path unwritable by occupying it with a directory:
	// saves are best-effort and must not panic.
	if err := os.Mkdir(filepath.Join(tmp, "sessions.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Put(assignment(nil), "s1", "/tmp/x")
	if got := sessionID(s.Get(assignment(nil))); got != "s1" {
		t.Fatalf("Get = %s", got)
	}
}

func TestWorkDirIsStablePerThreadAndCreated(t *testing.T) {
	tmp := t.TempDir()
	d1 := WorkDir(tmp, "agent#chan#root")
	d2 := WorkDir(tmp, "agent#chan#root")
	if d1 != d2 {
		t.Fatalf("unstable: %s vs %s", d1, d2)
	}
	if !strings.HasPrefix(d1, filepath.Join(tmp, "threads")) || !exists(d1) {
		t.Fatalf("WorkDir = %s", d1)
	}
	// sha256("agent#chan#root")[:16], same slug as the TS runner.
	if filepath.Base(d1) != "51de2cfc8464cdcd" {
		t.Errorf("slug = %s", filepath.Base(d1))
	}
	if WorkDir(tmp, "agent#chan#other") == d1 {
		t.Fatal("different threads share a dir")
	}
}

func TestPersonaHashMatchesTS(t *testing.T) {
	// sha256("be helpful\0")[:16] and sha256("be helpful\0opus")[:16].
	if got := PersonaHash(assignment(nil)); got != "f5844e6ba186dd89" {
		t.Errorf("PersonaHash = %s", got)
	}
	if got := PersonaHash(assignment(func(a *protocol.Assignment) { a.Model = "opus" })); got != "747e7e6de69f0341" {
		t.Errorf("PersonaHash(model) = %s", got)
	}
}

func TestMemoryOnlyStoreNeverWrites(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	s := memStore()
	s.Put(assignment(nil), "s1", "/tmp/x")
	s.AddCost(ThreadKey(assignment(nil)), 5)
	s.Drop(ThreadKey(assignment(nil)))
	if exists(filepath.Join(cwd, "sessions.json")) {
		t.Fatal("memory-only store wrote sessions.json")
	}
}

func TestCorruptOrForeignFilesStartEmpty(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":  "{not json",
		"trailing": `{"a":{}} x`,
		"null":     "null",
		"array":    "[]",
		"string":   `"x"`,
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if n := len(Open(tmp).sessions); n != 0 {
				t.Fatalf("loaded %d entries", n)
			}
		})
	}
}

func TestLoadKeepsHistoryFileOrderAndLastDuplicate(t *testing.T) {
	tmp := t.TempDir()
	live := t.TempDir()
	now := time.Now().UnixMilli()
	entry := func(id string, extra string) string {
		return fmt.Sprintf(`{"sessionId":%q,"personaHash":%q,"workDir":%q,"lastUsed":%d%s}`, id, PersonaHash(withRoot("x")), live, now, extra)
	}
	body := `{"agent-gg#chan1#b":` + entry("first-b", "") +
		`,"agent-gg#chan1#a":` + entry("a", `,"historyTokens":21000`) +
		`,"agent-gg#chan1#b":` + entry("last-b", `,"personaHash":7`) + `}`
	if err := os.WriteFile(filepath.Join(tmp, "sessions.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(tmp)
	// File order is insertion order: b (first position, last value) then a.
	if got := strings.Join(s.ordered(), ","); got != "agent-gg#chan1#b,agent-gg#chan1#a" {
		t.Fatalf("order = %s", got)
	}
	if got := s.sessions["agent-gg#chan1#b"].entry; got.SessionID != "last-b" || got.PersonaHash != "" {
		t.Fatalf("duplicate = %+v", got)
	}
	if got := s.sessions["agent-gg#chan1#a"].entry.HistoryTokens; got != 21000 {
		t.Fatalf("historyTokens = %d", got)
	}
	// A non-string personaHash never matches: the first Get drops it.
	if e := s.Get(withRoot("b")); e != nil {
		t.Fatalf("bad persona hash resumed %+v", e)
	}
}

func TestSavedFileUsesTSKeysAndOrder(t *testing.T) {
	tmp := t.TempDir()
	s := Open(tmp)
	c := fakeClock(s, "2026-09-22T10:00:00Z")
	s.Put(withRoot("z"), "s-z", "/tmp/a&b")
	c.ms++
	s.Put(withRoot("a"), "s-a", "/tmp/y")
	s.AddCost(ThreadKey(withRoot("a")), 12)
	b, err := os.ReadFile(filepath.Join(tmp, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"agent-gg#chan1#z":{"sessionId":"s-z","personaHash":"f5844e6ba186dd89","workDir":"/tmp/a&b","lastUsed":1790071200000},` +
		`"agent-gg#chan1#a":{"sessionId":"s-a","personaHash":"f5844e6ba186dd89","workDir":"/tmp/y","lastUsed":1790071200001,"historyTokens":12}}`
	if string(b) != want {
		t.Fatalf("sessions.json =\n%s\nwant\n%s", b, want)
	}
}

// --- stale-and-fat resume guard ---

func TestDropsColdAndFatButResumesColdAndThin(t *testing.T) {
	s := memStore()
	c := fakeClock(s, "2026-09-22T10:00:00Z")
	a := withRoot("stale-fat")
	s.Put(a, "sess-fat", "/tmp/nonexistent-ex-fat")
	s.AddCost(ThreadKey(a), 60_000) // > 20k fresh tokens recorded
	// Six minutes later: past the 5-minute prompt-cache window, well inside
	// the 7-day session TTL — the stale+fat guard (not the TTL) must fire.
	c.set("2026-09-22T10:06:00Z")
	if e := s.Get(a); e != nil {
		t.Fatalf("stale+fat resumed %+v", e)
	}
	// A stale but THIN session still resumes.
	b := withRoot("stale-thin")
	s.Put(b, "sess-thin", "/tmp/nonexistent-ex-thin")
	s.AddCost(ThreadKey(b), 500)
	c.set("2026-09-22T10:12:00Z")
	if got := sessionID(s.Get(b)); got != "sess-thin" {
		t.Fatalf("stale thin = %s", got)
	}
	// A stale session with NO recorded spend counts as thin too.
	cc := withRoot("stale-uncosted")
	s.Put(cc, "sess-uncosted", "/tmp/nonexistent-ex-uncosted")
	c.set("2026-09-22T10:18:00Z")
	if got := sessionID(s.Get(cc)); got != "sess-uncosted" {
		t.Fatalf("stale uncosted = %s", got)
	}
}

func TestSessionTTLExpires(t *testing.T) {
	s := memStore()
	c := fakeClock(s, "2026-09-22T10:00:00Z")
	a := assignment(nil)
	s.Put(a, "sess-1", "/tmp/x")
	c.set("2026-09-29T10:00:00Z") // exactly 7 days: still inside the TTL
	if s.Get(a) == nil {
		t.Fatal("dropped at exactly the TTL")
	}
	s.Put(a, "sess-1", "/tmp/x")
	c.set("2026-09-29T10:00:00Z")
	c.ms += sessionTTLMs + 1
	if e := s.Get(a); e != nil {
		t.Fatalf("expired session resumed %+v", e)
	}
}

func TestDropUnknownKeyIsNoOp(t *testing.T) {
	s := memStore()
	s.Drop("agent#chan#never-seen")
	if len(s.sessions) != 0 {
		t.Fatal("unexpected entries")
	}
}

// sessions.json names each thread's workdir; a tampered entry must not make
// Drop remove anything but that thread's own dirs under the state dir.
func TestDropOnlyRemovesTheThreadsOwnDirs(t *testing.T) {
	state := t.TempDir()
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "precious"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(state)
	a := assignment(nil)
	key := ThreadKey(a)
	s.Put(a, "sess-1", victim) // what a tampered sessions.json would say
	priv := PrivateDir(state, key)
	s.Drop(key)
	if !exists(filepath.Join(victim, "precious")) {
		t.Fatal("Drop removed a directory outside the state dir")
	}
	if exists(priv) {
		t.Error("the thread's private dir survived Drop")
	}
	// The thread's real workdir is still swept.
	own := WorkDir(state, key)
	s.Put(a, "sess-2", own)
	s.Drop(key)
	if exists(own) {
		t.Error("own workdir survived Drop")
	}
	for _, d := range []string{WorkDir(state, key), PrivateDir(state, key)} {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %o", d, st.Mode().Perm())
		}
	}
	s.Put(a, "sess-3", own)
	if st, _ := os.Stat(filepath.Join(state, "sessions.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("sessions.json mode = %o", st.Mode().Perm())
	}
}
