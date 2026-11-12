package access

import (
	"database/sql"
	"errors"
	"net/http"
	"time"
)

const (
	rememberedSessionLifetime = int64(30 * 24 * 3600)
	sessionRenewalInterval    = int64(12 * 3600)
	sessionRenewalOverlap     = int64(60)
)

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, remembered bool, expires, now int64) {
	cookie := &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: HTTPS(r), SameSite: http.SameSiteStrictMode}
	if remembered {
		cookie.MaxAge = int(expires - now)
		cookie.Expires = time.Unix(expires, 0)
	}
	http.SetCookie(w, cookie)
}

// renewRememberedSession rotates on browser entry or a CSRF-checked user
// heartbeat, not background log/status polling. It never extends the 30-day
// absolute deadline or the password verification time (auth_at).
func (m *Manager) renewRememberedSession(w http.ResponseWriter, r *http.Request, p Principal) error {
	if p.Guest() || p.Token != nil || p.Session == "" {
		return nil
	}
	c, e := r.Cookie(cookieName)
	if e != nil || c.Value == "" || len(c.Value) > 256 || !Secure(r) {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.currentPrincipal(p) == nil {
		return nil
	}
	now := m.now().Unix()
	hash := digest(c.Value)
	var expires int64
	e = m.db.QueryRow("SELECT expires_at FROM access_session WHERE digest=? AND token_digest=? AND remembered=1 AND rotated_at<=? AND expires_at>?", p.Session, hash, now-sessionRenewalInterval, now).Scan(&expires)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	token, e := randomToken()
	if e != nil {
		return e
	}
	// Atomic compare-and-swap: exactly one concurrent tab wins. Losing
	// requests do not set/clear cookies or mint another successor. A brief
	// predecessor overlap lets already-in-flight requests finish, but an old
	// token cannot renew again. Only hashes are stored in SQLite.
	result, e := m.db.Exec("UPDATE access_session SET previous_digest=token_digest,previous_until=?,token_digest=?,rotated_at=? WHERE digest=? AND token_digest=? AND remembered=1 AND rotated_at<=? AND expires_at>?", now+sessionRenewalOverlap, digest(token), now, p.Session, hash, now-sessionRenewalInterval, now)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n == 1 {
		setSessionCookie(w, r, token, true, expires, now)
	}
	return nil
}
