package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

func TestNameRoundTrip(t *testing.T) {
	cases := []struct {
		id      ID
		label   string
		want    string
		display string
	}{
		{ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 1}, "gate:unit",
			"01791720000000000000-0000004711-000001-gate_unit", "gate_unit"},
		{ID{OrderKey: 1791719345000000000, PID: 5120, Seq: 7}, "ci:o-r",
			"01791719345000000000-0000005120-000007-ci_o_r", "ci_o_r"},
		{ID{OrderKey: 0, PID: 0, Seq: 0}, "", "00000000000000000000-0000000000-000000", ""},
		{ID{OrderKey: math.MaxUint64, PID: 9999999999, Seq: 999999}, "a.b_C9",
			"18446744073709551615-9999999999-999999-a.b_C9", "a.b_C9"},
	}
	for _, c := range cases {
		name := TicketName(c.id, c.label)
		if name != c.want {
			t.Errorf("TicketName(%+v, %q) = %q, want %q", c.id, c.label, name, c.want)
		}
		got, ok := ParseName(name)
		if !ok || got != c.id {
			t.Errorf("ParseName(%q) = %+v, %v; want %+v", name, got, ok, c.id)
		}
		if d := Display(c.label); d != c.display {
			t.Errorf("Display(%q) = %q, want %q", c.label, d, c.display)
		}
	}
	// A hold name has the same shape, with the start time in the first field.
	name := HoldName(1791720000000000000, 7300, 4, "gate:unit")
	if want := "01791720000000000000-0000007300-000004-gate_unit"; name != want {
		t.Errorf("HoldName = %q, want %q", name, want)
	}
	if id, ok := ParseName(name); !ok || id != (ID{1791720000000000000, 7300, 4}) {
		t.Errorf("ParseName(hold name) = %+v, %v", id, ok)
	}
	// The cancel request's name is the ticket's id without display.
	id := ID{OrderKey: 5, PID: 6, Seq: 7}
	if got, want := id.String(), "00000000000000000005-0000000006-000007"; got != want {
		t.Errorf("ID.String = %q, want %q", got, want)
	}
	if got, ok := ParseID(id.String()); !ok || got != id {
		t.Errorf("ParseID(%q) = %+v, %v", id.String(), got, ok)
	}
}

func TestParseNameRejects(t *testing.T) {
	const id = "01791720000000000000-0000004711-000001"
	good := []string{id, id + "-x", id + "-" + strings.Repeat("a", 32), id + "-A.b_9"}
	for _, n := range good {
		if _, ok := ParseName(n); !ok {
			t.Errorf("ParseName(%q) rejected a valid name", n)
		}
	}
	bad := map[string]string{
		"empty":                "",
		"two fields":           "01791720000000000000-0000004711",
		"short order key":      "1791720000000000000-0000004711-000001",
		"long order key":       "001791720000000000000-0000004711-000001",
		"short pid":            "01791720000000000000-000004711-000001",
		"long pid":             "01791720000000000000-00000004711-000001",
		"short seq":            "01791720000000000000-0000004711-00001",
		"long seq":             "01791720000000000000-0000004711-0000001",
		"non digit":            "0179172000000000000x-0000004711-000001",
		"plus sign":            "+1791720000000000000-0000004711-000001",
		"minus sign":           "-1791720000000000000-0000004711-000001",
		"unicode digit":        "0179172000000000000\u0663-0000004711-000001",
		"order key overflow":   "99999999999999999999-0000004711-000001",
		"order key 2^64":       "18446744073709551616-0000004711-000001",
		"trailing dash":        id + "-",
		"empty display":        id + "--x",
		"display too long":     id + "-" + strings.Repeat("a", 33),
		"display has dash":     id + "-a-b",
		"display has space":    id + "-a b",
		"display has slash":    id + "-a/b",
		"display non ASCII":    id + "-caf\u00e9",
		"display control":      id + "-a\nb",
		"temporary file":       ".tmp-0000004711-000001",
		"directory traversal":  "../" + id,
		"leading space":        " " + id,
		"trailing newline":     id + "\n",
		"swapped separators":   "01791720000000000000_0000004711_000001",
		"hold-like with extra": id + "-x-000001",
	}
	for why, n := range bad {
		if got, ok := ParseName(n); ok {
			t.Errorf("ParseName(%q) accepted (%s): %+v", n, why, got)
		}
	}
	// ParseID is the cancel grammar: three fields and nothing after them.
	if _, ok := ParseID(id + "-x"); ok {
		t.Errorf("ParseID accepted a name with a display")
	}
	if _, ok := ParseID(id + "-"); ok {
		t.Errorf("ParseID accepted a trailing dash")
	}
}

