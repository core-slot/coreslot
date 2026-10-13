package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

// object is a parsed JSON object. Values are map[string]any, []any, json.Number, string, bool
// or nil, so a null and an absent key stay distinguishable.
type object map[string]any

// parseObject checks the body as a whole (size, UTF-8, one JSON object, nothing after it) and
// builds its tree. Duplicate keys are refused at every depth, comparing keys after unescaping,
// so `"c\u0070u"` repeats `"cpu"`. A string with an unpaired UTF-16 surrogate escape is refused too;
// Go's decoder would silently turn it into U+FFFD, so the scan reads the raw bytes.
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
	if err := checkSurrogates(body); err != nil {
		return nil, err
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

// checkSurrogates refuses a `\uD800`-`\uDBFF` escape not immediately followed by a
// `\uDC00`-`\uDFFF` escape, and a `\uDC00`-`\uDFFF` escape not preceded by one. body is valid JSON,
// so a backslash is always the start of an escape of a string and the escaped character after
// it is never itself the start of one: `\\u0041` is a backslash followed by the text "u0041".
func checkSurrogates(body []byte) error {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		if body[i+1] != 'u' {
			i++ // skip the escaped character, which may be a backslash
			continue
		}
		u, _ := hex4(body[i+2:])
		switch {
		case u >= 0xdc00 && u <= 0xdfff:
			return malformed("unpaired low surrogate escape")
		case u >= 0xd800 && u <= 0xdbff:
			if len(body) >= i+12 && body[i+6] == '\\' && body[i+7] == 'u' {
				if lo, _ := hex4(body[i+8:]); lo >= 0xdc00 && lo <= 0xdfff {
					i += 11
					continue
				}
			}
			return malformed("unpaired high surrogate escape")
		}
		i += 5
	}
	return nil
}

// hex4 reads four hexadecimal digits; body is valid JSON, so after "\u" they are there.
func hex4(b []byte) (rune, bool) {
	var v rune
	for _, c := range b[:4] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}
