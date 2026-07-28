package httpapi

import "net/http"

// The six error codes of 0007-P 0.2. There is no seventh: a condition that does
// not map onto one of these is a defect in this package, not a new code.
const (
	codeInvalidRequest   = "invalid_request"
	codeUnauthorized     = "unauthorized"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codePayloadTooLarge  = "payload_too_large"
	codeStorageError     = "storage_error"
)

// The message strings are fixed by 0007-P, down to the full stop. They are
// constants rather than literals at the call sites so that the two places each
// one is used — the handler and its contract test — cannot drift apart.
const (
	msgUnknownEndpoint  = "Unknown endpoint."
	msgProcessNotFound  = "Process not found."
	msgMethodNotAllowed = "Method not allowed."
	msgTooLarge         = "Request body is too large."
	msgUnauthorized     = "Valid authentication token required."
	msgNotJSON          = "Request body is not valid JSON."
	msgNotObject        = "Request body must be an object."
	msgInstanceStorage  = "Failed to save instance."
)

// apiError is a failure on its way to the wire. It carries the status rather
// than deriving it, because the mapping is one-way: `not_found` is always 404,
// but 404 is not always `not_found` in some future route.
type apiError struct {
	status  int
	code    string
	field   string
	message string
}

// errorBody is the `error` object of 0007-P 0.2. `field` is absent when the
// offending item cannot be named — absent, not null, which is why it is
// omitempty on a string rather than a pointer.
type errorBody struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type errorResponse struct {
	OK    bool      `json:"ok"`
	Error errorBody `json:"error"`
}

func (e apiError) response() errorResponse {
	return errorResponse{
		OK:    false,
		Error: errorBody{Code: e.code, Field: e.field, Message: e.message},
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, e apiError) {
	s.writeJSON(w, r, e.status, e.response())
}

func tooLargeError() apiError {
	return apiError{status: http.StatusRequestEntityTooLarge, code: codePayloadTooLarge, message: msgTooLarge}
}

func notFoundError(message string) apiError {
	return apiError{status: http.StatusNotFound, code: codeNotFound, message: message}
}

func methodNotAllowedError() apiError {
	return apiError{status: http.StatusMethodNotAllowed, code: codeMethodNotAllowed, message: msgMethodNotAllowed}
}

func unauthorisedError() apiError {
	return apiError{status: http.StatusUnauthorized, code: codeUnauthorized, message: msgUnauthorized}
}

// invalidBody is a body-level complaint: malformed JSON, or JSON that is not an
// object. Neither can name a field, because neither got far enough to have one.
func invalidBody(message string) apiError {
	return apiError{status: http.StatusBadRequest, code: codeInvalidRequest, message: message}
}

// invalidField is an item-level complaint. Only ever one at a time: validation
// stops at the first thing that is wrong and does not collect the rest
// (0007-P [새 명령 등록 — 검증 실패], 0008-L 4.1).
func invalidField(field, message string) apiError {
	return apiError{status: http.StatusBadRequest, code: codeInvalidRequest, field: field, message: message}
}

func storageError(message string) apiError {
	return apiError{status: http.StatusInternalServerError, code: codeStorageError, message: message}
}