func TestSeqWrapFormat(t *testing.T) {
	name := TicketName(ID{OrderKey: 1, PID: 1, Seq: 999999}, "")
	if want := "00000000000000000001-0000000001-999999"; name != want {
		t.Fatalf("seq 999999 formats as %q, want %q", name, want)
	}
	if got := NextSeq(999999); got != 0 {
		t.Errorf("NextSeq(999999) = %d, want 0", got)
	}
	if got := NextSeq(0); got != 1 {
		t.Errorf("NextSeq(0) = %d, want 1", got)
	}
	// The wrapped value is a six-digit name again, and it sorts before the unwrapped tail,
	// which is why a writer never reuses a name still on disk (PROTOCOL.md section 11).
	wrapped := TicketName(ID{OrderKey: 1, PID: 1, Seq: NextSeq(999999)}, "")
	if want := "00000000000000000001-0000000001-000000"; wrapped != want {
		t.Errorf("wrapped seq formats as %q, want %q", wrapped, want)
	}
}

func TestDisplaySanitizes(t *testing.T) {
	cases := []struct{ label, want string }{
		{"", ""},
		{"gate:unit", "gate_unit"},
		{"plain.Name_9", "plain.Name_9"},
		{"a b\tc\nd", "a_b_c_d"},
		{"a-b", "a_b"},
		{"../../etc", ".._.._etc"},
		{"\x1b[31mred\x1b[0m", "__31mred__0m"},
		{"caf\u00e9", "caf__"}, // two UTF-8 bytes, two underscores
		{strings.Repeat("x", 40), strings.Repeat("x", 32)},
		{strings.Repeat("\u00e9", 20), strings.Repeat("_", 32)},
	}
	for _, c := range cases {
		got := Display(c.label)
		if got != c.want {
			t.Errorf("Display(%q) = %q, want %q", c.label, got, c.want)
		}
		name := TicketName(ID{OrderKey: 1, PID: 2, Seq: 3}, c.label)
		if _, ok := ParseName(name); !ok {
			t.Errorf("TicketName with label %q produced an unparsable name %q", c.label, name)
		}
	}
}

func TestOrderKeyAging(t *testing.T) {
	const t0 = int64(1791720000000000000)
	if a, b := OrderKey(t0+60e9, 1), OrderKey(t0, 0); a != b {
		t.Errorf("OrderKey(t+60s, 1) = %d, OrderKey(t, 0) = %d; want equal", a, b)
	}
	if got, want := OrderKey(t0, 10), uint64(t0-600e9); got != want {
		t.Errorf("OrderKey(t, 10) = %d, want %d", got, want)
	}
	if got, want := OrderKey(t0, -1000), uint64(t0)+60_000_000_000_000; got != want {
		t.Errorf("OrderKey(t, -1000) = %d, want %d", got, want)
	}
	// Higher priority sorts first; equal keys tie by pid, then seq, through the name.
	if !(OrderKey(t0, 1) < OrderKey(t0, 0)) {
		t.Errorf("a higher priority must sort ahead")
	}
	// Worked example of PROTOCOL.md 21.1.
	if got := fmt.Sprintf("%020d", OrderKey(1791720000000000000, 0)); got != "01791720000000000000" {
		t.Errorf("21.1 order key = %s", got)
	}
	if got := fmt.Sprintf("%020d", OrderKey(1791719345000000000+600e9, 10)); got != "01791719345000000000" {
		t.Errorf("21.1 pid 5120 order key = %s", got)
	}
	// Saturation: below zero the key is 0 (PROTOCOL.md section 9), above the largest uint64 it is
	// that maximum, and the arithmetic never wraps, whatever the arguments.
	for _, c := range []struct {
		arrival  int64
		priority int
		want     uint64
	}{
		{0, 1000, 0},
		{59_999_999_999_999, 1000, 0},
		{60_000_000_000_000, 1000, 0},
		{60_000_000_000_001, 1000, 1},
		{-5, 0, 0},
		{math.MinInt64, math.MaxInt, 0},
		{math.MaxInt64, -1000, uint64(math.MaxInt64) + 60_000_000_000_000}, // int64 would wrap here
		{math.MaxInt64, -1_000_000_000, math.MaxUint64},
		{math.MaxInt64, math.MinInt, math.MaxUint64},
		{t0, 2000, uint64(t0) - 2000*60_000_000_000}, // no clamp of the priority: callers validate
	} {
		if got := OrderKey(c.arrival, c.priority); got != c.want {
			t.Errorf("OrderKey(%d, %d) = %d, want %d", c.arrival, c.priority, got, c.want)
		}
	}
	if !(ID{OrderKey: 5, PID: 9, Seq: 1}.String() < ID{OrderKey: 5, PID: 10, Seq: 0}.String()) {
		t.Errorf("name order must follow (order key, pid, seq)")
	}
}

