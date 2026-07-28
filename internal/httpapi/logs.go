package httpapi

import "net/http"

// logResponse is 0007-P [로그 최초 조회]. Every field is always present — none
// is omitted when it is zero or false, because the screen reads all seven on
// every poll and a missing `reset` would read as false.
type logResponse struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
	// StartOffset is where the read actually began. For a tail read that is not
	// where the window landed: the partial first line is skipped.
	StartOffset int64 `json:"start_offset"`
	// NextOffset is what to send back next time. It can be short of Size when
	// the tail of a multi-byte character was left for the next poll (E-10), so
	// the caller must echo this rather than compute one from Size.
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	Truncated  bool   `json:"truncated"`
	Reset      bool   `json:"reset"`
	Encoding   string `json:"encoding"`
}

// serveLogs answers a log query.
//
// The `offset` parameter is handed to the runner as the raw string it arrived
// as. Parsing it here would mean this package deciding what `tail:65536`, a
// negative number and a typo each mean, and those three answers are the reading
// contract (0008-L 4.2), not the transport's business. Sending the raw value
// down is also what keeps an unreadable parameter falling to the recent end of
// the file rather than to offset 0 — the current server parses it at this layer
// and turns one typo into a full read of a 20 MiB file (0004-NR 1.7).
//
// A missing log file is a 200 with an empty result, not a 404. The entry exists
// and has simply never run; a caller polling it should keep polling. 404 here
// means the entry is not registered, and nothing else (E-8, E-14).
func (s *Server) serveLogs(w http.ResponseWriter, r *http.Request, id string) {
	if s.runner == nil {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	// Query().Get returns "" both for an absent parameter and for `offset=`,
	// which is the right conflation: 0008-L 4.2 sends both to a tail read.
	view, ok := s.runner.ReadLog(id, r.URL.Query().Get("offset"))
	if !ok {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	s.writeJSON(w, r, http.StatusOK, logResponse{
		OK:          true,
		Text:        view.Text,
		StartOffset: view.StartOffset,
		NextOffset:  view.NextOffset,
		Size:        view.Size,
		Truncated:   view.Truncated,
		Reset:       view.Reset,
		Encoding:    view.Encoding,
	})
}
