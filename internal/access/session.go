package access

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"spawnpoint/internal/requesttransport"
)

const cookieName = "sp_session"

var errLogin = errors.New("계정 또는 비밀번호를 확인하세요")

func localRequest(r *http.Request) bool { return requesttransport.DirectLocal(r) }
func HTTPS(r *http.Request) bool        { return requesttransport.HTTPS(r) }
func Secure(r *http.Request) bool       { return requesttransport.Secure(r) }
func sameOrigin(r *http.Request) bool   { return requesttransport.SameOrigin(r) }
func bearer(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "", false
	}
	a, b, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(a, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(b), true
}
func (m *Manager) principal(r *http.Request) (Principal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if raw, present := bearer(r); present {
		if !Secure(r) {
			return Principal{}, errLogin
		}
		hash := digest(raw)
		for _, t := range m.state.Tokens {
			if sameSecret(t.Hash, hash) {
				u := m.state.user(t.Owner)
				expiry, e := time.Parse("2006-01-02", t.Expiry)
				if !active(u) || u.Version != t.Version || e != nil || !m.now().Before(expiry.Add(24*time.Hour)) {
					return Principal{}, errLogin
				}
				copy := t
				return Principal{UserID: u.ID, Version: u.Version, Token: &copy}, nil
			}
		}
		return Principal{}, errLogin // Invalid bearer credentials never fall back to a cookie.
	}
	c, e := r.Cookie(cookieName)
	if errors.Is(e, http.ErrNoCookie) {
		return Principal{}, nil
	}
	if e != nil || c.Value == "" || len(c.Value) > 256 || !Secure(r) {
		return Principal{}, errLogin
	}
	p := Principal{}
	now := m.now().Unix()
	var last, expires int64
	var remembered bool
	hash := digest(c.Value)
	// digest is the stable session row ID; only token_digest (or its brief
	// overlap predecessor) is a bearer credential after a token rotation.
	e = m.db.QueryRow("SELECT digest,user_id,user_version,csrf,last_seen,expires_at,auth_at,remembered FROM access_session WHERE token_digest=? OR (previous_digest=? AND previous_until>?)", hash, hash, now).Scan(&p.Session, &p.UserID, &p.Version, &p.CSRF, &last, &expires, &p.AuthAt, &remembered)
	if errors.Is(e, sql.ErrNoRows) {
		return Principal{}, errLogin
	}
	if e != nil {
		return Principal{}, e
	}
	idle, _ := strconv.Atoi(m.state.Settings.Session)
	u := m.state.user(p.UserID)
	if !active(u) || u.Version != p.Version || now >= expires || (!remembered && now-last >= int64(idle*60)) {
		return Principal{}, errLogin
	}
	if (u.Auth == "local" && !m.state.Settings.LocalLogin) || (u.Auth == "os" && !m.state.Settings.OSLogin) {
		return Principal{}, errLogin
	}
	// Background status/log polling does not extend idle sessions indefinitely.
	if r.Method != "GET" && r.Method != "HEAD" || r.URL.Path == "/auth/me" {
		if _, e = m.db.Exec("UPDATE access_session SET last_seen=? WHERE digest=?", now, p.Session); e != nil {
			return Principal{}, e
		}
	}
	return p, nil
}
func (m *Manager) currentPrincipal(p Principal) *User {
	u := m.state.user(p.UserID)
	if !active(u) || u.Version != p.Version || (u.Auth == "local" && !m.state.Settings.LocalLogin) || (u.Auth == "os" && !m.state.Settings.OSLogin) {
		return nil
	}
	if p.Token != nil {
		found := false
		for _, t := range m.state.Tokens {
			if t.ID == p.Token.ID && sameSecret(t.Hash, p.Token.Hash) && t.Version == u.Version {
				expiry, e := time.Parse("2006-01-02", t.Expiry)
				found = e == nil && m.now().Before(expiry.Add(24*time.Hour))
				break
			}
		}
		if !found {
			return nil
		}
	} else if p.Session != "" {
		idle, _ := strconv.Atoi(m.state.Settings.Session)
		var exists int
		e := m.db.QueryRow("SELECT 1 FROM access_session WHERE digest=? AND user_id=? AND user_version=? AND expires_at>? AND (remembered=1 OR last_seen>?)", p.Session, u.ID, u.Version, m.now().Unix(), m.now().Unix()-int64(idle*60)).Scan(&exists)
		if e != nil {
			return nil
		}
	} else {
		return nil
	}
	return u
}
func (m *Manager) issueSession(w http.ResponseWriter, r *http.Request, u User, remember bool) (Principal, error) {
	token, e := randomToken()
	if e != nil {
		return Principal{}, e
	}
	csrf, e := randomToken()
	if e != nil {
		return Principal{}, e
	}
	now := m.now().Unix()
	expiry := now + 12*3600
	if remember {
		expiry = now + rememberedSessionLifetime
	}
	p := Principal{UserID: u.ID, Version: u.Version, Session: digest(token), CSRF: csrf, AuthAt: now}
	tx, e := m.db.Begin()
	if e != nil {
		return Principal{}, e
	}
	defer tx.Rollback()
	_, e = tx.Exec("INSERT INTO access_session(digest,user_id,user_version,csrf,created_at,last_seen,expires_at,auth_at,remembered,token_digest,rotated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", p.Session, u.ID, u.Version, csrf, now, now, expiry, now, remember, digest(token), now)
	if e != nil {
		return Principal{}, e
	}
	if old, e := r.Cookie(cookieName); e == nil {
		hash := digest(old.Value)
		if _, e = tx.Exec("DELETE FROM access_session WHERE token_digest=? OR (previous_digest=? AND previous_until>?)", hash, hash, now); e != nil {
			return Principal{}, e
		}
	}
	_, e = tx.Exec("DELETE FROM access_session WHERE expires_at<=? OR (user_id=? AND digest NOT IN (SELECT digest FROM access_session WHERE user_id=? ORDER BY created_at DESC,rowid DESC LIMIT 10))", now, u.ID, u.ID)
	if e != nil {
		return Principal{}, e
	}
	if e = tx.Commit(); e != nil {
		return Principal{}, e
	}
	setSessionCookie(w, r, token, remember, expiry, now)
	return p, nil
}
func clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: HTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func csrfOK(r *http.Request, p Principal) bool {
	return p.Token != nil || (!p.Guest() && sameSecret(p.CSRF, r.Header.Get("X-CSRF-Token")))
}

