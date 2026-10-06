package spill

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestPassesSmallResultsThroughUnchanged(t *testing.T) {
	s := NewStore()
	if got := s.MaybeSpill("get_thread", "short result"); got != "short result" {
		t.Fatalf("got %q", got)
	}
}

func TestSpillsOversizedResultsIntoPreview(t *testing.T) {
	s := newStore(100, totalMax) // tiny inline cap for the test
	full := strings.Repeat("H", Head) + strings.Repeat("M", 5000) + strings.Repeat("T", Tail)
	out := s.MaybeSpill("search_messages", full)
	if out == full {
		t.Fatal("not spilled")
	}
	for _, want := range []string{"sp-1", fmt.Sprintf("%d chars", len(full)), strings.Repeat("H", Head), strings.Repeat("T", Tail)} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %.40q", want)
		}
	}
	if strings.Contains(out, strings.Repeat("M", 5000)) {
		t.Error("middle not omitted")
	}
	if len(out) >= len(full) {
		t.Errorf("preview %d >= full %d", len(out), len(full))
	}
}

func TestPreviewExactWording(t *testing.T) {
	s := NewStore()
	full := strings.Repeat("a", Head) + strings.Repeat("b", InlineMax) + strings.Repeat("c", Tail)
	out := s.MaybeSpill("read_channel", full)
	want := "[read_channel returned 21504 chars — too large to inline. " +
		"Showing the first 4096 and last 1024. The FULL result is stored: " +
		`read more with fetch_spill(locator="sp-1", offset=<char offset>, length=<≤16384>). ` +
		"Only fetch what the task actually needs.]\n" +
		strings.Repeat("a", Head) +
		"\n…[16384 chars omitted — fetch_spill \"sp-1\" offset 4096]…\n" +
		strings.Repeat("c", Tail)
	if out != want {
		t.Fatalf("preview mismatch:\n%.400q\nwant\n%.400q", out, want)
	}
	// Locators count up per spill.
	if got := s.MaybeSpill("x", full); !strings.Contains(got, `locator="sp-2"`) {
		t.Errorf("second locator: %.200q", got)
	}
}

func TestLengthsAreCharactersNotBytes(t *testing.T) {
	s := NewStore()
	// 10k two-byte characters: 20k bytes, but only 10k chars — inline.
	wide := strings.Repeat("é", 10_000)
	if got := s.MaybeSpill("t", wide); got != wide {
		t.Fatal("char-count under the cap must stay inline")
	}
	// Over the cap in characters: head/tail are whole characters.
	full := strings.Repeat("é", Head) + strings.Repeat("€", InlineMax) + strings.Repeat("ü", Tail)
	out := s.MaybeSpill("t", full)
	if !strings.Contains(out, "returned 21504 chars") {
		t.Errorf("count in chars: %.120q", out)
	}
	if !strings.Contains(out, "]\n"+strings.Repeat("é", Head)+"\n…[") || !strings.HasSuffix(out, "]…\n"+strings.Repeat("ü", Tail)) {
		t.Error("head/tail not character-sliced")
	}
	got, isErr := s.Fetch("sp-1", Head-1, 2)
	if isErr || got != "[sp-1 chars 4095–4097 of 21504] More remains: next offset 4097.\né€" {
		t.Errorf("fetch = %q", got)
	}
}

func TestTinyCapTailIsWholeText(t *testing.T) {
	// text.slice(text.length - SPILL_TAIL) with a negative start is the whole
	// text; the omitted count goes negative, as in the TS.
	s := newStore(10, totalMax)
	text := strings.Repeat("y", 50)
	out := s.MaybeSpill("t", text)
	if !strings.Contains(out, "]\n"+text+"\n…[-5070 chars omitted") || !strings.HasSuffix(out, "]…\n"+text) {
		t.Errorf("out = %q", out)
	}
}

func TestFetchPagesClampedToFetchCap(t *testing.T) {
	s := newStore(10, totalMax)
	full := strings.Repeat("x", FetchMax*2+500)
	s.MaybeSpill("read_channel", full)

	first, isErr := s.Fetch("sp-1", 0, FetchMax*10) // over-ask is clamped
	if isErr {
		t.Fatal("unexpected error")
	}
	for _, want := range []string{fmt.Sprintf("chars 0–%d", FetchMax), fmt.Sprintf("next offset %d", FetchMax)} {
		if !strings.Contains(first, want) {
			t.Errorf("first missing %q: %.100q", want, first)
		}
	}
	if want := fmt.Sprintf("[sp-1 chars 0–%d of %d] More remains: next offset %d.\n", FetchMax, len(full), FetchMax); !strings.HasPrefix(first, want) {
		t.Errorf("first header = %.100q", first)
	}
	if len(first) != len(fmt.Sprintf("[sp-1 chars 0–%d of %d] More remains: next offset %d.\n", FetchMax, len(full), FetchMax))+FetchMax {
		t.Errorf("slice not clamped: %d", len(first))
	}

	last, isErr := s.Fetch("sp-1", FetchMax*2, 1000)
	if isErr || !strings.Contains(last, "End of result.") {
		t.Errorf("last = %.100q", last)
	}
	if want := fmt.Sprintf("[sp-1 chars %d–%d of %d] End of result.\n%s", FetchMax*2, len(full), len(full), strings.Repeat("x", 500)); last != want {
		t.Errorf("last = %.100q", last)
	}
}

