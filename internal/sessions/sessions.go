// Package sessions is the warm-session cache (buzz-pool-inspired, adapted to
// headless claude): chained conversation rounds resume the SAME claude
// session per (agent, thread) via `claude -p --resume <id>` instead of
// cold-starting and re-sending the whole context bundle every round. The
// session — and its working directory, which claude ties sessions to — lives
// on this machine exactly as long as the conversation stays warm.
//
// The TypeScript runner kept this as module-level state; here it is an
// explicit, mutex-guarded Store.
package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

const maxSessions = 40

// A thread IS a session: coming back to an old thread days later should
// land in the same warm session. The TTL is hygiene for truly dead threads,
// not a conversation boundary.
const sessionTTLMs = 7 * 24 * 60 * 60 * 1000

// Resuming replays the session's WHOLE recorded history through the model.
// Within the provider's prompt-cache lifetime that replay is nearly free;
// after it expires the full history is re-billed as cache writes. So a
// session that is both STALE (cache certainly cold) and FAT (history costs
// more to replay than a cold start's trimmed bundle) must not be resumed.
const (
	promptCacheTTLMs            = 5 * 60 * 1000
	resumeMaxStaleHistoryTokens = 20_000
)

// Entry is one pinned session. The JSON tags are the TypeScript runner's
// SessionEntry keys, so sessions.json stays readable by either runner.
type Entry struct {
	SessionID   string `json:"sessionId"`
	PersonaHash string `json:"personaHash"`
	WorkDir     string `json:"workDir"`
	// LastUsed is epoch milliseconds (JavaScript's Date.now()).
	LastUsed int64 `json:"lastUsed"`
	// Accumulated fresh tokens spent across this session's runs — a proxy for
	// how big a history a resume would replay.
	HistoryTokens int `json:"historyTokens,omitempty"`
}

// slot is a stored entry plus its first-insertion order. The TS cache was a
// JavaScript Map: iteration (and so the stable eviction sort's tie-break and
// the sessions.json key order) follows first insertion, and re-setting an
// existing key keeps its place. seq reproduces that.
type slot struct {
	entry Entry
	seq   uint64
}

// Store is the per-machine session cache.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*slot
	nextSeq  uint64
	// Pins survive app restarts: the map mirrors to <stateDir>/sessions.json.
	// The harness-side conversation state (claude session files, codex
	// rollouts) already lives on disk — losing only the pin map was what
	// forced cold restarts after every app relaunch. Empty = memory only.
	persistPath string
	// stateDir is where thread dirs live; drop only ever removes those.
	stateDir string
	// now is the clock (epoch ms); tests replace it.
	now func() int64
}

// Open loads the session pins persisted under stateDir (TS initSessions). A
// missing or corrupt sessions.json starts empty. An empty stateDir yields a
// memory-only store that never touches disk (the TS module's state before
// initSessions ran).
func Open(stateDir string) *Store {
	s := &Store{
		sessions: map[string]*slot{},
		now:      func() int64 { return time.Now().UnixMilli() },
	}
	if stateDir == "" {
		return s
	}
	s.stateDir = stateDir
	s.persistPath = filepath.Join(stateDir, "sessions.json")
	s.load()
	return s
}

// load reads sessions.json, keeping only live entries whose dir still exists.
// First run or corrupt file — start empty.
func (s *Store) load() {
	raw, err := os.ReadFile(s.persistPath)
	if err != nil || !json.Valid(raw) {
		return
	}
	// Walk the object's members in file order (a Go map would lose it); the
	// document is already known to be valid JSON, so the decoder's errors
	// cannot occur past this point.
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, _ := dec.Token(); t != json.Delim('{') {
		return
	}
	var order []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		t, _ := dec.Token()
		key, _ := t.(string)
		var v json.RawMessage
		_ = dec.Decode(&v)
		if _, dup := vals[key]; !dup {
			order = append(order, key)
		}
		vals[key] = v // JSON.parse: a duplicate key's last value wins
	}
	now := s.now()
	for _, key := range order {
		if e, ok := liveEntry(vals[key], now); ok {
			s.set(key, e)
		}
	}
}

// liveEntry validates one persisted entry exactly like the TS loader: an
// object with string sessionId and workDir, a lastUsed inside the TTL, and a
// workDir that still exists on disk.
func liveEntry(raw json.RawMessage, now int64) (Entry, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return Entry{}, false
	}
	sessionID, ok1 := m["sessionId"].(string)
	workDir, ok2 := m["workDir"].(string)
	lastUsed, ok3 := m["lastUsed"].(float64)
	if !ok1 || !ok2 || !ok3 || float64(now)-lastUsed > sessionTTLMs {
		return Entry{}, false
	}
	if _, err := os.Stat(workDir); err != nil {
		return Entry{}, false
	}
	// A non-string personaHash never matches a real hash: the first Get
	// drops it, as the TS strict comparison did.
	personaHash, _ := m["personaHash"].(string)
	history, _ := m["historyTokens"].(float64)
	return Entry{
		SessionID:     sessionID,
		PersonaHash:   personaHash,
		WorkDir:       workDir,
		LastUsed:      int64(lastUsed),
		HistoryTokens: int(history),
	}, true
}

// set stores e under key, keeping the key's original insertion position.
func (s *Store) set(key string, e Entry) {
	if sl, ok := s.sessions[key]; ok {
		sl.entry = e
		return
	}
	s.nextSeq++
	s.sessions[key] = &slot{entry: e, seq: s.nextSeq}
}