func TestEscapeHuman(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain label: ok", "plain label: ok"},
		{"caf\u00e9 \u65e5\u672c \U0001F600", "caf\u00e9 \u65e5\u672c \U0001F600"},
		{"line1\nline2", `line1\nline2`},
		{"a\rb\tc", `a\rb\tc`},
		{"back\\slash", `back\\slash`},
		{"literal \\n is not a newline", `literal \\n is not a newline`},
		{"\x1b[31mred\x1b[0m", `\x1b[31mred\x1b[0m`},
		{"\x00\x01\x7f", `\x00\x01\x7f`},
		{"c1\u0085\u009b", `c1\u0085\u009b`},
		{"sep\u2028\u2029", `sep\u2028\u2029`},
		{"bidi\u202e\u2066", `bidi\u202e\u2066`},
		{"bad\xffbyte\xc3", `bad\xffbyte\xc3`},
		{"zwj \u200d kept", "zwj \u200d kept"},
	}
	for _, c := range cases {
		got := EscapeHuman(c.in)
		if got != c.want {
			t.Errorf("EscapeHuman(%q) = %q, want %q", c.in, got, c.want)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("EscapeHuman(%q) left control %q in %q", c.in, r, got)
			}
		}
	}
	// Injective: two different labels never escape to the same text.
	if EscapeHuman("a\nb") == EscapeHuman(`a\nb`) {
		t.Errorf("EscapeHuman is not injective")
	}
}

