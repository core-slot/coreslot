package wire

// AdmitLock is admit.lock's informational body (PROTOCOL.md section 7.7): the pid of the client
// that last entered the admission section and the wall clock of that entry. It is for display
// only: a reader tolerates an empty, stale or malformed body and never acts on it.
type AdmitLock struct {
	PID       int
	EnteredNS int64
}

type admitLockJSON struct {
	PID       int   `json:"pid"`
	EnteredNS int64 `json:"entered_ns"`
}

// validate is the one rule for the body, applied by the writer and by every reader: a positive
// pid and a non-negative entry time.
func (a AdmitLock) validate() error {
	if a.PID <= 0 {
		return malformed("pid %d is not positive", a.PID)
	}
	if a.EnteredNS < 0 {
		return malformed("entered_ns %d is negative", a.EnteredNS)
	}
	return nil
}

// EncodeAdmitLock returns admit.lock's body, `{"pid":<pid>,"entered_ns":<ns>}`. A value no reader
// would accept is refused with ErrMalformed.
func EncodeAdmitLock(a AdmitLock) ([]byte, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	return encodeCompact(admitLockJSON(a))
}

// DecodeAdmitLock parses admit.lock's body under the rules of section 7.2 and the rule of
// EncodeAdmitLock. Any error means the body is not to be shown.
func DecodeAdmitLock(body []byte) (AdmitLock, error) {
	o, err := parseObject(body)
	if err != nil {
		return AdmitLock{}, err
	}
	var a AdmitLock
	if a.PID, err = o.smallInt("pid"); err != nil {
		return AdmitLock{}, err
	}
	if a.EnteredNS, err = o.int64("entered_ns"); err != nil {
		return AdmitLock{}, err
	}
	if err := a.validate(); err != nil {
		return AdmitLock{}, err
	}
	return a, nil
}
