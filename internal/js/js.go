// Package js gives Go strings the JavaScript string semantics nib's behavior was defined with: UTF-16 lengths and offsets, and JS whitespace.
package js

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// IsSpace reports whether r is whitespace to JavaScript's trim and \s.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Trim is String.prototype.trim.
func Trim(s string) string { return strings.TrimFunc(s, IsSpace) }

// TrimStart is String.prototype.trimStart.
func TrimStart(s string) string { return strings.TrimLeftFunc(s, IsSpace) }

// TrimEnd is String.prototype.trimEnd.
func TrimEnd(s string) string { return strings.TrimRightFunc(s, IsSpace) }

// Len is a JavaScript string's length: UTF-16 code units.
func Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// ByteOffset is the byte offset of UTF-16 offset units into s, clamped to len(s); an offset inside a surrogate pair rounds up past the character.
func ByteOffset(s string, units int) int {
	if units <= 0 {
		return 0
	}
	n := 0
	for i, r := range s {
		if n >= units {
			return i
		}
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return len(s)
}

// Slice is String.prototype.slice with UTF-16 offsets; a cut through a surrogate pair keeps the whole character.
func Slice(s string, start, end int) string {
	length := Len(s)
	if start < 0 {
		start = max(0, length+start)
	}
	if end < 0 {
		end = max(0, length+end)
	}
	start, end = min(start, length), min(end, length)
	if start >= end {
		return ""
	}
	return s[ByteOffset(s, start):ByteOffset(s, end)]
}

// UnitsOf is the UTF-16 length of the first n bytes of s.
func UnitsOf(s string, n int) int {
	if n > len(s) {
		n = len(s)
	}
	return Len(s[:n])
}

// Runes counts code points, as [...s].length does.
func Runes(s string) int { return utf8.RuneCountInString(s) }

// Units encodes s as UTF-16 code units, for code that must compare JS strings unit by unit.
func Units(s string) []uint16 { return utf16.Encode([]rune(s)) }

// Quote is JSON.stringify for a string.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[r>>4])
				b.WriteByte("0123456789abcdef"[r&0xf])
			} else if r == utf8.RuneError {
				b.WriteString(`\ufffd`)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
