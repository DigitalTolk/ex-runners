package textutil

import (
	"math"
	"testing"
)

func TestHeadTailSliceClip(t *testing.T) {
	s := "héllo👋 world"
	if got := Len(s); got != 12 {
		t.Fatalf("Len = %d", got)
	}
	if got := Head(s, 6); got != "héllo👋" {
		t.Errorf("Head = %q", got)
	}
	if got := Head(s, 0); got != "" {
		t.Errorf("Head(0) = %q", got)
	}
	if got := Head("ab", 5); got != "ab" {
		t.Errorf("Head(long n) = %q", got)
	}
	if got := Tail(s, 5); got != "world" {
		t.Errorf("Tail = %q", got)
	}
	if got := Tail(s, 99); got != s {
		t.Errorf("Tail(long n) = %q", got)
	}
	if got := Tail(s, 0); got != "" {
		t.Errorf("Tail(0) = %q", got)
	}
	if got := Slice(s, 1, 6); got != "éllo👋" {
		t.Errorf("Slice = %q", got)
	}
	if got := Slice(s, -3, 2); got != "hé" {
		t.Errorf("Slice(neg start) = %q", got)
	}
	if got := Slice(s, 4, 4); got != "" {
		t.Errorf("Slice(empty) = %q", got)
	}
	if got := Clip("abcdef", 3); got != "abc…" {
		t.Errorf("Clip = %q", got)
	}
	if got := Clip("abc", 3); got != "abc" {
		t.Errorf("Clip(fits) = %q", got)
	}
}

func TestCollapseSizeJSString(t *testing.T) {
	if got := CollapseSpace("  a\n\n b\tc  "); got != "a b c" {
		t.Errorf("CollapseSpace = %q", got)
	}
	// JS whitespace: \v, NBSP and the BOM collapse; U+0085 does not.
	if got := CollapseSpace("\ufeff a\v\u00a0b\u2028c\u0085d \u3000"); got != "a b c\u0085d" {
		t.Errorf("CollapseSpace(JS whitespace) = %q", got)
	}
	if JSTrim("\u2009x\u200a") != "x" || IsJSSpace('\u0085') || !IsJSSpace('\u2005') {
		t.Error("JSTrim/IsJSSpace")
	}
	for n, want := range map[int]string{812: "812B", 1023: "1023B", 1024: "1.0KB", 1280: "1.3KB", 1331: "1.3KB", 3482: "3.4KB", 10188: "9.9KB", 10189: "10.0KB"} {
		if got := SizeLabel(n); got != want {
			t.Errorf("SizeLabel(%d) = %q, want %q", n, got, want)
		}
	}
	cases := map[string]any{
		"null":      nil,
		"x":         "x",
		"true":      true,
		"5":         float64(5),
		"5.5":       5.5,
		"1e+21":     1e21,
		"7":         7,
		`{"a":1}`:   map[string]any{"a": 1},
		`["a","b"]`: []any{"a", "b"},
		"(0x0,0x0)": complex(0, 0),
	}
	for want, in := range cases {
		got := JSString(in)
		if want == "(0x0,0x0)" {
			// Unmarshalable values fall back to fmt formatting.
			if got != "(0+0i)" {
				t.Errorf("JSString(complex) = %q", got)
			}
			continue
		}
		if got != want {
			t.Errorf("JSString(%v) = %q, want %q", in, got, want)
		}
	}
	if JSString(math.Inf(1)) != "+Inf" {
		t.Errorf("JSString(Inf) = %q", JSString(math.Inf(1)))
	}
	if Str(map[string]any{"k": "v", "n": 1}, "k") != "v" || Str(map[string]any{"n": 1}, "n") != "" {
		t.Error("Str")
	}
}
