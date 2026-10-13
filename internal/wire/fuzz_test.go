package wire

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// seedBodies adds every fixture body of a kind to the corpus.
func seedBodies(f *testing.F, kind string) {
	files, err := filepath.Glob(filepath.Join("testdata", "bodies", kind, "*.body"))
	if err != nil || len(files) == 0 {
		f.Fatalf("no %s fixtures: %v", kind, err)
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
}

// checkCodec holds for every body a decoder accepts: it re-encodes, the encoding decodes to the
// same value and re-encodes to the same bytes. A body it refuses is ErrMalformed, never anything
// else, and nothing panics.
func checkCodec[T any](t *testing.T, body []byte, decode func([]byte) (T, error)) {
	t.Helper()
	v, err := decode(body)
	if err != nil {
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("refusal is not ErrMalformed: %v", err)
		}
		return
	}
	enc, err := Encode(v)
	if err != nil {
		t.Fatalf("accepted %q but cannot encode %+v: %v", body, v, err)
	}
	back, err := decode(enc)
	if err != nil {
		t.Fatalf("encoding %q does not decode: %v", enc, err)
	}
	if !reflect.DeepEqual(back, v) {
		t.Fatalf("round trip changed the value: %+v -> %+v", v, back)
	}
	again, err := Encode(back)
	if err != nil || !bytes.Equal(again, enc) {
		t.Fatalf("encoding is not stable: %q vs %q (%v)", enc, again, err)
	}
}

func FuzzDecodeTicket(f *testing.F) {
	seedBodies(f, "ticket")
	f.Fuzz(func(t *testing.T, body []byte) { checkCodec(t, body, DecodeTicket) })
}

func FuzzDecodeHold(f *testing.F) {
	seedBodies(f, "hold")
	f.Fuzz(func(t *testing.T, body []byte) { checkCodec(t, body, DecodeHold) })
}

func FuzzDecodeCancel(f *testing.F) {
	seedBodies(f, "cancel")
	f.Fuzz(func(t *testing.T, body []byte) { checkCodec(t, body, DecodeCancel) })
}

func FuzzParseName(f *testing.F) {
	for _, s := range []string{
		"01791720000000000000-0000004711-000001-gate_unit",
		"01791720000000000000-0000004711-000001",
		"18446744073709551615-9999999999-999999-a.b_C9",
		"01791720000000000000-0000004711-000001-", ".tmp-0000004711-000001", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		id, ok := ParseName(name)
		if ok {
			// An accepted name is the ID's spelling, then nothing or "-" and a display.
			if len(name) < idLen || name[:idLen] != id.String() {
				t.Fatalf("ParseName(%q) = %+v, which spells %q", name, id, id.String())
			}
			if rest := name[idLen:]; rest != "" && rest[0] != '-' {
				t.Fatalf("ParseName(%q) accepted a tail %q", name, rest)
			}
			if got, ok := ParseID(id.String()); !ok || got != id {
				t.Fatalf("ParseID(%q) = %+v, %v", id.String(), got, ok)
			}
		}
		if cid, cok := ParseID(name); cok && (!ok || cid != id || name != id.String()) {
			t.Fatalf("ParseID(%q) accepted but ParseName gives %+v, %v", name, id, ok)
		}
	})
}

func FuzzEscapeHuman(f *testing.F) {
	for _, s := range []string{"", "a\nb", "\x1b[0m", "\\n", "\xff", "café"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, label string) {
		out := EscapeHuman(label)
		for i := 0; i < len(out); i++ {
			if c := out[i]; c < 0x20 || c == 0x7f {
				t.Fatalf("EscapeHuman(%q) = %q contains control byte %#x", label, out, c)
			}
		}
		if EscapeHuman(label+"x") != out+"x" {
			t.Fatalf("EscapeHuman is not per-character for %q", label)
		}
	})
}
