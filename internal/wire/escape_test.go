package wire

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode"
	"unicode/utf8"
)

// terminalActive reports a character that acts on a terminal or on the line it sits in when
// printed raw: what EscapeJSON must leave only as an escape.
func terminalActive(r rune) bool {
	return r < 0x20 || r == 0x7f || unicode.IsControl(r) || isLineOrBidi(r)
}

func encodeForTest(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEscapeJSON(t *testing.T) {
	label := "x\x1b]0;pwn\a\u009b2J\u202ey"
	in := encodeForTest(t, map[string]string{"label": label})
	got := EscapeJSON(in)
	t.Logf("encoded % x", got)
	want := `{"label":"x\u001b]0;pwn\u0007\u009b2J\u202ey"}` + "\n"
	if string(got) != want {
		t.Errorf("EscapeJSON(%q)\n got %q\nwant %q", in, got, want)
	}

	cases := []string{
		"",
		"plain label: ok",
		"caf\u00e9 \u65e5\u672c \U0001F600 zwj \u200d kept",
		"c1 \u0080\u0085\u009b\u009f",
		"marks \u061c\u200e\u200f",
		"embeddings \u202a\u202b\u202c\u202d\u202e",
		"isolates \u2066\u2067\u2068\u2069",
		"separators \u2028\u2029",
		"ascii \x00\x07\x1b\x7f \\ \" <tag> &",
	}
	for _, s := range cases {
		out := EscapeJSON(encodeForTest(t, s))
		for _, r := range string(out) {
			if r != '\n' && terminalActive(r) {
				t.Errorf("EscapeJSON left %U raw in %q", r, out)
			}
		}
		var back string
		if err := json.Unmarshal(out, &back); err != nil {
			t.Errorf("EscapeJSON(%q) is not valid JSON: %v (%q)", s, err, out)
		} else if back != s {
			t.Errorf("EscapeJSON(%q) decodes to %q", s, back)
		}
	}

	// Without a character to escape the bytes are returned as they are.
	plain := []byte(`{"label":"caf` + "\u00e9" + `"}`)
	if out := EscapeJSON(plain); &out[0] != &plain[0] {
		t.Errorf("EscapeJSON copied a document with nothing to escape")
	}
}

func FuzzEscapeJSON(f *testing.F) {
	for _, s := range []string{"", "a\nb", "\x1b[0m\u009b", "\u202e", "\xff\xc2", "caf\u00e9"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, label string) {
		in := encodeForTest(t, label)
		out := EscapeJSON(in)
		var back, want string
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("EscapeJSON(%q) is not valid JSON: %v", in, err)
		}
		if err := json.Unmarshal(in, &want); err != nil {
			t.Fatal(err)
		}
		if back != want {
			t.Fatalf("EscapeJSON(%q) decodes to %q, want %q", in, back, want)
		}
		for i := 0; i < len(out); {
			r, size := utf8.DecodeRune(out[i:])
			if r != '\n' && terminalActive(r) {
				t.Fatalf("EscapeJSON(%q) = %q left %U raw", in, out, r)
			}
			i += size
		}
	})
}