func TestFetchClampsOffsetAndLength(t *testing.T) {
	s := newStore(10, totalMax)
	s.MaybeSpill("t", "0123456789abcdef")
	if got, _ := s.Fetch("sp-1", -5, 3); got != "[sp-1 chars 0–3 of 16] More remains: next offset 3.\n012" {
		t.Errorf("negative offset: %q", got)
	}
	if got, _ := s.Fetch("sp-1", 4, 0); got != "[sp-1 chars 4–5 of 16] More remains: next offset 5.\n4" {
		t.Errorf("zero length: %q", got)
	}
	if got, _ := s.Fetch("sp-1", 4, -9); got != "[sp-1 chars 4–5 of 16] More remains: next offset 5.\n4" {
		t.Errorf("negative length: %q", got)
	}
}

func TestRejectsUnknownLocatorsAndOutOfRangeOffsets(t *testing.T) {
	s := newStore(10, totalMax)
	s.MaybeSpill("t", strings.Repeat("y", 50))
	text, isErr := s.Fetch("sp-99", 0, FetchMax)
	if !isErr || !strings.Contains(text, "unknown spill locator") {
		t.Errorf("unknown = %q %v", text, isErr)
	}
	if text != `unknown spill locator "sp-99" — locators only live for this run` {
		t.Errorf("unknown wording = %q", text)
	}
	past, isErr := s.Fetch("sp-1", 10_000, FetchMax)
	if !isErr || !strings.Contains(past, "past the end") {
		t.Errorf("past = %q %v", past, isErr)
	}
	if past != "offset 10000 is past the end (50 chars total)" {
		t.Errorf("past wording = %q", past)
	}
	if exact, isErr := s.Fetch("sp-1", 50, 1); !isErr || exact != "offset 50 is past the end (50 chars total)" {
		t.Errorf("offset == length must be past the end: %q", exact)
	}
}

func TestEvictsOldestOnceTotalCapExceeded(t *testing.T) {
	s := newStore(10, 100)                     // total cap 100 chars
	s.MaybeSpill("a", strings.Repeat("1", 60)) // sp-1
	s.MaybeSpill("b", strings.Repeat("2", 60)) // sp-2 → total 120 → evict sp-1
	if _, isErr := s.Fetch("sp-1", 0, FetchMax); !isErr {
		t.Error("sp-1 should be evicted")
	}
	if _, isErr := s.Fetch("sp-2", 0, FetchMax); isErr {
		t.Error("sp-2 should remain")
	}
	if s.total != 60 || len(s.order) != 1 || len(s.entries) != 1 {
		t.Errorf("bookkeeping: total=%d order=%v entries=%d", s.total, s.order, len(s.entries))
	}
}

func TestEvictsAnEntryThatAloneExceedsTheCap(t *testing.T) {
	// The TS loop walks every entry, so a single result over the total cap
	// evicts itself; the preview still names its (now dead) locator.
	s := newStore(10, 100)
	out := s.MaybeSpill("a", strings.Repeat("z", 150))
	if !strings.Contains(out, `locator="sp-1"`) {
		t.Errorf("preview = %.120q", out)
	}
	if _, isErr := s.Fetch("sp-1", 0, FetchMax); !isErr {
		t.Error("oversized entry should be gone")
	}
	if s.total != 0 || len(s.order) != 0 {
		t.Errorf("bookkeeping: total=%d order=%v", s.total, s.order)
	}
}

func TestConcurrentUse(t *testing.T) {
	s := newStore(10, 1_000)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := s.MaybeSpill("t", strings.Repeat(fmt.Sprint(i%10), 40))
			_ = out
			s.Fetch("sp-1", 0, 5)
		}()
	}
	wg.Wait()
	if s.seq != 32 {
		t.Errorf("seq = %d", s.seq)
	}
	if s.total > 1_000 {
		t.Errorf("total over cap: %d", s.total)
	}
}

func TestByteOffset(t *testing.T) {
	cases := []struct {
		s    string
		i, w int
	}{
		{"héllo", -1, 0},
		{"héllo", 0, 0},
		{"héllo", 2, 3},
		{"héllo", 5, 6},
		{"héllo", 9, 6},
	}
	for _, c := range cases {
		if got := byteOffset(c.s, c.i); got != c.w {
			t.Errorf("byteOffset(%q, %d) = %d, want %d", c.s, c.i, got, c.w)
		}
	}
}
