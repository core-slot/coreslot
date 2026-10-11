package wire

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// Gen is the body of $S/gen (PROTOCOL.md section 7.8). Gen is never negative: a negative value is
// malformed, and a missing or malformed gen reads as the zero Gen.
type Gen struct {
	Gen          int64
	ReconciledNS int64
}

// Degraded is the body of $S/degraded (section 7.9).
type Degraded struct {
	Reason  string
	FirstNS int64
	Count   int64
}

type genJSON struct {
	Gen          int64 `json:"gen"`
	ReconciledNS int64 `json:"reconciled_ns"`
}

type degradedJSON struct {
	Reason  string `json:"reason"`
	FirstNS int64  `json:"first_ns"`
	Count   int64  `json:"count"`
}

// DecodeGen parses a gen body under the rules of section 7.2.
func DecodeGen(body []byte) (Gen, error) {
	o, err := parseObject(body)
	if err != nil {
		return Gen{}, err
	}
	var g Gen
	if g.Gen, err = o.int64("gen"); err != nil {
		return Gen{}, err
	}
	if g.ReconciledNS, err = o.int64("reconciled_ns"); err != nil {
		return Gen{}, err
	}
	if g.Gen < 0 {
		return Gen{}, malformed("gen %d is negative", g.Gen)
	}
	return g, nil
}

// DecodeDegraded parses a degraded record body under the rules of section 7.2.
func DecodeDegraded(body []byte) (Degraded, error) {
	o, err := parseObject(body)
	if err != nil {
		return Degraded{}, err
	}
	var d Degraded
	if d.Reason, err = o.str("reason"); err != nil {
		return Degraded{}, err
	}
	if d.FirstNS, err = o.int64("first_ns"); err != nil {
		return Degraded{}, err
	}
	if d.Count, err = o.int64("count"); err != nil {
		return Degraded{}, err
	}
	return d, nil
}

// EncodeGen returns gen's body: compact JSON with both keys.
func EncodeGen(g Gen) ([]byte, error) {
	if g.Gen < 0 {
		return nil, malformed("gen %d is negative", g.Gen)
	}
	return encodeCompact(genJSON(g))
}

// EncodeDegraded returns the degraded record's body. A reason that makes the body exceed
// MaxBodyBytes, or that is not valid UTF-8, is refused with ErrMalformed; the caller shortens it.
func EncodeDegraded(d Degraded) ([]byte, error) {
	if !utf8.ValidString(d.Reason) {
		return nil, malformed("reason is not valid UTF-8")
	}
	return encodeCompact(degradedJSON(d))
}

func encodeCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, malformed("%v", err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > MaxBodyBytes {
		return nil, malformed("body is %d bytes, at most %d", len(b), MaxBodyBytes)
	}
	return b, nil
}
