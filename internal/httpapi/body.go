package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// readObject reads the request body and returns its top-level members.
//
// The members come back as raw JSON rather than as decoded Go values on
// purpose. Validation has to tell three states apart — absent, present and
// null, present and the wrong type — and it has to reject `true` where an
// integer belongs (0007-P [인스턴스 발급 — 검증 실패]). Decoding into `any`
// loses the first distinction and turns the second into a bool that reads as an
// integer in most type switches.
//
// An empty body is an empty object, not an error. The field checks then produce
// the complaint, which is `cmd` for POST /processes and `requester` for /spawn
// (0007-P [잘못된 본문]).
func readObject(r *http.Request) (map[string]json.RawMessage, *apiError) {
	empty := map[string]json.RawMessage{}
	if r.Body == nil {
		return empty, nil
	}
	// One byte past the limit: enough to know the body is too large without
	// holding any more of it than that. This backs up the Content-Length check
	// in ServeHTTP, which a chunked request carries no value for.
	data, err := io.ReadAll(io.LimitReader(r.Body, RequestBodyMaxBytes+1))
	if err != nil {
		e := invalidBody(msgNotJSON)
		return nil, &e
	}
	if len(data) > RequestBodyMaxBytes {
		e := tooLargeError()
		return nil, &e
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return empty, nil
	}

	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		e := invalidBody(msgNotJSON)
		return nil, &e
	}
	if _, ok := probe.(map[string]any); !ok {
		// Valid JSON, wrong shape: an array, a bare string, a number.
		e := invalidBody(msgNotObject)
		return nil, &e
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		e := invalidBody(msgNotJSON)
		return nil, &e
	}
	return obj, nil
}

// member returns the raw value for key, and whether the key was there at all.
// A key present with a null value comes back as (null, true), which is what
// lets `"cwd": null` and a missing `cwd` be treated alike while `"cwd": 12345`
// is not.
func member(obj map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := obj[key]
	return raw, ok
}

// present reports whether key carries a value that is not null.
func present(obj map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := member(obj, key)
	if !ok || isNull(raw) {
		return nil, false
	}
	return raw, true
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// asString decodes a JSON string. Anything else — a number, a bool, an object —
// fails, which is what produces `cwd must be a string.` rather than a coerced
// value nobody asked for.
func asString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// asStringMap decodes a JSON object whose every value is a string. JSON keys
// are always strings, so only the values can fail the check that
// 0008-L 4.1 words as "string keys and values".
func asStringMap(raw json.RawMessage) (map[string]string, bool) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(members))
	for k, v := range members {
		s, ok := asString(v)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

// asInteger decodes a JSON integer.
//
// json.Number is used rather than an int so that `3600.0` and `3.6e3` are
// rejected: they are the same quantity, but accepting them would mean the
// server decides what a caller's fractional seconds round to. `true` is
// rejected for the same reason the current implementation rejects it — bool is
// a subtype of int in Python and the check there is explicit
// (0007-P [인스턴스 발급 — 검증 실패]).
func asInteger(raw json.RawMessage) (int64, bool) {
	text := bytes.TrimSpace(raw)
	if len(text) == 0 {
		return 0, false
	}
	// The leading byte is checked before decoding because json.Number is a
	// string type, and encoding/json will happily read the JSON string "3600"
	// into one. A quoted number is not an integer here — the current
	// implementation's isinstance(value, int) rejects it, and a server that
	// silently accepted it would be inventing a coercion the contract does not
	// describe.
	if c := text[0]; c != '-' && (c < '0' || c > '9') {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(text, &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}
