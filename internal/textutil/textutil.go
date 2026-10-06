// Package textutil holds the small string helpers every package shares, so
// clipping and sizing behave the same everywhere. Lengths are counted in
// characters (runes), never bytes: a clip must not split a multi-byte
// character, and model-facing limits are about how much text there is.
package textutil

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Len is the length of s in characters.
func Len(s string) int { return utf8.RuneCountInString(s) }

// Head returns the first n characters of s (all of s when shorter).
func Head(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Tail returns the last n characters of s (all of s when shorter).
func Tail(s string, n int) string {
	r := []rune(s)
	if n >= len(r) {
		return s
	}
	if n <= 0 {
		return ""
	}
	return string(r[len(r)-n:])
}

// Slice returns characters [start, end) of s, clamped to its bounds.
func Slice(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end <= start {
		return ""
	}
	return Head(Tail(s, Len(s)-start), end-start)
}

// Clip returns s cut to n characters with a trailing "…" when it was longer.
func Clip(s string, n int) string {
	if Len(s) > n {
		return Head(s, n) + "…"
	}
	return s
}

// JSSpaceClass is JavaScript's \s (WhiteSpace + LineTerminator) as a Go
// regexp class. Go's own \s is ASCII-only and lacks \v, so it can't stand in
// where the TS used /\s/ or String.prototype.trim.
const JSSpaceClass = `[` + JSSpaceChars + `]`

// JSSpaceChars is the inside of JSSpaceClass, for building negated or
// combined classes.
const JSSpaceChars = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// IsJSSpace reports whether r is JavaScript whitespace. It differs from
// unicode.IsSpace on U+0085 (not JS whitespace) and U+FEFF (JS whitespace).
func IsJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// JSTrim is String.prototype.trim.
func JSTrim(s string) string { return strings.TrimFunc(s, IsJSSpace) }

var spaceRun = regexp.MustCompile(JSSpaceClass + `+`)

// CollapseSpace is s.replace(/\s+/g, ' ').trim().
func CollapseSpace(s string) string { return JSTrim(spaceRun.ReplaceAllString(s, " ")) }

// SizeLabel renders a character count as "812B" or "3.4KB" — the TS
// (n / 1024).toFixed(1), which rounds a tie up (1280 → "1.3KB"), where Go's
// %.1f would round it to even.
func SizeLabel(n int) string {
	if n >= 1024 {
		tenths := (n*10 + 512) / 1024
		return fmt.Sprintf("%d.%dKB", tenths/10, tenths%10)
	}
	return fmt.Sprintf("%dB", n)
}

// JSString renders a decoded-JSON value the way JavaScript's String() does
// for the shapes tool arguments carry: integral numbers without a decimal
// point, strings verbatim, null as "null", and composite values as JSON.
func JSString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e21 {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

// Str reads a string field from decoded JSON arguments ("" when absent or
// not a string).
func Str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
