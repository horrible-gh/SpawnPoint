package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os/user"
	"runtime"

	"spawnpoint/internal/access"
	"spawnpoint/internal/requesttransport"
	"spawnpoint/internal/runas"
	"spawnpoint/internal/runner"
)

// The existing Runner contract is preserved for older integrations/mocks.
type identityRunner interface {
	RegisterIdentity(string, string, *string, map[string]string, runas.Input, bool, ...bool) (runner.Info, bool, error)
	UpdateIdentity(string, *string, *string, **string, map[string]string, *runas.Input, ...bool) (runner.Info, bool, bool, error)
}

func parseRunAs(raw json.RawMessage) (*runas.Input, *apiError) {
	if len(raw) == 0 {
		return nil, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, ptr(invalidField("run_as", "run_as must be an object"))
	}
	var input runas.Input
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return nil, ptr(invalidField("run_as", "invalid execution account fields"))
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, ptr(invalidField("run_as", "invalid execution account fields"))
	}
	normalized, err := runas.Normalize(input)
	if err != nil {
		return nil, ptr(invalidField("run_as", err.Error()))
	}
	return &normalized, nil
}
func sameOrigin(r *http.Request) bool { return requesttransport.SameOrigin(r) }
func (s *Server) identityTransport(r *http.Request) bool {
	if requesttransport.DirectLocal(r) {
		return true
	}
	return requesttransport.HTTPS(r) && (access.Authenticated(r) || (len(s.tokens) > 0 && s.authorised(r)))
}
func (s *Server) identityGuard(w http.ResponseWriter, r *http.Request, body bool) bool {
	if !sameOrigin(r) {
		s.identityRefuse(w, r, "Cross-origin execution requests are not allowed.")
		return false
	}
	if body {
		kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || kind != "application/json" {
			s.identityRefuse(w, r, "Execution credentials require application/json.")
			return false
		}
	}
	if !s.identityTransport(r) {
		s.identityRefuse(w, r, "Account execution requires localhost access or HTTPS with API authentication.")
		return false
	}
	return true
}
func (s *Server) identityRefuse(w http.ResponseWriter, r *http.Request, message string) {
	s.writeJSON(w, r, http.StatusForbidden, map[string]any{"ok": false, "error": map[string]any{"code": "forbidden", "message": message}})
}
func (s *Server) guardExistingIdentity(w http.ResponseWriter, r *http.Request, id string, body bool) bool {
	if s.runner == nil {
		return true
	}
	// Legacy Runner implementations cannot create execution identities.
	if _, supported := s.runner.(identityRunner); !supported {
		return true
	}
	if info, ok := s.runner.Get(id); ok && info.RunAs != nil {
		return s.identityGuard(w, r, body)
	}
	return true
}
func (s *Server) serveRuntime(w http.ResponseWriter, r *http.Request) {
	supported, reason := runas.Capability()
	if supported && !s.identityTransport(r) {
		supported = false
		reason = "Use localhost or authenticated HTTPS for execution credentials."
	}
	account := ""
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"ok": true, "os": runtime.GOOS, "service_account": account, "run_as_supported": supported, "run_as_reason": reason})
}