func TestDecodeStrict(t *testing.T) {
	const okTicket = `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`
	if _, err := DecodeTicket([]byte(okTicket)); err != nil {
		t.Fatalf("baseline ticket rejected: %v", err)
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	nine := `{"r1":[1,1],"r2":[1,1],"r3":[1,1],"r4":[1,1],"r5":[1,1],"r6":[1,1],"r7":[1,1],"r8":[1,1],"r9":[1,1]}`
	bad := map[string]string{
		"duplicate label":                 `{"label":"x","label":"y","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"duplicate priority":              `{"label":"x","priority":0,"priority":1,"need":{"cpu":[1,2]},"created_ns":5}`,
		"duplicate cpu inside need":       `{"label":"x","priority":0,"need":{"cpu":[1,2],"cpu":[3,4]},"created_ns":5}`,
		"duplicate by escaped spelling":   `{"label":"x","priority":0,"need":{"cpu":[1,2],"c\u0070u":[3,4]},"created_ns":5}`,
		"duplicate in an unknown subtree": `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5,"extra":{"a":{"b":1,"b":2}}}`,
		"duplicate in an unknown array":   `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5,"extra":[{"b":1,"b":2}]}`,
		"priority 1001":                   `{"label":"x","priority":1001,"need":{"cpu":[1,2]},"created_ns":5}`,
		"priority -1001":                  `{"label":"x","priority":-1001,"need":{"cpu":[1,2]},"created_ns":5}`,
		"priority 1.0":                    `{"label":"x","priority":1.0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"priority 1e3":                    `{"label":"x","priority":1e3,"need":{"cpu":[1,2]},"created_ns":5}`,
		"priority string":                 `{"label":"x","priority":"1","need":{"cpu":[1,2]},"created_ns":5}`,
		"priority null":                   `{"label":"x","priority":null,"need":{"cpu":[1,2]},"created_ns":5}`,
		"need min above max":              `{"label":"x","priority":0,"need":{"cpu":[5,4]},"created_ns":5}`,
		"need min zero":                   `{"label":"x","priority":0,"need":{"cpu":[0,4]},"created_ns":5}`,
		"need max 257":                    `{"label":"x","priority":0,"need":{"cpu":[1,257]},"created_ns":5}`,
		"need one element":                `{"label":"x","priority":0,"need":{"cpu":[1]},"created_ns":5}`,
		"need three elements":             `{"label":"x","priority":0,"need":{"cpu":[1,2,3]},"created_ns":5}`,
		"need string element":             `{"label":"x","priority":0,"need":{"cpu":["1",2]},"created_ns":5}`,
		"need float element":              `{"label":"x","priority":0,"need":{"cpu":[1,2.0]},"created_ns":5}`,
		"need not an array":               `{"label":"x","priority":0,"need":{"cpu":2},"created_ns":5}`,
		"nine resources":                  `{"label":"x","priority":0,"need":` + nine + `,"created_ns":5}`,
		"empty need":                      `{"label":"x","priority":0,"need":{},"created_ns":5}`,
		"need is an array":                `{"label":"x","priority":0,"need":[],"created_ns":5}`,
		"bad resource name":               `{"label":"x","priority":0,"need":{"Cpu":[1,2]},"created_ns":5}`,
		"created_ns negative":             `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":-1}`,
		"minimums sum to 513":             `{"label":"x","priority":0,"need":{"a":[256,256],"b":[256,256],"c":[1,1]},"created_ns":5}`,
		"lone high surrogate":             `{"label":"\ud800","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"lone low surrogate":              `{"label":"\udc00","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"low before high surrogate":       `{"label":"\ude00\ud83d","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"high surrogate then other":       `{"label":"\ud83d\u0041","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"high surrogate then text":        `{"label":"\ud83dx","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"surrogate in an unknown key":     `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5,"\ud800":1}`,
		"surrogate in an unknown value":   `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5,"e":[{"k":"\udfff"}]}`,
		"surrogate after an escaped pair": `{"label":"\ud83d\ude00\ud83d","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"missing label":                   `{"priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"missing priority":                `{"label":"x","need":{"cpu":[1,2]},"created_ns":5}`,
		"missing need":                    `{"label":"x","priority":0,"created_ns":5}`,
		"missing created_ns":              `{"label":"x","priority":0,"need":{"cpu":[1,2]}}`,
		"label null":                      `{"label":null,"priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"label number":                    `{"label":1,"priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"label 129 bytes":                 `{"label":"` + long(129) + `","priority":0,"need":{"cpu":[1,2]},"created_ns":5}`,
		"label invalid UTF-8":             "{\"label\":\"\xff\",\"priority\":0,\"need\":{\"cpu\":[1,2]},\"created_ns\":5}",
		"created_ns float":                `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5.0}`,
		"created_ns exponent":             `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5e0}`,
		"created_ns beyond int64":         `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":9223372036854775808}`,
		"not an object":                   `[]`,
		"null":                            `null`,
		"empty body":                      ``,
		"whitespace only":                 ` `,
		"trailing garbage":                okTicket + ` x`,
		"two objects":                     okTicket + okTicket,
		"truncated":                       okTicket[:len(okTicket)-1],
		"trailing comma":                  `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5,}`,
		"byte order mark":                 "\ufeff" + okTicket,
		"body of 8193 bytes":              okTicket[:len(okTicket)-1] + `,"pad":"` + long(8193-len(okTicket)-9) + `"}`,
	}
	if got := len(bad["body of 8193 bytes"]); got != 8193 {
		t.Fatalf("test body is %d bytes, want 8193", got)
	}
	for why, body := range bad {
		if _, err := DecodeTicket([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeTicket accepted or mis-reported (%s): err=%v", why, err)
		}
	}

	// Hold: duplicate resource inside grant, and the grant rules.
	const okHold = `{"label":"x","grant":{"cpu":[0,1]},"start_ns":5,"unbudgeted":false,"unbudgeted_resources":[],"clamped":{},"adopted":false}`
	if _, err := DecodeHold([]byte(okHold)); err != nil {
		t.Fatalf("baseline hold rejected: %v", err)
	}
	holdWith := func(key, val string) string {
		m := map[string]string{
			"label": `"x"`, "grant": `{"cpu":[0,1]}`, "start_ns": `5`, "unbudgeted": `false`,
			"unbudgeted_resources": `[]`, "clamped": `{}`, "adopted": `false`,
		}
		m[key] = val
		var parts []string
		for _, k := range []string{"label", "grant", "start_ns", "unbudgeted", "unbudgeted_resources", "clamped", "adopted"} {
			if m[k] != "" {
				parts = append(parts, strconv.Quote(k)+":"+m[k])
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	badHold := map[string]string{
		"duplicate resource in grant":   holdWith("grant", `{"cpu":[0],"cpu":[1]}`),
		"duplicate resource in clamped": holdWith("clamped", `{"cpu":4,"cpu":5}`),
		"duplicate top-level key":       strings.Replace(okHold, `"adopted":false`, `"adopted":false,"adopted":true`, 1),
		"grant unsorted":                holdWith("grant", `{"cpu":[1,0]}`),
		"grant repeated index":          holdWith("grant", `{"cpu":[1,1]}`),
		"grant empty array":             holdWith("grant", `{"cpu":[]}`),
		"grant empty beside a token":    holdWith("grant", `{"cpu":[0],"gpu":[]}`),
		"clamped zero":                  holdWith("clamped", `{"cpu":0}`),
		"clamped 257":                   holdWith("clamped", `{"cpu":257}`),
		"clamped negative":              holdWith("clamped", `{"cpu":-1}`),
		"clamped bad name":              holdWith("clamped", `{"Bad":4}`),
		"label lone high surrogate":     holdWith("label", `"\ud800"`),
		"label lone low surrogate":      holdWith("label", `"\udc00"`),
		"surrogate in a grant key":      holdWith("grant", `{"\ud800":[0]}`),
		"grant index 4096":              holdWith("grant", `{"cpu":[4096]}`),
		"grant index -1":                holdWith("grant", `{"cpu":[-1]}`),
		"grant index float":             holdWith("grant", `{"cpu":[1.0]}`),
		"grant 257 in one resource":     holdWith("grant", `{"cpu":[`+seqList(0, 257)+`]}`),
		"grant 513 in total":            holdWith("grant", `{"cpu":[`+seqList(0, 256)+`],"gpu":[`+seqList(0, 256)+`],"io":[0]}`),
		"grant not an object":           holdWith("grant", `[]`),
		"grant value not an array":      holdWith("grant", `{"cpu":3}`),
		"grant bad name":                holdWith("grant", `{"CPU":[1]}`),
		"start_ns negative":             holdWith("start_ns", `-1`),
		"start_ns float":                holdWith("start_ns", `5.5`),
		"start_ns beyond int64":         holdWith("start_ns", `9223372036854775808`),
		"unbudgeted number":             holdWith("unbudgeted", `1`),
		"unbudgeted null":               holdWith("unbudgeted", `null`),
		"unbudgeted_resources object":   holdWith("unbudgeted_resources", `{}`),
		"unbudgeted_resources number":   holdWith("unbudgeted_resources", `[1]`),
		"unbudgeted_resources name":     holdWith("unbudgeted_resources", `["Bad Name"]`),
		"clamped string":                holdWith("clamped", `{"cpu":"4"}`),
		"clamped float":                 holdWith("clamped", `{"cpu":4.0}`),
		"adopted string":                holdWith("adopted", `"true"`),
		"missing label":                 holdWith("label", ``),
		"missing grant":                 holdWith("grant", ``),
		"missing start_ns":              holdWith("start_ns", ``),
		"missing unbudgeted":            holdWith("unbudgeted", ``),
		"missing unbudgeted_resources":  holdWith("unbudgeted_resources", ``),
		"missing clamped":               holdWith("clamped", ``),
		"missing adopted":               holdWith("adopted", ``),
	}
	for why, body := range badHold {
		if _, err := DecodeHold([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeHold accepted or mis-reported (%s): err=%v", why, err)
		}
	}

	// Cancel.
	const okCancel = `{"requested_ns":5,"requester_pid":4711}`
	if _, err := DecodeCancel([]byte(okCancel)); err != nil {
		t.Fatalf("baseline cancel rejected: %v", err)
	}
	for why, body := range map[string]string{
		"duplicate requested_ns": `{"requested_ns":5,"requested_ns":6,"requester_pid":4711}`,
		"missing pid":            `{"requested_ns":5}`,
		"missing requested_ns":   `{"requester_pid":4711}`,
		"pid string":             `{"requested_ns":5,"requester_pid":"4711"}`,
		"requested_ns float":     `{"requested_ns":5.0,"requester_pid":4711}`,
		"requested_ns negative":  `{"requested_ns":-1,"requester_pid":4711}`,
		"requester_pid negative": `{"requested_ns":5,"requester_pid":-1}`,
		"surrogate in a key":     `{"requested_ns":5,"requester_pid":4711,"\ud800":1}`,
		"not an object":          `"x"`,
	} {
		if _, err := DecodeCancel([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeCancel accepted or mis-reported (%s): err=%v", why, err)
		}
	}

	// Unknown keys are ignored at any depth, whitespace around the object is allowed.
	good := ` {"label":"x","priority":-1000,"need":{"cpu":[1,2],"gpu":[3,256]},"created_ns":5,` +
		`"future":{"a":[1,{"b":null}],"c":1.5},"also":"x"}` + "\n\t "
	tk, err := DecodeTicket([]byte(good))
	if err != nil {
		t.Fatalf("ticket with unknown keys rejected: %v", err)
	}
	want := Ticket{Label: "x", Priority: -1000, CreatedNS: 5,
		Need: map[string]spec.Range{"cpu": {Min: 1, Max: 2}, "gpu": {Min: 3, Max: 256}}}
	if !reflect.DeepEqual(tk, want) {
		t.Errorf("decoded ticket = %+v, want %+v", tk, want)
	}
	h, err := DecodeHold([]byte(strings.Replace(okHold, `{"label"`, `{"future":{"k":[1]},"label"`, 1)))
	if err != nil {
		t.Fatalf("hold with an unknown key rejected: %v", err)
	}
	wantHold := Hold{Label: "x", Grant: map[string][]int{"cpu": {0, 1}}, StartNS: 5,
		UnbudgetedResources: []string{}, Clamped: map[string]int{}}
	if !reflect.DeepEqual(h, wantHold) {
		t.Errorf("decoded hold = %+v, want %+v", h, wantHold)
	}
	// An escaped backslash before "ud800" is the text, not an escape; pairs in either case are fine.
	for body, want := range map[string]string{
		`{"label":"\\ud800","priority":0,"need":{"cpu":[1,2]},"created_ns":0}`:             `\ud800`,
		`{"label":"\uD83D\uDE00","priority":0,"need":{"cpu":[1,2]},"created_ns":0}`:        "\U0001F600",
		`{"label":"\ud83d\ude00\\ud800","priority":0,"need":{"cpu":[1,2]},"created_ns":0}`: "\U0001F600\\ud800",
	} {
		got, err := DecodeTicket([]byte(body))
		if err != nil || got.Label != want {
			t.Errorf("DecodeTicket(%s) = %q, %v; want label %q", body, got.Label, err, want)
		}
	}
	// Ticket at the limits: the body is exactly 8192 bytes.
	pad := `{"label":"x","priority":0,"need":{"cpu":[1,2]},"created_ns":5,"pad":"`
	body := pad + strings.Repeat("a", 8192-len(pad)-2) + `"}`
	if len(body) != 8192 {
		t.Fatalf("limit body is %d bytes", len(body))
	}
	if _, err := DecodeTicket([]byte(body)); err != nil {
		t.Errorf("an 8192-byte body must be accepted: %v", err)
	}
}

func seqList(from, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = strconv.Itoa(from + i)
	}
	return strings.Join(parts, ",")
}

func TestEncodeShapeAndRefusals(t *testing.T) {
	// Every key is written, nil collections become empty ones, keys are in a fixed order.
	b, err := Encode(Ticket{Label: "gate:unit", Priority: 0, Need: map[string]spec.Range{"cpu": {Min: 4, Max: 12}}, CreatedNS: 1791720000000000000})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"label":"gate:unit","priority":0,"need":{"cpu":[4,12]},"created_ns":1791720000000000000}`; string(b) != want {
		t.Errorf("ticket body = %s, want %s", b, want)
	}
	b, err = Encode(&Hold{Label: "", StartNS: 7})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"label":"","grant":{},"start_ns":7,"unbudgeted":false,"unbudgeted_resources":[],"clamped":{},"adopted":false}`; string(b) != want {
		t.Errorf("hold body = %s, want %s", b, want)
	}
	b, err = Encode(Cancel{RequestedNS: 9, RequesterPID: 4711})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"requested_ns":9,"requester_pid":4711}`; string(b) != want {
		t.Errorf("cancel body = %s, want %s", b, want)
	}
	// A label with HTML characters and a line separator stays decodable.
	tk := Ticket{Label: "<a&b>\u2028", Need: map[string]spec.Range{"cpu": {Min: 1, Max: 1}}}
	b, err = Encode(tk)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := DecodeTicket(b); err != nil || !reflect.DeepEqual(back, tk) {
		t.Errorf("label round trip = %+v, %v", back, err)
	}

	cpu := map[string]spec.Range{"cpu": {Min: 1, Max: 1}}
	refused := map[string]any{
		"unsupported type":    42,
		"nil ticket pointer":  (*Ticket)(nil),
		"nil hold pointer":    (*Hold)(nil),
		"nil cancel pointer":  (*Cancel)(nil),
		"ticket without need": Ticket{},
		"ticket priority":     Ticket{Priority: 1001, Need: cpu},
		"ticket range":        Ticket{Need: map[string]spec.Range{"cpu": {Min: 3, Max: 2}}},
		"ticket label length": Ticket{Label: strings.Repeat("a", 129), Need: cpu},
		"ticket label UTF-8":  Ticket{Label: "\xff", Need: cpu},
		"hold label UTF-8":    Hold{Label: "\xff"},
		"hold unsorted":       Hold{Grant: map[string][]int{"cpu": {2, 1}}},
		"hold repeated":       Hold{Grant: map[string][]int{"cpu": {1, 1}}},
		"hold index":          Hold{Grant: map[string][]int{"cpu": {4096}}},
		"hold too many":       Hold{Grant: map[string][]int{"cpu": make257()}},
		"hold start":          Hold{StartNS: math.MaxInt64 + 1},
		"hold name":           Hold{Clamped: map[string]int{"BAD": 3}},
		"hold clamped zero":   Hold{Clamped: map[string]int{"cpu": 0}},
		"hold clamped 257":    Hold{Clamped: map[string]int{"cpu": 257}},
		"hold nil tokens":     Hold{Grant: map[string][]int{"cpu": nil}},
		"hold empty tokens":   Hold{Grant: map[string][]int{"cpu": {}}},
		"ticket created":      Ticket{Need: cpu, CreatedNS: -1},
		"ticket minimums":     Ticket{Need: map[string]spec.Range{"a": {Min: 256, Max: 256}, "b": {Min: 256, Max: 256}, "c": {Min: 1, Max: 1}}},
		"cancel requested":    Cancel{RequestedNS: -1},
		"cancel pid":          Cancel{RequesterPID: -1},
		"cancel pointer":      &Cancel{RequesterPID: -1},
	}
	for why, v := range refused {
		if b, err := Encode(v); err == nil {
			t.Errorf("Encode accepted (%s): %s", why, b)
		}
	}
}

func make257() []int {
	s := make([]int, 257)
	for i := range s {
		s[i] = i
	}
	return s
}

func TestLargestGrantFitsBody(t *testing.T) {
	res := make([]string, 8)
	for i := range res {
		res[i] = strings.Repeat(string(rune('a'+i)), 31) + "-" // 32 bytes, the longest name
	}
	// 512 tokens over 8 resources: 64 each, the highest indices (4032..4095).
	grant := make(map[string][]int, 8)
	for _, r := range res {
		idx := make([]int, 64)
		for i := range idx {
			idx[i] = 4032 + i
		}
		grant[r] = idx
	}
	// A 128-byte label of control characters: each takes six bytes as \u00XX.
	var label strings.Builder
	for label.Len() < 128 {
		for c := byte(0); c < 0x20 && label.Len() < 128; c++ {
			if c != '\b' && c != '\t' && c != '\n' && c != '\f' && c != '\r' { // those have two-byte escapes
				label.WriteByte(c)
			}
		}
	}
	lbl := label.String()
	if len(lbl) != 128 {
		t.Fatalf("label is %d bytes", len(lbl))
	}
	clamped := make(map[string]int, 8)
	for _, r := range res {
		clamped[r] = 256
	}
	h := Hold{Label: lbl, Grant: grant, StartNS: math.MaxInt64, Unbudgeted: true,
		UnbudgetedResources: res, Clamped: clamped, Adopted: true}
	b, err := Encode(h)
	if err != nil {
		t.Fatalf("largest hold refused: %v", err)
	}
	if len(b) > MaxBodyBytes {
		t.Fatalf("largest hold is %d bytes, over %d", len(b), MaxBodyBytes)
	}
	t.Logf("largest hold body: %d bytes", len(b))
	back, err := DecodeHold(b)
	if err != nil {
		t.Fatalf("largest hold does not decode: %v", err)
	}
	if !reflect.DeepEqual(back, h) {
		t.Errorf("largest hold does not round trip")
	}
	need := make(map[string]spec.Range, 8)
	for _, r := range res {
		need[r] = spec.Range{Min: 64, Max: 256} // the minimums sum to the 512 cap
	}
	tk := Ticket{Label: lbl, Priority: -1000, Need: need, CreatedNS: math.MaxInt64}
	b, err = Encode(tk)
	if err != nil || len(b) > MaxBodyBytes {
		t.Fatalf("largest ticket: %d bytes, %v", len(b), err)
	}
	if back, err := DecodeTicket(b); err != nil || !reflect.DeepEqual(back, tk) {
		t.Errorf("largest ticket does not round trip: %v", err)
	}
}

func TestEncodeCanonicalAndStable(t *testing.T) {
	h := Hold{Label: "x", Grant: map[string][]int{"gpu": {0}, "cpu": {1, 2}}, StartNS: 3,
		UnbudgetedResources: []string{"zz", "aa"}, Clamped: map[string]int{"cpu": 16}}
	a, err := Encode(h)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		b, _ := Encode(h)
		if !bytes.Equal(a, b) {
			t.Fatalf("Encode is not deterministic: %s vs %s", a, b)
		}
	}
	if want := `{"label":"x","grant":{"cpu":[1,2],"gpu":[0]},"start_ns":3,"unbudgeted":false,"unbudgeted_resources":["zz","aa"],"clamped":{"cpu":16},"adopted":false}`; string(a) != want {
		t.Errorf("hold body = %s\nwant        %s", a, want)
	}
}

// The fixtures under testdata are shared with the independent Python client. This test is the
// Go side of the same contract: every valid file decodes to its expectation, every bad file
// is malformed, every name is accepted or refused as stated.

type nameCase struct {
	Name     string  `json:"name"`
	OrderKey string  `json:"order_key"`
	PID      int     `json:"pid"`
	Seq      int     `json:"seq"`
	Display  *string `json:"display"`
	Why      string  `json:"why"`
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestFixtureNames(t *testing.T) {
	var f struct {
		Valid         []nameCase `json:"valid"`
		Invalid       []nameCase `json:"invalid"`
		CancelValid   []nameCase `json:"cancel_valid"`
		CancelInvalid []nameCase `json:"cancel_invalid"`
	}
	readJSON(t, "testdata/names.json", &f)
	if len(f.Valid) == 0 || len(f.Invalid) == 0 || len(f.CancelValid) == 0 || len(f.CancelInvalid) == 0 {
		t.Fatalf("names.json lacks a section")
	}
	for _, c := range f.Valid {
		id, ok := ParseName(c.Name)
		key, err := strconv.ParseUint(c.OrderKey, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || id != (ID{OrderKey: key, PID: c.PID, Seq: c.Seq}) {
			t.Errorf("ParseName(%q) = %+v, %v", c.Name, id, ok)
		}
		want := id.String()
		if c.Display != nil {
			want += "-" + *c.Display
		}
		if c.Name != want {
			t.Errorf("fixture %q is not the canonical spelling %q", c.Name, want)
		}
	}
	for _, c := range f.Invalid {
		if id, ok := ParseName(c.Name); ok {
			t.Errorf("ParseName accepted %q (%s): %+v", c.Name, c.Why, id)
		}
	}
	for _, c := range f.CancelValid {
		id, ok := ParseID(c.Name)
		key, err := strconv.ParseUint(c.OrderKey, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || id != (ID{OrderKey: key, PID: c.PID, Seq: c.Seq}) || id.String() != c.Name {
			t.Errorf("ParseID(%q) = %+v, %v", c.Name, id, ok)
		}
		if c.Display != nil {
			t.Errorf("cancel case %q carries a display", c.Name)
		}
	}
	for _, c := range f.CancelInvalid {
		if _, ok := ParseID(c.Name); ok {
			t.Errorf("ParseID accepted %q (%s)", c.Name, c.Why)
		}
	}
}

func TestFixtureDisplayAndOrderKeys(t *testing.T) {
	var disp []struct{ Label, Display string }
	readJSON(t, "testdata/display.json", &disp)
	for _, c := range disp {
		if got := Display(c.Label); got != c.Display {
			t.Errorf("Display(%q) = %q, fixture says %q", c.Label, got, c.Display)
		}
	}
	var keys []struct {
		ArrivalNS int64  `json:"arrival_ns"`
		Priority  int    `json:"priority"`
		OrderKey  string `json:"order_key"`
	}
	readJSON(t, "testdata/order_keys.json", &keys)
	for _, c := range keys {
		if got := fmt.Sprintf("%020d", OrderKey(c.ArrivalNS, c.Priority)); got != c.OrderKey {
			t.Errorf("OrderKey(%d, %d) = %s, fixture says %s", c.ArrivalNS, c.Priority, got, c.OrderKey)
		}
	}
}

func TestFixtureBodies(t *testing.T) {
	type codec struct {
		decode func([]byte) (any, error)
	}
	codecs := map[string]codec{
		"ticket": {func(b []byte) (any, error) { return DecodeTicket(b) }},
		"hold":   {func(b []byte) (any, error) { return DecodeHold(b) }},
		"cancel": {func(b []byte) (any, error) { return DecodeCancel(b) }},
	}
	seen := map[string][2]int{}
	for kind, c := range codecs {
		dir := filepath.Join("testdata", "bodies", kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".body") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			stem := strings.TrimSuffix(name, ".body")
			got, derr := c.decode(body)
			n := seen[kind]
			switch {
			case strings.HasPrefix(stem, "bad-"):
				n[1]++
				if !errors.Is(derr, ErrMalformed) {
					t.Errorf("%s/%s must be malformed, got %v", kind, name, derr)
				}
				if _, err := os.Stat(filepath.Join(dir, stem+".why")); err != nil {
					t.Errorf("%s/%s has no .why file", kind, name)
				}
			case strings.HasPrefix(stem, "ok-"):
				n[0]++
				if derr != nil {
					t.Errorf("%s/%s must decode: %v", kind, name, derr)
					break
				}
				expect, err := os.ReadFile(filepath.Join(dir, stem+".expect.json"))
				if err != nil {
					t.Fatal(err)
				}
				enc, err := Encode(got)
				if err != nil {
					t.Errorf("%s/%s does not re-encode: %v", kind, name, err)
					break
				}
				var a, b any
				if err := json.Unmarshal(enc, &a); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(expect, &b); err != nil {
					t.Fatalf("%s expectation: %v", stem, err)
				}
				if !reflect.DeepEqual(a, b) {
					t.Errorf("%s/%s decodes to %s, expectation is %s", kind, name, enc, bytes.TrimSpace(expect))
				}
				// The expectation is itself a valid body and decodes to the same value.
				if again, err := c.decode(expect); err != nil || !reflect.DeepEqual(again, got) {
					t.Errorf("%s/%s: expectation does not decode to the same value: %v", kind, name, err)
				}
			default:
				t.Errorf("%s/%s: file names start with ok- or bad-", kind, name)
			}
			seen[kind] = n
		}
		if n := seen[kind]; n[0] < 3 || n[1] < 5 {
			t.Errorf("%s: only %d valid and %d malformed fixtures", kind, n[0], n[1])
		}
	}
}

// The fixture of the largest possible hold is what Encode writes for the same hold.
func TestLargestFixtureMatchesEncode(t *testing.T) {
	body, err := os.ReadFile("testdata/bodies/hold/ok-largest-possible.body")
	if err != nil {
		t.Fatal(err)
	}
	h, err := DecodeHold(body)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := Encode(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) > MaxBodyBytes {
		t.Errorf("largest hold encodes to %d bytes", len(enc))
	}
	var tot, res int
	for _, g := range h.Grant {
		tot += len(g)
		res++
	}
	if tot != spec.MaxPerGrant || res != spec.MaxResources || len(h.Label) != spec.MaxLabelBytes {
		t.Errorf("fixture is not the largest hold: %d tokens, %d resources, %d label bytes", tot, res, len(h.Label))
	}
}
