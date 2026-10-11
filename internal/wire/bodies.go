package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	case Cancel:
		return t.validate()
	case *Cancel:
		return t.validate()
	}
	return nil
}

func (t Ticket) wire() ticketJSON {
	need := make(map[string][2]int, len(t.Need))
	for name, r := range t.Need {
		need[name] = [2]int{r.Min, r.Max}
	}
	return ticketJSON{Label: t.Label, Priority: t.Priority, Need: need, CreatedNS: t.CreatedNS}
}

func (h Hold) wire() holdJSON {
	j := holdJSON(h)
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
	if t.CreatedNS < 0 {
		return malformed("created_ns is negative")
	}
	return nil
}

func (c Cancel) validate() error {
	if c.RequestedNS < 0 || c.RequesterPID < 0 {
		return malformed("requested_ns and requester_pid are at least 0")
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
		if len(idx) == 0 {
			return malformed("grant: %s maps to an empty array; leave the resource out", name)
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
	for name, req := range h.Clamped {
		if err := spec.ValidateName(name); err != nil {
			return malformed("clamped: %v", err)
		}
		if req < 1 || req > spec.MaxPerResource {
			return malformed("clamped: %s is %d, want an integer in [1, %d]", name, req, spec.MaxPerResource)
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
	if err := c.validate(); err != nil {
		return Cancel{}, err
	}
	return c, nil
}
