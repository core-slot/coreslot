package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/spec"
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

// The order key is the arrival, 0 before the epoch, and the name orders by (key, pid, seq).
func TestOrderKey(t *testing.T) {
	for arrival, want := range map[int64]uint64{1791720000000000000: 1791720000000000000, 0: 0, -5: 0,
		math.MinInt64: 0, math.MaxInt64: math.MaxInt64} {
		if got := OrderKey(arrival); got != want {
			t.Errorf("OrderKey(%d) = %d, want %d", arrival, got, want)
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
	const okTicket = `{"label":"x","need_mib":128,"created_ns":5}`
	if _, err := DecodeTicket([]byte(okTicket)); err != nil {
		t.Fatalf("baseline ticket rejected: %v", err)
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	bad := map[string]string{
		"duplicate label":                 `{"label":"x","label":"y","need_mib":128,"created_ns":5}`,
		"duplicate need_mib":              `{"label":"x","need_mib":128,"need_mib":256,"created_ns":5}`,
		"duplicate by escaped spelling":   `{"label":"x","need_mib":128,"need\u005fmib":256,"created_ns":5}`,
		"duplicate in an unknown subtree": `{"label":"x","need_mib":128,"created_ns":5,"extra":{"a":{"b":1,"b":2}}}`,
		"duplicate in an unknown array":   `{"label":"x","need_mib":128,"created_ns":5,"extra":[{"b":1,"b":2}]}`,
		"need_mib zero":                   `{"label":"x","need_mib":0,"created_ns":5}`,
		"need_mib negative":               `{"label":"x","need_mib":-1,"created_ns":5}`,
		"need_mib above the maximum":      `{"label":"x","need_mib":1073741825,"created_ns":5}`,
		"need_mib float":                  `{"label":"x","need_mib":128.0,"created_ns":5}`,
		"need_mib string":                 `{"label":"x","need_mib":"128","created_ns":5}`,
		"need_mib object":                 `{"label":"x","need_mib":{"mem":[1,2]},"created_ns":5}`,
		"created_ns negative":             `{"label":"x","need_mib":128,"created_ns":-1}`,
		"lone high surrogate":             `{"label":"\ud800","need_mib":128,"created_ns":5}`,
		"lone low surrogate":              `{"label":"\udc00","need_mib":128,"created_ns":5}`,
		"low before high surrogate":       `{"label":"\ude00\ud83d","need_mib":128,"created_ns":5}`,
		"high surrogate then other":       `{"label":"\ud83d\u0041","need_mib":128,"created_ns":5}`,
		"high surrogate then text":        `{"label":"\ud83dx","need_mib":128,"created_ns":5}`,
		"surrogate in an unknown key":     `{"label":"x","need_mib":128,"created_ns":5,"\ud800":1}`,
		"surrogate in an unknown value":   `{"label":"x","need_mib":128,"created_ns":5,"e":[{"k":"\udfff"}]}`,
		"surrogate after an escaped pair": `{"label":"\ud83d\ude00\ud83d","need_mib":128,"created_ns":5}`,
		"missing label":                   `{"need_mib":128,"created_ns":5}`,
		"missing need_mib":                `{"label":"x","created_ns":5}`,
		"missing created_ns":              `{"label":"x","need_mib":128}`,
		"label null":                      `{"label":null,"need_mib":128,"created_ns":5}`,
		"label number":                    `{"label":1,"need_mib":128,"created_ns":5}`,
		"label 129 bytes":                 `{"label":"` + long(129) + `","need_mib":128,"created_ns":5}`,
		"label invalid UTF-8":             "{\"label\":\"\xff\",\"need_mib\":128,\"created_ns\":5}",
		"created_ns float":                `{"label":"x","need_mib":128,"created_ns":5.0}`,
		"created_ns exponent":             `{"label":"x","need_mib":128,"created_ns":5e0}`,
		"created_ns beyond int64":         `{"label":"x","need_mib":128,"created_ns":9223372036854775808}`,
		"not an object":                   `[]`,
		"null":                            `null`,
		"empty body":                      ``,
		"whitespace only":                 ` `,
		"trailing garbage":                okTicket + ` x`,
		"two objects":                     okTicket + okTicket,
		"truncated":                       okTicket[:len(okTicket)-1],
		"trailing comma":                  `{"label":"x","need_mib":128,"created_ns":5,}`,
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

	// Hold: every key is required, each within its rule.
	holdKeys := []string{"label", "pid", "start_ns", "arrival_ns", "need_mib", "stops", "footprint_mib",
		"d_user_ns", "d_sys_ns", "d_page_wait_ns", "normal_share", "sampled_ns", "victim", "learned", "child_exited"}
	holdWith := func(key, val string) string {
		m := map[string]string{
			"label": `"x"`, "pid": `4711`, "start_ns": `5`, "arrival_ns": `4`, "need_mib": `2048`, "stops": `0`,
			"footprint_mib": `512`, "d_user_ns": `1`, "d_sys_ns": `2`, "d_page_wait_ns": `3`, "normal_share": `0.04`,
			"sampled_ns": `6`, "victim": `false`, "learned": `true`, "child_exited": `false`,
		}
		m[key] = val
		var parts []string
		for _, k := range holdKeys {
			if m[k] != "" {
				parts = append(parts, strconv.Quote(k)+":"+m[k])
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	okHold := holdWith("label", `"x"`)
	if _, err := DecodeHold([]byte(okHold)); err != nil {
		t.Fatalf("baseline hold rejected: %v", err)
	}
	badHold := map[string]string{
		"duplicate top-level key":   strings.Replace(okHold, `"child_exited":false`, `"child_exited":false,"child_exited":false`, 1),
		"label lone high surrogate": holdWith("label", `"\ud800"`),
		"label lone low surrogate":  holdWith("label", `"\udc00"`),
		"label 129 bytes":           holdWith("label", `"`+long(129)+`"`),
		"pid negative":              holdWith("pid", `-1`),
		"pid string":                holdWith("pid", `"4711"`),
		"start_ns negative":         holdWith("start_ns", `-1`),
		"start_ns float":            holdWith("start_ns", `5.5`),
		"start_ns beyond int64":     holdWith("start_ns", `9223372036854775808`),
		"arrival_ns negative":       holdWith("arrival_ns", `-1`),
		"need_mib negative":         holdWith("need_mib", `-1`),
		"need_mib above maximum":    holdWith("need_mib", `1073741825`),
		"need_mib object":           holdWith("need_mib", `{"mem":[0,1]}`),
		"footprint_mib negative":    holdWith("footprint_mib", `-1`),
		"footprint_mib above max":   holdWith("footprint_mib", `1073741825`),
		"stops negative":            holdWith("stops", `-1`),
		"stops float":               holdWith("stops", `1.5`),
		"d_user_ns negative":        holdWith("d_user_ns", `-1`),
		"d_sys_ns negative":         holdWith("d_sys_ns", `-1`),
		"d_page_wait_ns negative":   holdWith("d_page_wait_ns", `-1`),
		"sampled_ns negative":       holdWith("sampled_ns", `-1`),
		"normal_share negative":     holdWith("normal_share", `-0.1`),
		"normal_share above 1":      holdWith("normal_share", `1.5`),
		"normal_share string":       holdWith("normal_share", `"0.04"`),
		"normal_share out of range": holdWith("normal_share", `1e999`),
		"learned number":            holdWith("learned", `1`),
		"victim number":             holdWith("victim", `1`),
		"child_exited null":         holdWith("child_exited", `null`),
	}
	for _, k := range holdKeys {
		badHold["missing "+k] = holdWith(k, ``)
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
	good := ` {"label":"x","need_mib":40960,"created_ns":5,` +
		`"future":{"a":[1,{"b":null}],"c":1.5},"need":{"mem":[1,2]}}` + "\n\t "
	tk, err := DecodeTicket([]byte(good))
	if err != nil {
		t.Fatalf("ticket with unknown keys rejected: %v", err)
	}
	if want := (Ticket{Label: "x", NeedMiB: 40960, CreatedNS: 5}); tk != want {
		t.Errorf("decoded ticket = %+v, want %+v", tk, want)
	}
	h, err := DecodeHold([]byte(strings.Replace(okHold, `{"label"`, `{"future":{"k":[1]},"label"`, 1)))
	if err != nil {
		t.Fatalf("hold with an unknown key rejected: %v", err)
	}
	wantHold := Hold{Label: "x", PID: 4711, StartNS: 5, ArrivalNS: 4, NeedMiB: 2048, FootprintMiB: 512,
		DUserNS: 1, DSysNS: 2, DPageWaitNS: 3, NormalShare: 0.04, SampledNS: 6, Learned: true}
	if h != wantHold {
		t.Errorf("decoded hold = %+v, want %+v", h, wantHold)
	}
	// An escaped backslash before "ud800" is the text, not an escape; pairs in either case are fine.
	for body, want := range map[string]string{
		`{"label":"\\ud800","need_mib":128,"created_ns":0}`:             `\ud800`,
		`{"label":"\uD83D\uDE00","need_mib":128,"created_ns":0}`:        "\U0001F600",
		`{"label":"\ud83d\ude00\\ud800","need_mib":128,"created_ns":0}`: "\U0001F600\\ud800",
	} {
		got, err := DecodeTicket([]byte(body))
		if err != nil || got.Label != want {
			t.Errorf("DecodeTicket(%s) = %q, %v; want label %q", body, got.Label, err, want)
		}
	}
	// Ticket at the limits: the body is exactly 8192 bytes.
	pad := `{"label":"x","need_mib":128,"created_ns":5,"pad":"`
	body := pad + strings.Repeat("a", 8192-len(pad)-2) + `"}`
	if len(body) != 8192 {
		t.Fatalf("limit body is %d bytes", len(body))
	}
	if _, err := DecodeTicket([]byte(body)); err != nil {
		t.Errorf("an 8192-byte body must be accepted: %v", err)
	}
}

func TestEncodeShapeAndRefusals(t *testing.T) {
	// Every key is written, keys are in a fixed order.
	b, err := Encode(Ticket{Label: "gate:unit", NeedMiB: 3072, CreatedNS: 1791720000000000000})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"label":"gate:unit","need_mib":3072,"created_ns":1791720000000000000}`; string(b) != want {
		t.Errorf("ticket body = %s, want %s", b, want)
	}
	b, err = Encode(&Hold{Label: "", StartNS: 7})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"label":"","pid":0,"start_ns":7,"arrival_ns":0,"need_mib":0,"stops":0,"footprint_mib":0,` +
		`"d_user_ns":0,"d_sys_ns":0,"d_page_wait_ns":0,"normal_share":0,"sampled_ns":0,"victim":false,"learned":false,"child_exited":false}`; string(b) != want {
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
	tk := Ticket{Label: "<a&b> ", NeedMiB: 128}
	b, err = Encode(tk)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := DecodeTicket(b); err != nil || back != tk {
		t.Errorf("label round trip = %+v, %v", back, err)
	}

	refused := map[string]any{
		"unsupported type":      42,
		"nil ticket pointer":    (*Ticket)(nil),
		"nil hold pointer":      (*Hold)(nil),
		"nil cancel pointer":    (*Cancel)(nil),
		"ticket without need":   Ticket{},
		"ticket need too large": Ticket{NeedMiB: spec.MaxNeedMiB + 1},
		"ticket label length":   Ticket{Label: strings.Repeat("a", 129), NeedMiB: 128},
		"ticket label UTF-8":    Ticket{Label: "\xff", NeedMiB: 128},
		"ticket created":        Ticket{NeedMiB: 128, CreatedNS: -1},
		"hold label UTF-8":      Hold{Label: "\xff"},
		"hold start":            Hold{StartNS: -1},
		"hold pid":              Hold{PID: -1},
		"hold need negative":    Hold{NeedMiB: -1},
		"hold need too large":   Hold{NeedMiB: spec.MaxNeedMiB + 1},
		"hold footprint":        Hold{FootprintMiB: -1},
		"hold stops":            Hold{Stops: -1},
		"hold page wait":        Hold{DPageWaitNS: -1},
		"cancel requested":      Cancel{RequestedNS: -1},
		"cancel pid":            Cancel{RequesterPID: -1},
		"cancel pointer":        &Cancel{RequesterPID: -1},
	}
	for why, v := range refused {
		if b, err := Encode(v); err == nil {
			t.Errorf("Encode accepted (%s): %s", why, b)
		}
	}
}

// largestLabel is a 128-byte label of control characters: each takes six bytes as \u00XX.
func largestLabel(t *testing.T) string {
	var label strings.Builder
	for label.Len() < 128 {
		for c := byte(0); c < 0x20 && label.Len() < 128; c++ {
			if c != '\b' && c != '\t' && c != '\n' && c != '\f' && c != '\r' { // those have two-byte escapes
				label.WriteByte(c)
			}
		}
	}
	if label.Len() != 128 {
		t.Fatalf("label is %d bytes", label.Len())
	}
	return label.String()
}

func TestLargestBodiesFit(t *testing.T) {
	lbl := largestLabel(t)
	h := Hold{Label: lbl, PID: math.MaxInt, StartNS: math.MaxInt64, ArrivalNS: math.MaxInt64, NeedMiB: spec.MaxNeedMiB,
		TypicalS: math.MaxInt64, Stops: math.MaxInt, FootprintMiB: spec.MaxNeedMiB, DUserNS: math.MaxInt64, DSysNS: math.MaxInt64,
		DPageWaitNS: math.MaxInt64, NormalShare: 1, SampledNS: math.MaxInt64, Victim: true, Learned: true, ChildExited: true,
		Container: true}
	b, err := Encode(h)
	if err != nil {
		t.Fatalf("largest hold refused: %v", err)
	}
	if len(b) > MaxBodyBytes {
		t.Fatalf("largest hold is %d bytes, over %d", len(b), MaxBodyBytes)
	}
	if back, err := DecodeHold(b); err != nil || back != h {
		t.Errorf("largest hold does not round trip: %v", err)
	}
	tk := Ticket{Label: lbl, NeedMiB: spec.MaxNeedMiB, CreatedNS: math.MaxInt64, Restart: true}
	b, err = Encode(tk)
	if err != nil || len(b) > MaxBodyBytes {
		t.Fatalf("largest ticket: %d bytes, %v", len(b), err)
	}
	if back, err := DecodeTicket(b); err != nil || back != tk {
		t.Errorf("largest ticket does not round trip: %v", err)
	}
}

func TestEncodeCanonicalAndStable(t *testing.T) {
	h := Hold{Label: "x", PID: 7300, StartNS: 3, ArrivalNS: 2, NeedMiB: 2048, Stops: 1, FootprintMiB: 900,
		DUserNS: 10, DSysNS: 20, DPageWaitNS: 30, NormalShare: 0.04, SampledNS: 40, Victim: true, Learned: true}
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
	if want := `{"label":"x","pid":7300,"start_ns":3,"arrival_ns":2,"need_mib":2048,"stops":1,"footprint_mib":900,` +
		`"d_user_ns":10,"d_sys_ns":20,"d_page_wait_ns":30,"normal_share":0.04,"sampled_ns":40,"victim":true,"learned":true,"child_exited":false}`; string(a) != want {
		t.Errorf("hold body = %s\nwant        %s", a, want)
	}
}

// A hold from a build without the key reads as no container, the mark round-trips, and a false
// mark leaves the body byte for byte what such a build wrote. Mutations: move container into the
// required-boolean loop → the old body is refused; drop its omitempty → the plain body gains the
// key, or read it nowhere → true does not come back.
func TestHoldContainerKey(t *testing.T) {
	old := `{"label":"x","pid":7300,"start_ns":3,"arrival_ns":2,"need_mib":1,"stops":0,"footprint_mib":0,` +
		`"d_user_ns":0,"d_sys_ns":0,"d_page_wait_ns":0,"normal_share":0,"sampled_ns":0,"victim":true,"learned":false,"child_exited":false}`
	h, err := DecodeHold([]byte(old))
	if err != nil || h.Container {
		t.Fatalf("a hold without the key: %+v, %v; want Container false", h, err)
	}
	b, err := Encode(h)
	if err != nil || string(b) != old {
		t.Errorf("a hold with no mark encodes to %s, %v; want the same bytes as %s", b, err, old)
	}
	h.Container = true
	b, err = Encode(h)
	if err != nil || !bytes.HasSuffix(b, []byte(`"child_exited":false,"container":true}`)) {
		t.Fatalf("a marked hold encodes to %s, %v", b, err)
	}
	if back, err := DecodeHold(b); err != nil || back != h {
		t.Errorf("a marked hold reads back as %+v, %v; want %+v", back, err, h)
	}
}

// The fixtures under testdata are the contract of the wire format: every valid file decodes to
// its expectation, every bad file is malformed, every name is accepted or refused as stated.

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

func TestFixtureDisplay(t *testing.T) {
	var disp []struct{ Label, Display string }
	readJSON(t, "testdata/display.json", &disp)
	for _, c := range disp {
		if got := Display(c.Label); got != c.Display {
			t.Errorf("Display(%q) = %q, fixture says %q", c.Label, got, c.Display)
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
	if !bytes.Equal(enc, body) {
		t.Errorf("largest hold encodes to %s, the fixture is %s", enc, body)
	}
	if h.Label != largestLabel(t) || h.NeedMiB != spec.MaxNeedMiB || h.FootprintMiB != spec.MaxNeedMiB ||
		h.TypicalS != math.MaxInt64 {
		t.Errorf("fixture is not the largest hold: %+v", h)
	}
}

func TestNameDisplay(t *testing.T) {
	id := ID{OrderKey: 5, PID: 7, Seq: 9}
	for name, want := range map[string]string{
		TicketName(id, ""):           "",
		TicketName(id, "gate:unit"):  "gate_unit",
		HoldName(5, 7, 9, "ci job"):  "ci_job",
		id.String() + "-bad/display": "", // not a name
		".tmp-0000000007-000009":     "",
		"gate":                       "",
	} {
		if got := NameDisplay(name); got != want {
			t.Errorf("NameDisplay(%q) = %q, want %q", name, got, want)
		}
	}
}

// A hold from a build without the key reads as not idle, a zero leaves the body as such a build
// wrote it, a value round-trips and a negative one is no body. Mutations: drop the key's
// omitempty → the plain body gains it, red; read it nowhere → 2100 does not come back, red; drop
// the range check → -1 is accepted, red.
func TestHoldNoCPUKey(t *testing.T) {
	old := `{"label":"x","pid":7300,"start_ns":3,"arrival_ns":2,"need_mib":1,"stops":0,"footprint_mib":0,` +
		`"d_user_ns":0,"d_sys_ns":0,"d_page_wait_ns":0,"normal_share":0,"sampled_ns":0,"victim":true,"learned":false,"child_exited":false}`
	h, err := DecodeHold([]byte(old))
	if err != nil || h.NoCPUS != 0 {
		t.Fatalf("a hold without the key: %+v, %v; want NoCPUS 0", h, err)
	}
	if b, err := Encode(h); err != nil || string(b) != old {
		t.Errorf("a hold not idle encodes to %s, %v; want the same bytes as %s", b, err, old)
	}
	h.NoCPUS = 2100
	b, err := Encode(h)
	if err != nil || !bytes.HasSuffix(b, []byte(`"child_exited":false,"no_cpu_s":2100}`)) {
		t.Fatalf("an idle hold encodes to %s, %v", b, err)
	}
	if back, err := DecodeHold(b); err != nil || back != h {
		t.Errorf("an idle hold reads back as %+v, %v; want %+v", back, err, h)
	}
	if _, err := DecodeHold([]byte(strings.Replace(string(b), "2100", "-1", 1))); err == nil {
		t.Error("no_cpu_s -1 was accepted")
	}
}
