package access

import "net/http"

// Keep POST /processes compatible with the existing complete process response.
// Reuse the canonical API renderer, then apply the same redaction/capabilities
// as GET. The caller already holds the policy lock and completed authorization.
func (m *Manager) sendCreatedProcess(w http.ResponseWriter, r *http.Request, p Principal, id string) {
	rr := r.Clone(r.Context())
	u := *r.URL
	rr.URL = &u
	rr.Method = "GET"
	rr.URL.Path = "/processes/" + id
	rr.URL.RawPath = ""
	rr.URL.RawQuery = ""
	rr.Body = http.NoBody
	rr.ContentLength = 0
	c, out := m.delegate(rr, p)
	row, ok := out["process"].(map[string]any)
	if c.code != http.StatusOK || !ok || !m.filterProcess(p, row) {
		fail(w, http.StatusServiceUnavailable, "프로세스는 등록되었으나 결과를 조회하지 못했습니다")
		return
	}
	out["persisted"] = true
	flush(w, c, out)
}
