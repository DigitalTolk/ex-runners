// Package spill is the spill store: oversized tool results are persisted here
// instead of being dumped inline (wasting context) or silently truncated
// (losing data — the old failure mode). The model gets a head/tail PREVIEW
// plus an opaque locator it can page through with the fetch_spill tool. Idea
// adapted from DeepSeek Harness's spill subsystem.
//
// Offsets/lengths are in CHARACTERS — the units the model sees in the
// preview — not bytes. The store lives in the per-run MCP server process, so
// entries share the run's lifetime; a modest total cap evicts oldest-first as
// a runaway backstop.
package spill

import (
	"strconv"
	"sync"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

const (
	// InlineMax: results longer than this are spilled. Generous: most tool
	// results fit far under it, so only genuinely huge dumps (whole-thread
	// reads, big searches) take the preview path.
	InlineMax = 16_384
	// Head and Tail are the preview kept inline when spilling: enough head to
	// act on, a tail so the model can see how the result ends (lists,
	// summaries, closing state).
	Head = 4_096
	Tail = 1_024
	// FetchMax is the per-fetch slice cap — paging through a spill costs one
	// tool round per FetchMax chars.
	FetchMax = 16_384
	// totalMax is the total chars retained across all entries before
	// oldest-first eviction.
	totalMax = 32 * 1024 * 1024
)

type entry struct {
	text string
	n    int // length in characters
}

// Store holds the spilled results of one run. It is safe for concurrent use:
// tool calls may run concurrently.
type Store struct {
	inlineMax int
	totalMax  int

	mu      sync.Mutex
	entries map[string]entry // locator → full text
	order   []string         // locators, oldest first (insertion order)
	seq     int
	total   int
}

// NewStore returns an empty store with the production caps.
func NewStore() *Store { return newStore(InlineMax, totalMax) }

func newStore(inlineMax, totalMax int) *Store {
	return &Store{inlineMax: inlineMax, totalMax: totalMax, entries: map[string]entry{}}
}

// MaybeSpill returns the text unchanged when it fits inline; otherwise it
// stores the full text and returns the preview + retrieval instructions.
func (s *Store) MaybeSpill(tool, text string) string {
	// A string never has more characters than bytes, so short byte lengths
	// skip the character count entirely.
	if len(text) <= s.inlineMax {
		return text
	}
	n := textutil.Len(text)
	if n <= s.inlineMax {
		return text
	}

	s.mu.Lock()
	s.seq++
	locator := "sp-" + strconv.Itoa(s.seq)
	s.entries[locator] = entry{text: text, n: n}
	s.order = append(s.order, locator)
	s.total += n
	s.evict()
	s.mu.Unlock()

	head := textutil.Head(text, Head)
	// Same as JS text.slice(text.length - Tail): the whole text when it is
	// shorter than the tail (only possible with a tiny inline cap).
	tail := text[byteOffset(text, n-Tail):]
	omitted := n - Head - Tail
	return "[" + tool + " returned " + strconv.Itoa(n) + " chars — too large to inline. " +
		"Showing the first " + strconv.Itoa(Head) + " and last " + strconv.Itoa(Tail) + ". The FULL result is stored: " +
		`read more with fetch_spill(locator="` + locator + `", offset=<char offset>, length=<≤` + strconv.Itoa(FetchMax) + ">). " +
		"Only fetch what the task actually needs.]\n" +
		head +
		"\n…[" + strconv.Itoa(omitted) + ` chars omitted — fetch_spill "` + locator + `" offset ` + strconv.Itoa(Head) + "]…\n" +
		tail
}

// Fetch returns one slice of a spilled result, clamped to FetchMax, with
// position metadata so the model can keep paging. The TS defaults (offset 0,
// length FetchMax) are the caller's to pass.
func (s *Store) Fetch(locator string, offset, length int) (text string, isError bool) {
	s.mu.Lock()
	e, ok := s.entries[locator]
	s.mu.Unlock()
	if !ok {
		return `unknown spill locator "` + locator + `" — locators only live for this run`, true
	}
	start := max(0, offset)
	if start >= e.n {
		return "offset " + strconv.Itoa(start) + " is past the end (" + strconv.Itoa(e.n) + " chars total)", true
	}
	n := min(max(1, length), FetchMax)
	slice := textutil.Head(e.text[byteOffset(e.text, start):], n)
	end := start + min(n, e.n-start)
	more := " End of result."
	if end < e.n {
		more = " More remains: next offset " + strconv.Itoa(end) + "."
	}
	return "[" + locator + " chars " + strconv.Itoa(start) + "–" + strconv.Itoa(end) + " of " + strconv.Itoa(e.n) + "]" + more + "\n" + slice, false
}

// evict drops oldest entries until under the total cap — a runaway backstop,
// not an expected path. Like the TS original it may drop the entry just
// added when that one alone exceeds the cap. Callers hold s.mu.
func (s *Store) evict() {
	for len(s.order) > 0 && s.total > s.totalMax {
		loc := s.order[0]
		s.order = s.order[1:]
		s.total -= s.entries[loc].n
		delete(s.entries, loc)
	}
}

// byteOffset is the byte index of character i in s (0 for i <= 0, len(s)
// past the end). Walking the string avoids materialising a spilled result
// (up to the 32M-char cap) as a []rune just to slice it.
func byteOffset(s string, i int) int {
	if i <= 0 {
		return 0
	}
	c := 0
	for pos := range s {
		if c == i {
			return pos
		}
		c++
	}
	return len(s)
}
