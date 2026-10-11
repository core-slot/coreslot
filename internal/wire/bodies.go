package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// MaxBodyBytes is the largest body: 8 KiB (PROTOCOL.md section 7.2).
const MaxBodyBytes = 8192

// MaxTokenIndex is the highest token index a grant can name: sizes are at most 4096.
const MaxTokenIndex = spec.MaxSize - 1

// ErrMalformed is wrapped by every error a decoder returns for a body it refuses: not one UTF-8
// JSON object of at most 8 KiB, a key missing, of the wrong type or outside its rule, or a
// duplicate key at any depth. Unknown keys are not an error. Encode wraps it too for a value that
// could not be a valid body.
var ErrMalformed = errors.New("wire: malformed body")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// Ticket is the body of a queue entry (PROTOCOL.md section 7.3).
type Ticket struct {
	Label     string
	Priority  int
	Need      map[string]spec.Range
	CreatedNS int64
}

// Hold is the body of a holds entry (section 7.4). StartNS must equal the start time in the
// hold's name; the decoder cannot check that, the caller that has the name does.
type Hold struct {
	Label               string
	Grant               map[string][]int
	StartNS             uint64
	Unbudgeted          bool
	UnbudgetedResources []string
	Clamped             map[string]int
	Adopted             bool
}

// Cancel is the body of a cancel request (section 7.5).
type Cancel struct {
	RequestedNS  int64
	RequesterPID int
}

// ---------------------------------------------------------------- encoding

type ticketJSON struct {
	Label     string            `json:"label"`
	Priority  int               `json:"priority"`
	Need      map[string][2]int `json:"need"`
	CreatedNS int64             `json:"created_ns"`
}

type holdJSON struct {
	Label               string           `json:"label"`
	Grant               map[string][]int `json:"grant"`
	StartNS             uint64           `json:"start_ns"`
	Unbudgeted          bool             `json:"unbudgeted"`
	UnbudgetedResources []string         `json:"unbudgeted_resources"`
	Clamped             map[string]int   `json:"clamped"`
	Adopted             bool             `json:"adopted"`
}

type cancelJSON struct {
	RequestedNS  int64 `json:"requested_ns"`
	RequesterPID int   `json:"requester_pid"`
}

