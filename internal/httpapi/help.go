package httpapi

import "net/http"

func (s *Server) serveHelp(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", contentTypeJSON)
	h.Set("Cache-Control", "no-store")
	writeBody(w, r, http.StatusOK, s.help)
}