// Bounded, persistent limits use the actual TCP peer, not a spoofable forwarding
// header. They remain mandatory even if a stale client submits lockout=false.
func (m *Manager) attempt(r *http.Request, method, login string) error {
	m.rateMu.Lock()
	defer m.rateMu.Unlock()
	peer, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		peer = r.RemoteAddr
	}
	keys := []string{digest("ip:" + peer), digest("account:" + method + ":" + strings.ToLower(login))}
	now := m.now().Unix()
	tx, e := m.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for i, k := range keys {
		var count int
		var start int64
		e = tx.QueryRow("SELECT count,started FROM access_attempt WHERE key=?", k).Scan(&count, &start)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if now-start >= 300 {
			count = 0
			start = now
		}
		limit := 50
		if i == 1 {
			limit = 5
		}
		if count >= limit {
			return errors.New("로그인 시도가 많습니다. 5분 후 다시 시도하세요")
		}
		_, e = tx.Exec("INSERT INTO access_attempt(key,count,started) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET count=excluded.count,started=excluded.started", k, count+1, start)
		if e != nil {
			return e
		}
	}
	_, e = tx.Exec("DELETE FROM access_attempt WHERE started<?", now-3600)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (m *Manager) resetAttempt(method, login string) {
	m.rateMu.Lock()
	defer m.rateMu.Unlock()
	_, _ = m.db.Exec("DELETE FROM access_attempt WHERE key=?", digest("account:"+method+":"+strings.ToLower(login)))
}
func (m *Manager) authenticate(ctx context.Context, method, login, password string) (User, error) {
	if login == "" || len(login) > 256 || password == "" || len(password) > 1024 {
		return User{}, errLogin
	}
	m.mu.RLock()
	enabled := method == "local" && m.state.Settings.LocalLogin || method == "os" && m.state.Settings.OSLogin
	users := append([]User(nil), m.state.Users...)
	m.mu.RUnlock()
	if !enabled {
		return User{}, errLogin
	}
	var candidate *User
	if method == "local" {
		for i := range users {
			if users[i].Auth == "local" && strings.EqualFold(users[i].Login, login) {
				candidate = &users[i]
				break
			}
		}
		if candidate == nil {
			dummyPassword(password)
			return User{}, errLogin
		}
		if !checkPassword(candidate.Hash, password) || !active(candidate) {
			return User{}, errLogin
		}
	} else {
		subject, e := m.resolveOS(login)
		if e != nil {
			dummyPassword(password)
			return User{}, errLogin
		}
		for i := range users {
			if users[i].Auth == "os" && users[i].Subject == subject && active(&users[i]) {
				candidate = &users[i]
				break
			}
		}
		if candidate == nil {
			dummyPassword(password)
			return User{}, errLogin
		}
		verified, e := m.verifyOS(ctx, login, password)
		if e != nil || verified != candidate.Subject {
			return User{}, errLogin
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.state.user(candidate.ID)
	if !active(current) || current.Version != candidate.Version || current.Auth != candidate.Auth {
		return User{}, errLogin
	}
	return *current, nil
}
