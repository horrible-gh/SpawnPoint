package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"spawnpoint/internal/runas"
	"spawnpoint/internal/runner"
)

type capture struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (c *capture) Header() http.Header { return c.h }
func (c *capture) WriteHeader(n int) {
	if c.code == 0 {
		c.code = n
	}
}
func (c *capture) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = 200
	}
	return c.body.Write(b)
}
func (m *Manager) delegate(r *http.Request, p Principal) (*capture, map[string]any) {
	c := &capture{h: make(http.Header)}
	m.next.ServeHTTP(c, contextRequest(r, p))
	if c.code == 0 {
		c.code = 200
	}
	var out map[string]any
	_ = json.Unmarshal(c.body.Bytes(), &out)
	return c, out
}
func flush(w http.ResponseWriter, c *capture, out map[string]any) {
	for k, v := range c.h {
		w.Header()[k] = v
	}
	if out != nil {
		w.Header().Del("Content-Length")
		send(w, c.code, out)
	} else {
		w.WriteHeader(c.code)
		_, _ = w.Write(c.body.Bytes())
	}
}
func (m *Manager) actor(p Principal) string {
	if u := m.state.user(p.UserID); u != nil {
		return u.Login
	}
	return "anonymous"
}
func (m *Manager) deny(w http.ResponseWriter, p Principal, id string, status int) {
	_ = m.audit(m.actor(p), "권한 거부", id, "denied", "요청 권한 없음")
	fail(w, status, "이 작업을 수행할 권한이 없습니다")
}
func (m *Manager) processRequest(w http.ResponseWriter, r *http.Request, p Principal) {
	if m.runner == nil {
		fail(w, 503, "프로세스 관리자를 사용할 수 없습니다")
		return
	}
	route := r.URL.Path
	if route == "/processes" && r.Method == "POST" {
		m.createProcess(w, r, p)
		return
	}
	parts := strings.Split(strings.Trim(route, "/"), "/")
	if len(parts) == 2 && parts[0] == "processes" && r.Method == "PUT" {
		m.editProcess(w, r, p, parts[1])
		return
	}
	if len(parts) == 2 && parts[0] == "processes" && r.Method == "DELETE" {
		m.deleteProcess(w, r, p, parts[1])
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !p.Guest() && m.currentPrincipal(p) == nil {
		fail(w, 401, "세션이 만료되었습니다")
		return
	}
	if p.Guest() && !m.state.Settings.Guest {
		fail(w, 401, "로그인이 필요합니다")
		return
	}
	if route == "/spawn" {
		if !admin(m.currentPrincipal(p)) || p.Token != nil {
			m.deny(w, p, "spawn", 403)
			return
		}
	} else if route == "/help" {
		if p.Guest() {
			m.deny(w, p, "help", 403)
			return
		}
	} else if route == "/runtime" {
		// Runtime metadata is never an unauthenticated disclosure of the service user.
	} else if route == "/processes" {
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "허용되지 않는 메서드입니다")
			return
		}
	} else {
		if len(parts) < 2 || len(parts) > 3 || parts[0] != "processes" {
			fail(w, 404, "경로를 찾을 수 없습니다")
			return
		}
		info, ok := m.runner.Get(parts[1])
		if !ok {
			fail(w, 404, "프로세스가 없습니다")
			return
		}
		cap := m.capabilities(p, info.ID, info.RunAs != nil)
		if !cap.View {
			m.deny(w, p, info.ID, 404)
			return
		}
		if len(parts) == 2 {
			if r.Method != "GET" && r.Method != "HEAD" {
				fail(w, 405, "허용되지 않는 메서드입니다")
				return
			}
		} else if parts[2] == "logs" {
			if !cap.Logs {
				m.deny(w, p, info.ID, 403)
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" {
				fail(w, 405, "GET 요청이 필요합니다")
				return
			}
		} else {
			if !contains([]string{"run", "stop", "restart"}, parts[2]) {
				fail(w, 404, "경로를 찾을 수 없습니다")
				return
			}
			if r.Method != "POST" {
				fail(w, 405, "POST 요청이 필요합니다")
				return
			}
			if !cap.Run {
				m.deny(w, p, info.ID, 403)
				return
			}
		}
	}
	write := r.Method != "GET" && r.Method != "HEAD"
	if write {
		if e := m.audit(m.actor(p), "프로세스 작업 요청", route, "attempt", r.Method); e != nil {
			fail(w, 503, "감사 기록을 저장하지 못했습니다")
			return
		}
	}
	// HEAD must go through the same filtering as GET. No payload is written below.
	rr := r.Clone(r.Context())
	if r.Method == "HEAD" {
		rr.Method = "GET"
	}
	c, out := m.delegate(rr, p)
	if c.code == 200 && out != nil {
		if list, ok := out["processes"].([]any); ok {
			filtered := []any{}
			for _, x := range list {
				if row, ok := x.(map[string]any); ok {
					if m.filterProcess(p, row) {
						filtered = append(filtered, row)
					}
				}
			}
			out["processes"] = filtered
		}
		if row, ok := out["process"].(map[string]any); ok {
			m.filterProcess(p, row)
		}
		if route == "/runtime" {
			supported, _ := out["run_as_supported"].(bool)
			out["run_as_supported"] = supported && admin(m.currentPrincipal(p)) && Secure(r)
			if p.Guest() {
				delete(out, "service_account")
			}
			out["access_control"] = true
		}
	}
	if write {
		_ = m.audit(m.actor(p), "프로세스 작업 결과", route, fmt.Sprint(c.code), "비밀값 기록 안 함")
	}
	if r.Method == "HEAD" {
		for k, v := range c.h {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(c.code)
		return
	}
	flush(w, c, out)
}
func (m *Manager) filterProcess(p Principal, row map[string]any) bool {
	id, _ := row["id"].(string)
	_, specific := row["run_as"]
	cap := m.capabilities(p, id, specific)
	if !cap.View {
		return false
	}
	meta := m.state.meta(id)
	row["group_id"] = meta.PG
	row["capabilities"] = cap
	if p.Guest() {
		for k := range row {
			if !contains([]string{"id", "label", "status", "group_id", "capabilities"}, k) {
				delete(row, k)
			}
		}
	} else if !cap.Edit && !admin(m.currentPrincipal(p)) {
		delete(row, "env")
	}
	return true
}