// ordered returns the keys in insertion order.
func (s *Store) ordered() []string {
	keys := make([]string, 0, len(s.sessions))
	for k := range s.sessions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return s.sessions[keys[i]].seq < s.sessions[keys[j]].seq })
	return keys
}

// save mirrors the map to sessions.json. Best-effort: an unsaved pin just
// means a cold start after restart.
func (s *Store) save() {
	if s.persistPath == "" {
		return
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range s.ordered() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshal(key))
		b.WriteByte(':')
		b.Write(marshal(s.sessions[key].entry))
	}
	b.WriteByte('}')
	_ = os.WriteFile(s.persistPath, b.Bytes(), 0o600)
}

// marshal encodes like JSON.stringify: no HTML escaping, no trailing newline.
// Strings and Entry values cannot fail to encode.
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// ThreadKey identifies one agent's seat in one conversation thread.
func ThreadKey(a protocol.Assignment) string {
	root := a.ThreadRootID
	if root == "" {
		root = a.MessageID
	}
	return a.AgentID + "#" + a.ParentID + "#" + root
}

// PersonaHash fingerprints what a session was primed with: persona + model.
func PersonaHash(a protocol.Assignment) string {
	sum := sha256.Sum256([]byte(a.Persona + "\x00" + a.Model))
	return hex.EncodeToString(sum[:])[:16]
}

// WorkDir returns a STABLE per-thread working directory — claude sessions are
// bound to their cwd, so resuming requires the same dir. It is created here
// (TS sessionWorkDir); a failure to create it surfaces when the harness
// writes its per-run config into it.
func WorkDir(stateDir, key string) string {
	dir := filepath.Join(stateDir, "threads", keyHash(key))
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// PrivateDir is the thread's owner-only dir OUTSIDE everything the agent may
// touch without a card: per-run secrets (mcp.json) and the dev-server pid
// files (stopping a server signals whatever its pid file names).
func PrivateDir(stateDir, key string) string {
	dir := filepath.Join(stateDir, "private", keyHash(key))
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// Get returns a resumable session for this assignment, or nil when none
// exists / the persona or model changed (a re-pin mid-thread must not resume
// a session primed with the old persona). The returned Entry is a copy.
func (s *Store) Get(a protocol.Assignment) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ThreadKey(a)
	sl, ok := s.sessions[key]
	if !ok {
		return nil
	}
	age := s.now() - sl.entry.LastUsed
	if age > sessionTTLMs || sl.entry.PersonaHash != PersonaHash(a) {
		s.drop(key)
		return nil
	}
	// Stale + fat → a resume would re-bill the whole history as cache writes,
	// costing MORE than a cold start's trimmed bundle. Start cold instead;
	// conversational continuity survives via the thread window in the bundle.
	if age > promptCacheTTLMs && sl.entry.HistoryTokens > resumeMaxStaleHistoryTokens {
		s.drop(key)
		return nil
	}
	e := sl.entry
	return &e
}

// AddCost accumulates a finished run's fresh-token spend onto its session —
// the resume-vs-cold decision in Get reads it.
func (s *Store) AddCost(key string, tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.sessions[key]
	if !ok || tokens <= 0 {
		return
	}
	sl.entry.HistoryTokens += tokens
	s.save()
}

// Put records the session the harness reported for this thread. Like the TS
// putSession it writes a fresh entry: any accumulated cost starts over.
func (s *Store) Put(a protocol.Assignment, sessionID, workDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set(ThreadKey(a), Entry{
		SessionID:   sessionID,
		PersonaHash: PersonaHash(a),
		WorkDir:     workDir,
		LastUsed:    s.now(),
	})
	s.evict()
	s.save()
}

// Drop forgets a session (failed resume, eviction) and sweeps its dir.
//
// The sweep MUST be synchronous. Get drops a stale-and-fat session and
// returns nil, whereupon the runner immediately recreates the SAME path via
// WorkDir() for the cold start. An async removal loses that race: it lands
// after the mkdir and deletes the directory the run is about to work in, and
// the harness then dies writing mcp.json into it ("ENOENT ... /mcp.json",
// 0 turns, run marked failed). Ordering here is load-bearing, not hygiene.
func (s *Store) Drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop(key)
}

func (s *Store) drop(key string) {
	sl, ok := s.sessions[key]
	delete(s.sessions, key)
	if ok {
		// Best effort: a dir we cannot remove is stale state, not a reason to
		// fail the run that is about to start. A persisted store only ever
		// removes the thread's own dirs: sessions.json names the workdir, and
		// a tampered file must not turn this into `rm -rf` of anything else.
		if s.stateDir == "" || sl.entry.WorkDir == filepath.Join(s.stateDir, "threads", keyHash(key)) {
			_ = os.RemoveAll(sl.entry.WorkDir)
		}
		if s.stateDir != "" {
			_ = os.RemoveAll(filepath.Join(s.stateDir, "private", keyHash(key)))
		}
	}
	s.save()
}

// evict drops the least-recently-used sessions past the cap (ties broken by
// insertion order, as the TS stable sort over the Map did).
func (s *Store) evict() {
	if len(s.sessions) <= maxSessions {
		return
	}
	keys := s.ordered()
	sort.SliceStable(keys, func(i, j int) bool {
		return s.sessions[keys[i]].entry.LastUsed < s.sessions[keys[j]].entry.LastUsed
	})
	for _, key := range keys[:len(keys)-maxSessions] {
		s.drop(key)
	}
}
