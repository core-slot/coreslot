package wire

import (
	"errors"
	"fmt"

	"github.com/core-slot/coreslot/internal/spec"
)

// MaxBodyBytes is the largest body: 8 KiB (PROTOCOL.md section 7.2).
const MaxBodyBytes = 8192

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
	NeedMiB   int64
	CreatedNS int64
	Restart   bool // optional: a stopped run coming back
}

// Hold is the body of a holds entry (section 7.4): the reservation of a run and what its run last
// measured. PID and StartNS must equal the pid and start time in the hold's name; the decoder
// cannot check that, the caller that has the name does. A NeedMiB of 0 reserves nothing: a run
// outside the budget.
type Hold struct {
	Label                        string
	PID                          int
	StartNS, ArrivalNS           int64
	NeedMiB                      int64
	TypicalS                     int64 // optional: the label's typical run time in seconds, 0 unknown
	Stops                        int
	FootprintMiB                 int64
	DUserNS, DSysNS, DPageWaitNS int64
	NormalShare                  float64
	SampledNS                    int64
	Victim, Learned, ChildExited bool
	CPUNS                        int64   // optional: the attempt's user and system time so far
	TypicalCPUS                  float64 // optional: the label's typical user and system seconds, 0 unknown
	StallEWMA                    float64 // optional: the run's smoothed stall share, 0..1
	Container                    bool    // optional: container work, so never a stop round's victim
	IdleS                        int64   // optional: how long the tree has done next to nothing, in s
}

// Cancel is the body of a cancel request (section 7.5).
type Cancel struct {
	RequestedNS  int64
	RequesterPID int
}

// ---------------------------------------------------------------- encoding

type ticketJSON struct {
	Label     string `json:"label"`
	NeedMiB   int64  `json:"need_mib"`
	CreatedNS int64  `json:"created_ns"`
	Restart   bool   `json:"restart,omitempty"`
}

type holdJSON struct {
	Label        string  `json:"label"`
	PID          int     `json:"pid"`
	StartNS      int64   `json:"start_ns"`
	ArrivalNS    int64   `json:"arrival_ns"`
	NeedMiB      int64   `json:"need_mib"`
	TypicalS     int64   `json:"typical_s,omitempty"`
	Stops        int     `json:"stops"`
	FootprintMiB int64   `json:"footprint_mib"`
	DUserNS      int64   `json:"d_user_ns"`
	DSysNS       int64   `json:"d_sys_ns"`
	DPageWaitNS  int64   `json:"d_page_wait_ns"`
	NormalShare  float64 `json:"normal_share"`
	SampledNS    int64   `json:"sampled_ns"`
	Victim       bool    `json:"victim"`
	Learned      bool    `json:"learned"`
	ChildExited  bool    `json:"child_exited"`
	CPUNS        int64   `json:"cpu_ns,omitempty"`
	TypicalCPUS  float64 `json:"typical_cpu_s,omitempty"`
	StallEWMA    float64 `json:"stall_ewma,omitempty"`
	Container    bool    `json:"container,omitempty"`
	IdleS        int64   `json:"idle_s,omitempty"`
}

type cancelJSON struct {
	RequestedNS  int64 `json:"requested_ns"`
	RequesterPID int   `json:"requester_pid"`
}

// Encode returns the body of a Ticket, Hold or Cancel (or a pointer to one): compact JSON with
// every key of its table, in the table's order. It refuses, with ErrMalformed, a value no reader
// would accept.
func Encode(v any) ([]byte, error) {
	var out any
	switch t := v.(type) {
	case Ticket:
		out = ticketJSON(t)
	case *Ticket:
		if t != nil {
			out = ticketJSON(*t)
		}
	case Hold:
		out = holdJSON(t)
	case *Hold:
		if t != nil {
			out = holdJSON(*t)
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
	return encodeCompact(out)
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

// ---------------------------------------------------------------- rules

func (t Ticket) validate() error {
	if err := (spec.Request{Label: t.Label, NeedMiB: t.NeedMiB}).Validate(); err != nil {
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
	if err := spec.ValidateLabel(h.Label); err != nil {
		return malformed("%v", err)
	}
	if h.PID < 0 || h.Stops < 0 ||
		min(h.StartNS, h.ArrivalNS, h.TypicalS, h.DUserNS, h.DSysNS, h.DPageWaitNS, h.SampledNS, h.CPUNS, h.IdleS) < 0 {
		return malformed("pid, stops and every time are at least 0")
	}
	if h.NeedMiB < 0 || h.NeedMiB > spec.MaxNeedMiB || h.FootprintMiB < 0 || h.FootprintMiB > spec.MaxNeedMiB {
		return malformed("need_mib and footprint_mib are in [0, %d]", spec.MaxNeedMiB)
	}
	if !(h.NormalShare >= 0 && h.NormalShare <= 1) || !(h.StallEWMA >= 0 && h.StallEWMA <= 1) {
		return malformed("normal_share and stall_ewma are in [0, 1]")
	}
	if !(h.TypicalCPUS >= 0) {
		return malformed("typical_cpu_s is at least 0")
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
	if t.NeedMiB, err = o.int64("need_mib"); err != nil {
		return Ticket{}, err
	}
	if t.CreatedNS, err = o.int64("created_ns"); err != nil {
		return Ticket{}, err
	}
	if t.Restart, err = optional(o, "restart", o.boolean); err != nil {
		return Ticket{}, err
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
	for _, f := range []struct {
		key string
		dst *int
	}{{"pid", &h.PID}, {"stops", &h.Stops}} {
		if *f.dst, err = o.smallInt(f.key); err != nil {
			return Hold{}, err
		}
	}
	for _, f := range []struct {
		key string
		dst *int64
	}{{"start_ns", &h.StartNS}, {"arrival_ns", &h.ArrivalNS}, {"need_mib", &h.NeedMiB},
		{"footprint_mib", &h.FootprintMiB}, {"d_user_ns", &h.DUserNS}, {"d_sys_ns", &h.DSysNS},
		{"d_page_wait_ns", &h.DPageWaitNS}, {"sampled_ns", &h.SampledNS}} {
		if *f.dst, err = o.int64(f.key); err != nil {
			return Hold{}, err
		}
	}
	if h.NormalShare, err = o.float("normal_share"); err != nil {
		return Hold{}, err
	}
	if h.TypicalS, err = optional(o, "typical_s", o.int64); err != nil {
		return Hold{}, err
	}
	if h.CPUNS, err = optional(o, "cpu_ns", o.int64); err != nil {
		return Hold{}, err
	}
	if h.IdleS, err = optional(o, "idle_s", o.int64); err != nil {
		return Hold{}, err
	}
	if h.Container, err = optional(o, "container", o.boolean); err != nil {
		return Hold{}, err
	}
	for _, f := range []struct {
		key string
		dst *float64
	}{{"typical_cpu_s", &h.TypicalCPUS}, {"stall_ewma", &h.StallEWMA}} {
		if *f.dst, err = optional(o, f.key, o.float); err != nil {
			return Hold{}, err
		}
	}
	for _, f := range []struct {
		key string
		dst *bool
	}{{"learned", &h.Learned}, {"victim", &h.Victim}, {"child_exited", &h.ChildExited}} {
		if *f.dst, err = o.boolean(f.key); err != nil {
			return Hold{}, err
		}
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
