package wire

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeGenAndDegraded(t *testing.T) {
	g, err := DecodeGen([]byte(` {"gen":41,"reconciled_ns":1791719990000000000,"x":{"y":[1]}} `))
	if err != nil || g != (Gen{Gen: 41, ReconciledNS: 1791719990000000000}) {
		t.Fatalf("DecodeGen = %+v, %v", g, err)
	}
	for _, body := range []string{``, `{`, `{"gen":1}`, `{"gen":1,"reconciled_ns":2,"gen":3}`,
		`{"gen":1,"reconciled_ns":2,"x":{"a":1,"a":2}}`, `{"gen":1.0,"reconciled_ns":2}`,
		`{"gen":1e3,"reconciled_ns":2}`, `{"gen":-1,"reconciled_ns":2}`, `[1,2]`,
		`{"gen":1,"reconciled_ns":"2"}`, `{"gen":1,"reconciled_ns":2} {}`,
		`{"gen":1,"reconciled_ns":2,"s":"\ud800"}`, "{\"gen\":1,\"reconciled_ns\":2,\"s\":\"\xff\"}",
		`{"gen":1,"reconciled_ns":99999999999999999999}`} {
		if _, err := DecodeGen([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeGen(%q) = %v, want ErrMalformed", body, err)
		}
	}
	b, err := EncodeGen(Gen{Gen: 42, ReconciledNS: 7})
	if err != nil || string(b) != `{"gen":42,"reconciled_ns":7}` {
		t.Errorf("EncodeGen = %s, %v", b, err)
	}
	if _, err := EncodeGen(Gen{Gen: -1}); !errors.Is(err, ErrMalformed) {
		t.Errorf("EncodeGen of a negative gen = %v", err)
	}

	d := Degraded{Reason: "state: <inode> \"mismatch\"\n", FirstNS: 5, Count: 2}
	b, err = EncodeDegraded(d)
	if err != nil || string(b) != `{"reason":"state: <inode> \"mismatch\"\n","first_ns":5,"count":2}` {
		t.Fatalf("EncodeDegraded = %s, %v", b, err)
	}
	if back, err := DecodeDegraded(b); err != nil || back != d {
		t.Errorf("DecodeDegraded(EncodeDegraded) = %+v, %v", back, err)
	}
	for _, body := range []string{`not json`, `{"reason":"x","first_ns":1}`,
		`{"reason":1,"first_ns":1,"count":1}`, `{"reason":"x","first_ns":1,"count":1,"count":2}`} {
		if _, err := DecodeDegraded([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeDegraded(%q) = %v, want ErrMalformed", body, err)
		}
	}
	if _, err := EncodeDegraded(Degraded{Reason: strings.Repeat("x", MaxBodyBytes)}); !errors.Is(err, ErrMalformed) {
		t.Errorf("EncodeDegraded of an oversized reason = %v", err)
	}
	if _, err := EncodeDegraded(Degraded{Reason: "\xff"}); !errors.Is(err, ErrMalformed) {
		t.Errorf("EncodeDegraded of invalid UTF-8 = %v", err)
	}
}
