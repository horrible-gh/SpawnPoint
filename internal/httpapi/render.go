package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"spawnpoint/internal/timefmt"
)

const contentTypeJSON = "application/json; charset=utf-8"

// writeJSON serialises payload and sends it.
//
// Two things about the encoding are contract rather than taste (0007-P 표기 규칙):
//
//   - Non-ASCII is not escaped. Go already leaves it alone, which matches the
//     current server's ensure_ascii=false, so a Korean label travels as itself.
//   - HTML escaping is switched off. Go's default rewrites <, > and & into
//     <, > and &, which is valid JSON but not the same bytes:
//     a `cmd` containing `>nul` — every registered command has one — would come
//     back visibly different from what was registered. The escaping exists for
//     JSON pasted into a <script> element, which nothing here does.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// Every payload in this package is a plain struct of strings, numbers
		// and maps, so this cannot happen from data. If it ever does, the
		// caller still gets a well-formed answer rather than a truncated one.
		s.writeJSON(w, r, http.StatusInternalServerError, errorResponse{
			OK:    false,
			Error: errorBody{Code: codeStorageError, Message: "Failed to encode the response."},
		})
		return
	}
	// Encode appends a newline. The contract shows none, and a HEAD request's
	// Content-Length has to agree with what a GET would send.
	body := bytes.TrimRight(buf.Bytes(), "\n")

	w.Header().Set("Content-Type", contentTypeJSON)
	writeBody(w, r, status, body)
}

// writeBody sends the status line, the length and — unless this is a HEAD — the
// body itself.
//
// Content-Length is set explicitly so that a HEAD answer carries the length the
// matching GET would have (0007-P 0.1: HEAD returns the headers of its GET).
// Left to net/http, a HEAD reply whose body is never written gets no
// Content-Length at all.
func writeBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

// responseTime is the response-side timestamp of 0007-P 0.3: local offset, six
// fractional digits, never abbreviated.
func responseTime(t time.Time) string { return timefmt.Response(t) }

// responseTimePtr renders an optional timestamp. A missing one is JSON null,
// not an empty string — the screen tests `started_at` for null to decide
// whether a process ever ran.
func responseTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := responseTime(*t)
	return &s
}