type commandInput struct {
	Cmd       string            `json:"cmd"`
	Label     string            `json:"label"`
	Cwd       *string           `json:"cwd"`
	Env       map[string]string `json:"env"`
	AutoStart *bool             `json:"auto_start"`
	Start     *bool             `json:"start"`
	RunAs     json.RawMessage   `json:"run_as"`
	PG        string            `json:"group_id"`
	Protected *bool             `json:"protected"`
}

func commandBody(w http.ResponseWriter, r *http.Request) (commandInput, *runas.Input, error) {
	var in commandInput
	if e := readJSON(w, r, &in); e != nil {
		return in, nil, e
	}
	if strings.TrimSpace(in.Cmd) == "" || utf8.RuneCountInString(in.Cmd) > 4096 || strings.ContainsRune(in.Cmd, 0) {
		return in, nil, errors.New("명령어는 1~4096자로 입력하세요")
	}
	if in.Label == "" {
		in.Label = strings.Fields(in.Cmd)[0]
	}
	if utf8.RuneCountInString(in.Label) > 128 || !textOK(in.Label, 512) {
		return in, nil, errors.New("프로세스 이름을 확인하세요")
	}
	if in.Cwd != nil && strings.ContainsRune(*in.Cwd, 0) {
		return in, nil, errors.New("작업 경로를 확인하세요")
	}
	for k, v := range in.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return in, nil, errors.New("환경변수 이름 또는 값을 확인하세요")
		}
	}
	var identity *runas.Input
	if len(in.RunAs) > 0 {
		if string(bytes.TrimSpace(in.RunAs)) == "null" {
			return in, nil, errors.New("run_as must be an object")
		}
		var v runas.Input
		d := json.NewDecoder(bytes.NewReader(in.RunAs))
		d.DisallowUnknownFields()
		if e := d.Decode(&v); e != nil {
			return in, nil, errors.New("실행 계정 입력을 확인하세요")
		}
		n, e := runas.Normalize(v)
		if e != nil {
			return in, nil, e
		}
		identity = &n
	}
	return in, identity, nil
}
func (m *Manager) createProcess(w http.ResponseWriter, r *http.Request, p Principal) {
	in, identity, e := commandBody(w, r)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.currentPrincipal(p)
	if u == nil {
		fail(w, 401, "로그인 세션 또는 토큰이 만료되었습니다")
		return
	}
	if in.PG == "" {
		in.PG = "pg-default"
		if p.Token != nil {
			in.PG = p.Token.Scope
		}
	}
	if !m.mayCreate(p, in.PG) {
		m.deny(w, p, in.PG, 403)
		return
	}
	isAdmin := admin(u) && p.Token == nil
	if (identity != nil || in.Protected != nil) && !isAdmin {
		m.deny(w, p, in.PG, 403)
		return
	}
	if identity != nil && !Secure(r) {
		fail(w, 403, "실행 계정에는 HTTPS 또는 localhost가 필요합니다")
		return
	}
	if e = m.audit(m.actor(p), "프로세스 생성 요청", in.PG, "attempt", "설정·비밀번호 비기록"); e != nil {
		fail(w, 503, "감사 기록 저장 실패")
		return
	}
	auto := in.AutoStart != nil && *in.AutoStart
	var info runner.Info
	var persisted bool
	if identity != nil {
		impl, ok := m.runner.(identityRunner)
		if !ok {
			fail(w, 503, "실행 계정을 지원하지 않습니다")
			return
		}
		info, persisted, e = impl.RegisterIdentity(in.Label, in.Cmd, in.Cwd, in.Env, *identity, false, auto)
	} else {
		info, persisted = m.runner.RegisterOnly(in.Label, in.Cmd, in.Cwd, in.Env, auto)
	}
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if !persisted {
		m.runner.Delete(info.ID)
		fail(w, 503, "프로세스 저장에 실패했습니다. 실행하지 않았습니다")
		return
	}
	next, _ := cloneState(m.state)
	protected := info.RunAs != nil
	if in.Protected != nil {
		protected = protected || *in.Protected
	}
	next.Processes[info.ID] = ProcessMeta{PG: in.PG, Protected: protected, Overrides: map[string]Grant{}}
	if e = m.commit(next, m.actor(p), "프로세스 생성", info.ID, "권한 상속 후 등록"); e != nil {
		m.runner.Delete(info.ID)
		fail(w, 503, "권한 저장에 실패했습니다. 실행하지 않았습니다")
		return
	}
	start := in.Start == nil || *in.Start
	if start {
		info, _ = m.runner.Run(info.ID)
	}
	m.sendCreatedProcess(w, r, p, info.ID)
}
func (m *Manager) editProcess(w http.ResponseWriter, r *http.Request, p Principal, id string) {
	in, identity, e := commandBody(w, r)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.currentPrincipal(p)
	info, ok := m.runner.Get(id)
	if !ok {
		fail(w, 404, "프로세스가 없습니다")
		return
	}
	cap := m.capabilities(p, id, info.RunAs != nil)
	if u == nil || !cap.Edit {
		m.deny(w, p, id, 403)
		return
	}
	if (identity != nil || in.Protected != nil) && (!admin(u) || p.Token != nil) {
		m.deny(w, p, id, 403)
		return
	}
	meta := m.state.meta(id)
	if in.PG != "" && in.PG != meta.PG {
		fail(w, 400, "그룹 이동은 권한 영향 확인 후 일괄 이동에서 처리하세요")
		return
	}
	payload := map[string]any{"cmd": in.Cmd, "label": in.Label, "cwd": "", "env": in.Env}
	if in.Cwd != nil {
		payload["cwd"] = *in.Cwd
	}
	if in.AutoStart != nil {
		payload["auto_start"] = *in.AutoStart
	}
	if identity != nil {
		payload["run_as"] = identity
	}
	if in.Env == nil {
		payload["env"] = map[string]string{}
	}
	raw, _ := json.Marshal(payload)
	rr := r.Clone(r.Context())
	rr.Body = io.NopCloser(bytes.NewReader(raw))
	rr.ContentLength = int64(len(raw))
	if e = m.audit(u.Login, "프로세스 편집 요청", id, "attempt", "명령어·환경변수 비기록"); e != nil {
		fail(w, 503, "감사 기록 저장 실패")
		return
	}
	c, out := m.delegate(rr, p)
	if c.code == 200 && out != nil {
		if saved, ok := out["persisted"].(bool); ok && !saved {
			fail(w, 503, "설정을 디스크에 저장하지 못했습니다. 재시작하지 않았습니다")
			return
		}
		if in.Protected != nil {
			next, _ := cloneState(m.state)
			meta.Protected = *in.Protected
			next.Processes[id] = meta
			if e = m.commit(next, u.Login, "프로세스 보호 설정", id, "편집 보호 적용"); e != nil {
				fail(w, 503, "명령은 저장되었으나 보호 설정을 저장하지 못했습니다")
				return
			}
		}
		if row, ok := out["process"].(map[string]any); ok {
			m.filterProcess(p, row)
		}
	}
	_ = m.audit(u.Login, "프로세스 편집 결과", id, fmt.Sprint(c.code), "설정 저장 결과")
	flush(w, c, out)
}
func (m *Manager) deleteProcess(w http.ResponseWriter, r *http.Request, p Principal, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.currentPrincipal(p)
	if !admin(u) || p.Token != nil {
		m.deny(w, p, id, 403)
		return
	}
	if e := m.audit(u.Login, "프로세스 삭제 요청", id, "attempt", ""); e != nil {
		fail(w, 503, "감사 기록 저장 실패")
		return
	}
	c, out := m.delegate(r, p)
	if c.code == 200 && out != nil {
		if saved, ok := out["persisted"].(bool); ok && !saved {
			fail(w, 503, "삭제를 디스크에 반영하지 못했습니다")
			return
		}
		next, _ := cloneState(m.state)
		delete(next.Processes, id)
		if e := m.commit(next, u.Login, "프로세스 삭제", id, "등록·권한 제거"); e != nil {
			fail(w, 503, "프로세스는 삭제되었으나 권한 기록 정리에 실패했습니다")
			return
		}
	}
	flush(w, c, out)
}
