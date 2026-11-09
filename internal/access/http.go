package access

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
)

func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	send(w, status, map[string]any{"ok": false, "error": map[string]string{"code": http.StatusText(status), "message": msg}})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	mt, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || mt != "application/json" {
		return errors.New("application/json 요청이 필요합니다")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return errors.New("올바른 JSON 요청을 입력하세요")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("요청 본문은 하나의 JSON이어야 합니다")
	}
	return nil
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.ContentLength > 65536 {
		fail(w, 413, "요청이 너무 큽니다")
		return
	}
	if r.URL.Path != path.Clean(r.URL.Path) {
		fail(w, 404, "경로를 찾을 수 없습니다")
		return
	}
	mutating := r.Method != "GET" && r.Method != "HEAD"
	if mutating && !sameOrigin(r) {
		fail(w, 403, "교차 출처 요청은 허용되지 않습니다")
		return
	}
	route := r.URL.Path
	if route == "/healthz" {
		m.next.ServeHTTP(w, r)
		return
	}
	if route == "/" || route == "/index.html" || route == "/login" || route == "/account" || route == "/admin" || strings.HasPrefix(route, "/admin/") {
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "허용되지 않는 메서드입니다")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != "HEAD" {
			_, _ = w.Write(m.ui)
		}
		return
	}
	if route == "/auth/login" || route == "/auth/setup" {
		if r.Method != "POST" {
			fail(w, 405, "POST 요청이 필요합니다")
			return
		}
		if !Secure(r) {
			fail(w, 403, "로그인에는 HTTPS 또는 localhost 접속이 필요합니다")
			return
		}
		m.login(w, r, route == "/auth/setup")
		return
	}
	p, authErr := m.principal(r)
	if authErr != nil {
		clearCookie(w, r)
		if route == "/auth/me" {
			p = Principal{}
		} else {
			fail(w, 401, "로그인이 필요하거나 세션이 만료되었습니다")
			return
		}
	}
	if route == "/auth/me" {
		if r.Method != "GET" {
			fail(w, 405, "GET 요청이 필요합니다")
			return
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		m.me(w, r, p)
		return
	}
	if mutating && !csrfOK(r, p) {
		fail(w, 403, "로그인과 유효한 CSRF 토큰이 필요합니다")
		return
	}
	if route == "/auth/logout" {
		if r.Method != "POST" || p.Token != nil {
			fail(w, 405, "세션 로그아웃 요청이 필요합니다")
			return
		}
		if _, e := m.db.Exec("DELETE FROM access_session WHERE digest=?", p.Session); e != nil {
			fail(w, 503, "세션을 종료하지 못했습니다")
			return
		}
		clearCookie(w, r)
		send(w, 200, map[string]any{"ok": true})
		return
	}
	if route == "/auth/reauth" || route == "/auth/password" || route == "/auth/revoke-sessions" || route == "/auth/heartbeat" {
		m.accountAction(w, r, p)
		return
	}
	if route == "/api/access/state" {
		if r.Method != "GET" {
			fail(w, 405, "GET 요청이 필요합니다")
			return
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		if !p.Guest() && m.currentPrincipal(p) == nil {
			fail(w, 401, "세션이 만료되었습니다")
			return
		}
		if p.Token != nil {
			fail(w, 403, "API 토큰으로 관리 상태를 조회할 수 없습니다")
			return
		}
		if p.Guest() && !m.state.Settings.Guest {
			fail(w, 401, "로그인이 필요합니다")
			return
		}
		v, e := m.snapshot(p)
		if e != nil {
			fail(w, 503, "상태를 읽지 못했습니다")
			return
		}
		send(w, 200, v)
		return
	}
	if route == "/api/access/admin" || route == "/api/access/preview" {
		m.adminAction(w, r, p, route == "/api/access/preview")
		return
	}
	if strings.HasPrefix(route, "/processes") || route == "/runtime" || route == "/help" || route == "/spawn" {
		m.processRequest(w, r, p)
		return
	}
	fail(w, 404, "경로를 찾을 수 없습니다")
}
func (m *Manager) me(w http.ResponseWriter, r *http.Request, p Principal) {
	u := m.currentPrincipal(p)
	var user any
	if u != nil {
		v := *u
		v.Hash = ""
		v.Subject = ""
		user = v
	}
	available, reason := osAvailable()
	settings := m.state.Settings
	settings.OSAuto = false
	send(w, 200, map[string]any{"ok": true, "user": user, "csrf": p.CSRF, "setup_required": len(m.state.Users) == 0, "settings": settings, "runtime": map[string]any{"os": runtime.GOOS, "server": serverName(), "os_login_available": available, "os_login_reason": reason, "sso_available": false, "secure": Secure(r), "https": HTTPS(r)}})
}
func serverName() string {
	n, e := os.Hostname()
	if e != nil {
		return "SpawnPoint"
	}
	return n
}

type loginInput struct {
	Method     string `json:"method"`
	Login      string `json:"login"`
	Password   string `json:"password"`
	Name       string `json:"name"`
	SetupToken string `json:"setup_token"`
	Remember   bool   `json:"remember"`
}

func (m *Manager) login(w http.ResponseWriter, r *http.Request, setup bool) {
	var in loginInput
	if e := readJSON(w, r, &in); e != nil {
		fail(w, 400, e.Error())
		return
	}
	in.Login = strings.TrimSpace(in.Login)
	if len(in.Login) > 256 || len(in.Password) > 1024 {
		fail(w, 400, "입력값이 너무 깁니다")
		return
	}
	select {
	case m.authSlots <- struct{}{}:
		defer func() { <-m.authSlots }()
	default:
		fail(w, 429, "인증 요청이 많습니다. 잠시 후 다시 시도하세요")
		return
	}
	method := in.Method
	if setup {
		method = "setup"
	}
	if e := m.attempt(r, method, in.Login); e != nil {
		fail(w, 429, e.Error())
		return
	}
	if setup {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.state.Users) != 0 || !sameSecret(in.SetupToken, m.setupToken) {
			fail(w, 403, "초기 설정 토큰을 확인하세요")
			return
		}
		hash, e := hashPassword(in.Password)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		s, _ := cloneState(m.state)
		u := User{ID: newID("u-"), Login: in.Login, Name: in.Name, Role: "admin", Auth: "local", Status: "active", Groups: []string{}, Hash: hash, Version: 1, Last: "—"}
		if u.Name == "" {
			u.Name = u.Login
		}
		s.Users = append(s.Users, u)
		if e = m.commit(s, u.Login, "최초 관리자 생성", u.ID, "로컬 초기 설정 토큰 확인"); e != nil {
			fail(w, 400, e.Error())
			return
		}
		m.setupToken = ""
		_ = os.Remove(m.setupPath)
		p, e := m.issueSession(w, r, u, in.Remember)
		if e != nil {
			fail(w, 503, "계정은 생성되었으나 세션 생성에 실패했습니다. 로그인하세요")
			return
		}
		m.me(w, r, p)
		return
	}
	u, e := m.authenticate(r.Context(), in.Method, in.Login, in.Password)
	if e != nil {
		_ = m.audit("anonymous", "로그인 실패", "", "denied", "계정 인증 실패")
		fail(w, 401, errLogin.Error())
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.state.user(u.ID)
	if !active(current) || current.Version != u.Version {
		fail(w, 401, errLogin.Error())
		return
	}
	if e = m.audit(u.Login, "로그인", u.ID, "success", u.Auth); e != nil {
		fail(w, 503, "감사 기록 저장에 실패했습니다")
		return
	}
	p, e := m.issueSession(w, r, u, in.Remember)
	if e != nil {
		fail(w, 503, "세션을 생성하지 못했습니다")
		return
	}
	m.resetAttempt(in.Method, in.Login)
	m.me(w, r, p)
}
func (m *Manager) accountAction(w http.ResponseWriter, r *http.Request, p Principal) {
	if r.Method != "POST" || p.Guest() || p.Token != nil {
		fail(w, 403, "로그인 세션이 필요합니다")
		return
	}
	var in struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	if e := readJSON(w, r, &in); e != nil {
		fail(w, 400, e.Error())
		return
	}
	m.mu.RLock()
	u := m.currentPrincipal(p)
	if u == nil {
		m.mu.RUnlock()
		fail(w, 401, "세션이 만료되었습니다")
		return
	}
	copy := *u
	m.mu.RUnlock()
	if r.URL.Path == "/auth/heartbeat" {
		send(w, 200, map[string]any{"ok": true})
		return
	}
	if r.URL.Path == "/auth/revoke-sessions" {
		_, e := m.db.Exec("DELETE FROM access_session WHERE user_id=? AND digest<>?", copy.ID, p.Session)
		if e != nil {
			fail(w, 503, "세션을 종료하지 못했습니다")
			return
		}
		_ = m.audit(copy.Login, "다른 세션 종료", copy.ID, "success", "")
		send(w, 200, map[string]any{"ok": true})
		return
	}
	select {
	case m.authSlots <- struct{}{}:
		defer func() { <-m.authSlots }()
	default:
		fail(w, 429, "인증 요청이 많습니다")
		return
	}
	name := copy.Login
	if copy.Auth == "os" {
		name = copy.External
	}
	if e := m.attempt(r, copy.Auth, name); e != nil {
		fail(w, 429, e.Error())
		return
	}
	verified, e := m.authenticate(r.Context(), copy.Auth, name, in.Password)
	if e != nil || verified.ID != copy.ID {
		fail(w, 401, errLogin.Error())
		return
	}
	m.resetAttempt(copy.Auth, name)
	m.mu.Lock()
	defer m.mu.Unlock()
	u = m.currentPrincipal(p)
	if u == nil {
		fail(w, 401, "세션이 만료되었습니다")
		return
	}
	if r.URL.Path == "/auth/reauth" {
		_, e = m.db.Exec("UPDATE access_session SET auth_at=? WHERE digest=?", m.now().Unix(), p.Session)
		if e != nil {
			fail(w, 503, "재인증을 저장하지 못했습니다")
			return
		}
		send(w, 200, map[string]any{"ok": true})
		return
	}
	if u.Auth != "local" {
		fail(w, 400, "OS 계정 비밀번호는 운영체제에서 변경하세요")
		return
	}
	hash, e := hashPassword(in.NewPassword)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	s, _ := cloneState(m.state)
	next := s.user(u.ID)
	next.Hash = hash
	next.Version++
	if e = m.commit(s, u.Login, "비밀번호 변경", u.ID, "기존 세션 및 토큰 무효화"); e != nil {
		fail(w, 503, e.Error())
		return
	}
	p, e = m.issueSession(w, r, *next, false)
	if e != nil {
		clearCookie(w, r)
		fail(w, 503, "변경된 비밀번호로 다시 로그인하세요")
		return
	}
	m.me(w, r, p)
}
