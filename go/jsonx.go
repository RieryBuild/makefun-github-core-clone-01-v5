// JSON helpers: a small object-body parser for request bodies, and an
// append-style string escaper for the hand-built serializers.
//
// Parsing leans on encoding/json (correctness first: escapes, unicode,
// nesting). Serialization on the hot paths is hand-rolled appends, because
// encoding/json's reflection and its HTML escaping (<, >, &) do not match
// this forge's wire format — the reference behavior is JSON.stringify, which
// leaves <, > and & alone and escapes only ", \, and control characters.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// jsonEscape appends s with JSON string escaping, without the surrounding
// quotes: " and \ are backslash-escaped, control characters use the short
// forms where they exist (\n \r \t \b \f) and \u00XX otherwise. Bytes >=
// 0x20 pass through untouched, so valid UTF-8 (including non-ASCII) is
// preserved byte for byte, exactly as JSON.stringify emits it. An invalid
// UTF-8 sequence (possible only from raw git object bytes, never from a
// parsed JSON body) becomes U+FFFD, so the output is always valid JSON.
func jsonEscape(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0')
			dst = append(dst, "0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf])
		case c < 0x80:
			dst = append(dst, c)
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				dst = append(dst, 0xef, 0xbf, 0xbd)
			} else {
				dst = append(dst, s[i:i+size]...)
				i += size - 1
			}
		}
	}
	return dst
}

// jsonQuote appends s as a complete JSON string literal, quotes included.
func jsonQuote(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = jsonEscape(dst, s)
	return append(dst, '"')
}

// jsonQuoteBytes is jsonQuote for a []byte string.
func jsonQuoteBytes(dst, s []byte) []byte {
	dst = append(dst, '"')
	dst = jsonEscape(dst, string(s))
	return append(dst, '"')
}

// objBody is a parsed JSON object body: raw per-field values, decoded lazily.
type objBody map[string]json.RawMessage

// parseObjBody parses a request body as a JSON object. An empty body is an
// empty object; anything that is not a JSON object (array, null, scalar,
// malformed) is an error, matching the reference "Problems parsing JSON".
func parseObjBody(body []byte) (objBody, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return objBody{}, true
	}
	if trimmed[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, true
}

// str returns the field as a string, and whether it was present and a string.
func (b objBody) str(name string) (string, bool) {
	raw, ok := b[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// strOr returns the field as a string, or def when absent or not a string.
func (b objBody) strOr(name, def string) string {
	if s, ok := b.str(name); ok {
		return s
	}
	return def
}

// boolean returns the field as a bool, and whether it was present and a bool.
func (b objBody) boolean(name string) (bool, bool) {
	raw, ok := b[name]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return v, true
}

// boolOr returns the field as a bool, or def when absent or not a bool.
func (b objBody) boolOr(name string, def bool) bool {
	if v, ok := b.boolean(name); ok {
		return v
	}
	return def
}

// strList returns the field as a list of strings (non-string elements are
// dropped), or nil when the field is absent or not an array.
func (b objBody) strList(name string) []string {
	raw, ok := b[name]
	if !ok {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// num returns the field as a float64, and whether it was present and a number.
func (b objBody) num(name string) (float64, bool) {
	raw, ok := b[name]
	if !ok {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// marshalNoHTML marshals v to JSON without HTML escaping, matching
// JSON.stringify's treatment of <, > and &.
func marshalNoHTML(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	out := buf.Bytes()
	// Encode appends a trailing newline; drop it.
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out
}

// atoiDigits parses a run of ASCII digits; ok is false on empty/non-digit.
func atoiDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// itoa appends the decimal form of n.
func itoa(dst []byte, n int) []byte {
	return strconv.AppendInt(dst, int64(n), 10)
}

// utf8Valid reports whether s is valid UTF-8 (used where the wire format
// must not echo invalid bytes back).
func utf8Valid(s string) bool {
	return utf8.ValidString(s)
}

// b64Encode returns the standard base64 encoding of s.
func b64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// b64Decode decodes standard base64.
func b64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}
