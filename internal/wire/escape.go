package wire

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func displayByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_'
}

// Display derives the informational suffix of a file name from a label: every byte outside
// [A-Za-z0-9._] becomes "_" and the first 32 bytes are kept. A label that is empty has no
// display. Readers never interpret it (PROTOCOL.md section 7.1).
func Display(label string) string {
	n := min(len(label), maxDisplay)
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		if displayByte(label[i]) {
			b[i] = label[i]
		} else {
			b[i] = '_'
		}
	}
	return string(b)
}

// EscapeHuman makes a label safe to print on one line of a terminal. A backslash becomes `\\`,
// newline, carriage return and tab become `\n`, `\r` and `\t`, other ASCII control characters
// `\xNN`, bytes that are not UTF-8 `\xNN`, and other characters that act on a terminal or on
// the line they sit in (C1 controls, line and paragraph separators, bidirectional overrides)
// `\uNNNN`. Everything else, including every printable non-ASCII character, is kept. Distinct
// labels have distinct escapes.
func EscapeHuman(label string) string {
	var b strings.Builder
	const hex = "0123456789abcdef"
	for i := 0; i < len(label); {
		r, size := utf8.DecodeRuneInString(label[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			c := label[i]
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x80 && unicode.IsControl(r):
			b.WriteString(`\x`)
			b.WriteByte(hex[r>>4])
			b.WriteByte(hex[r&15])
		case unicode.IsControl(r) || isLineOrBidi(r):
			b.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				b.WriteByte(hex[(r>>shift)&15])
			}
		default:
			b.WriteString(label[i : i+size])
		}
		i += size
	}
	return b.String()
}

func isLineOrBidi(r rune) bool {
	switch {
	case r == 0x2028 || r == 0x2029: // line and paragraph separators
		return true
	case r == 0x061c || r == 0x200e || r == 0x200f: // marks that reorder text
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // embeddings, overrides, isolates
		return true
	}
	return false
}