// Encode returns the body of a Ticket, Hold or Cancel (or a pointer to one): compact JSON with
// every key of its table, nil collections written empty, keys in the table's order and map keys
// sorted. It refuses, with ErrMalformed, a value no reader would accept.
func Encode(v any) ([]byte, error) {
	var out any
	switch t := v.(type) {
	case Ticket:
		out = t.wire()
	case *Ticket:
		if t != nil {
			out = t.wire()
		}
	case Hold:
		out = t.wire()
	case *Hold:
		if t != nil {
			out = t.wire()
		}
	case Cancel:
		out = cancelJSON(t)
	case *Cancel:
		if t != nil {
			out = cancelJSON(*t)
		}
	default:
		return nil, malformed("cannot encode a %T", v)
	}
	if out == nil {
		return nil, malformed("cannot encode a nil %T", v)
	}
	if err := validate(v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, malformed("%v", err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > MaxBodyBytes {
		return nil, malformed("body is %d bytes, at most %d", len(b), MaxBodyBytes)
	}
	return b, nil
}

func validate(v any) error {
	switch t := v.(type) {
	case Ticket:
		return t.validate()
	case *Ticket:
		return t.validate()
	case Hold:
		return t.validate()
	case *Hold:
		return t.validate()
	}
	return nil // a Cancel has no rule beyond its types
}

func (t Ticket) wire() ticketJSON {
	need := make(map[string][2]int, len(t.Need))
	for name, r := range t.Need {
		need[name] = [2]int{r.Min, r.Max}
	}
	return ticketJSON{Label: t.Label, Priority: t.Priority, Need: need, CreatedNS: t.CreatedNS}
}

func (h Hold) wire() holdJSON {
	j := holdJSON{Label: h.Label, Grant: h.Grant, StartNS: h.StartNS, Unbudgeted: h.Unbudgeted,
		UnbudgetedResources: h.UnbudgetedResources, Clamped: h.Clamped, Adopted: h.Adopted}
	if j.Grant == nil {
		j.Grant = map[string][]int{}
	}
	if j.UnbudgetedResources == nil {
		j.UnbudgetedResources = []string{}
	}
	if j.Clamped == nil {
		j.Clamped = map[string]int{}
	}
	return j
}

// ---------------------------------------------------------------- rules

func (t Ticket) validate() error {
	if err := (spec.Request{Label: t.Label, Priority: t.Priority, Need: t.Need}).Validate(); err != nil {
		return malformed("%v", err)
	}
	return nil
}

func (h Hold) validate() error {
	if len(h.Label) > spec.MaxLabelBytes || !utf8.ValidString(h.Label) {
		return malformed("label is not valid UTF-8 of at most %d bytes", spec.MaxLabelBytes)
	}
	if h.StartNS > math.MaxInt64 {
		return malformed("start_ns %d is outside the signed 64-bit range", h.StartNS)
	}
	total := 0
	for name, idx := range h.Grant {
		if err := spec.ValidateName(name); err != nil {
			return malformed("grant: %v", err)
		}
		if len(idx) > spec.MaxPerResource {
			return malformed("grant: %d tokens of %s, at most %d", len(idx), name, spec.MaxPerResource)
		}
		total += len(idx)
		for i, x := range idx {
			if x < 0 || x > MaxTokenIndex || i > 0 && x <= idx[i-1] {
				return malformed("grant: indices of %s must be ascending, distinct, in [0, %d]", name, MaxTokenIndex)
			}
		}
	}
	if total > spec.MaxPerGrant {
		return malformed("grant: %d tokens, at most %d", total, spec.MaxPerGrant)
	}
	for _, name := range h.UnbudgetedResources {
		if err := spec.ValidateName(name); err != nil {
			return malformed("unbudgeted_resources: %v", err)
		}
	}
	for name := range h.Clamped {
		if err := spec.ValidateName(name); err != nil {
			return malformed("clamped: %v", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- decoding

// DecodeTicket parses and validates a ticket body.
func DecodeTicket(body []byte) (Ticket, error) {
	o, err := parseObject(body)
	if err != nil {
		return Ticket{}, err
	}
	var t Ticket
	if t.Label, err = o.str("label"); err != nil {
		return Ticket{}, err
	}
	if t.Priority, err = o.smallInt("priority"); err != nil {
		return Ticket{}, err
	}
	if t.CreatedNS, err = o.int64("created_ns"); err != nil {
		return Ticket{}, err
	}
	need, err := o.object("need")
	if err != nil {
		return Ticket{}, err
	}
	t.Need = make(map[string]spec.Range, len(need))
	for name, v := range need {
		pair, ok := v.([]any)
		if !ok || len(pair) != 2 {
			return Ticket{}, malformed("need.%s is not an array of two integers", name)
		}
		lo, err1 := toInt(pair[0])
		hi, err2 := toInt(pair[1])
		if err1 != nil || err2 != nil || int64(int(lo)) != lo || int64(int(hi)) != hi {
			return Ticket{}, malformed("need.%s is not an array of two integers", name)
		}
		t.Need[name] = spec.Range{Min: int(lo), Max: int(hi)}
	}
	if err := t.validate(); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

// DecodeHold parses and validates a hold body.
func DecodeHold(body []byte) (Hold, error) {
	o, err := parseObject(body)
	if err != nil {
		return Hold{}, err
	}
	var h Hold
	if h.Label, err = o.str("label"); err != nil {
		return Hold{}, err
	}
	start, err := o.int64("start_ns")
	if err != nil {
		return Hold{}, err
	}
	if start < 0 {
		return Hold{}, malformed("start_ns is negative")
	}
	h.StartNS = uint64(start)
	if h.Unbudgeted, err = o.boolean("unbudgeted"); err != nil {
		return Hold{}, err
	}
	if h.Adopted, err = o.boolean("adopted"); err != nil {
		return Hold{}, err
	}
	grant, err := o.object("grant")
	if err != nil {
		return Hold{}, err
	}
	h.Grant = make(map[string][]int, len(grant))
	for name, v := range grant {
		list, ok := v.([]any)
		if !ok {
			return Hold{}, malformed("grant.%s is not an array", name)
		}
		idx := make([]int, len(list))
		for i, e := range list {
			n, err := toInt(e)
			if err != nil || int64(int(n)) != n {
				return Hold{}, malformed("grant.%s holds a non-integer", name)
			}
			idx[i] = int(n)
		}
		h.Grant[name] = idx
	}
	list, err := o.array("unbudgeted_resources")
	if err != nil {
		return Hold{}, err
	}
	h.UnbudgetedResources = make([]string, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			return Hold{}, malformed("unbudgeted_resources holds a non-string")
		}
		h.UnbudgetedResources[i] = s
	}
	clamped, err := o.object("clamped")
	if err != nil {
		return Hold{}, err
	}
	h.Clamped = make(map[string]int, len(clamped))
	for name, v := range clamped {
		n, err := toInt(v)
		if err != nil || int64(int(n)) != n {
			return Hold{}, malformed("clamped.%s is not an integer", name)
		}
		h.Clamped[name] = int(n)
	}
	if err := h.validate(); err != nil {
		return Hold{}, err
	}
	return h, nil
}

// DecodeCancel parses and validates a cancel request body.
func DecodeCancel(body []byte) (Cancel, error) {
	o, err := parseObject(body)
	if err != nil {
		return Cancel{}, err
	}
	var c Cancel
	if c.RequestedNS, err = o.int64("requested_ns"); err != nil {
		return Cancel{}, err
	}
	if c.RequesterPID, err = o.smallInt("requester_pid"); err != nil {
		return Cancel{}, err
	}
	return c, nil
}

// ---------------------------------------------------------------- strict JSON

// object is a parsed JSON object. Values are map[string]any, []any, json.Number, string, bool
// or nil, so a null and an absent key stay distinguishable.
type object map[string]any

// parseObject checks the body as a whole (size, UTF-8, one JSON object, nothing after it) and
// builds its tree. Duplicate keys are refused at every depth, comparing keys after unescaping,
// so `"cpu"` repeats `"cpu"`.
func parseObject(body []byte) (object, error) {
	if len(body) > MaxBodyBytes {
		return nil, malformed("body is %d bytes, at most %d", len(body), MaxBodyBytes)
	}
	if !utf8.Valid(body) {
		return nil, malformed("body is not valid UTF-8")
	}
	if !json.Valid(body) {
		return nil, malformed("body is not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, malformed("data after the object")
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, malformed("body is not a JSON object")
	}
	return object(o), nil
}

func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, malformed("%v", err)
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil // string, json.Number, bool or nil
	}
	switch d {
	case '{':
		m := map[string]any{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, malformed("%v", err)
			}
			key := kt.(string)
			if _, dup := m[key]; dup {
				return nil, malformed("duplicate key %q", key)
			}
			if m[key], err = readValue(dec); err != nil {
				return nil, err
			}
		}
		if _, err := dec.Token(); err != nil { // the closing brace
			return nil, malformed("%v", err)
		}
		return m, nil
	case '[':
		a := []any{}
		for dec.More() {
			e, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, e)
		}
		if _, err := dec.Token(); err != nil { // the closing bracket
			return nil, malformed("%v", err)
		}
		return a, nil
	}
	return nil, malformed("unexpected %v", d)
}

// toInt accepts a JSON number with no fraction and no exponent that fits a signed 64-bit
// integer: `1.0` and `1e3` are not integers (PROTOCOL.md section 1).
func toInt(v any) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, errors.New("not a number")
	}
	s := string(n)
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || i == 0 && c == '-') {
			return 0, errors.New("not an integer")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func (o object) get(key string) (any, error) {
	v, ok := o[key]
	if !ok {
		return nil, malformed("key %q is missing", key)
	}
	return v, nil
}

func (o object) str(key string) (string, error) {
	v, err := o.get(key)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", malformed("%q is not a string", key)
	}
	return s, nil
}

func (o object) boolean(key string) (bool, error) {
	v, err := o.get(key)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, malformed("%q is not a boolean", key)
	}
	return b, nil
}

func (o object) int64(key string) (int64, error) {
	v, err := o.get(key)
	if err != nil {
		return 0, err
	}
	n, err := toInt(v)
	if err != nil {
		return 0, malformed("%q is not an integer", key)
	}
	return n, nil
}

// smallInt is an integer that also fits the platform's int.
func (o object) smallInt(key string) (int, error) {
	n, err := o.int64(key)
	if err != nil {
		return 0, err
	}
	if int64(int(n)) != n {
		return 0, malformed("%q is out of range", key)
	}
	return int(n), nil
}

func (o object) object(key string) (map[string]any, error) {
	v, err := o.get(key)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, malformed("%q is not an object", key)
	}
	return m, nil
}

func (o object) array(key string) ([]any, error) {
	v, err := o.get(key)
	if err != nil {
		return nil, err
	}
	a, ok := v.([]any)
	if !ok {
		return nil, malformed("%q is not an array", key)
	}
	return a, nil
}
