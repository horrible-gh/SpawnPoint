package access

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// SetupInfo contains diagnostics, never the setup secret. Only direct loopback
// clients may read paths. A CLI authenticates the server with a fresh challenge
// before it sends a new account password; the token itself is never sent by CLI.
type SetupInfo struct {
	Required    bool   `json:"setup_required"`
	DBPath      string `json:"db_path"`
	TokenPath   string `json:"token_path"`
	TokenSource string `json:"token_source"`
	TokenError  string `json:"token_error,omitempty"`
	Proof       string `json:"server_proof,omitempty"`
}
type BootstrapAccount struct {
	Login    string `json:"login"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Nonce    string `json:"nonce"`
	Proof    string `json:"proof,omitempty"`
}

func setupMAC(token, purpose, value string) string {
	h := hmac.New(sha256.New, []byte(token))
	h.Write([]byte("spawnpoint-bootstrap-v1\x00" + purpose + "\x00" + value))
	return hex.EncodeToString(h.Sum(nil))
}
func SetupServerProof(token, nonce string) string { return setupMAC(token, "server", nonce) }
func SetupAccountProof(token string, account BootstrapAccount) string {
	account.Proof = ""
	b, _ := json.Marshal(account)
	return setupMAC(token, "create-local-admin", string(b))
}
func validSetupNonce(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32
}
func directLocal(r *http.Request) bool {
	return localRequest(r) && r.Header.Get("Forwarded") == "" && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Forwarded-Host") == "" && r.Header.Get("X-Forwarded-Proto") == ""
}
func (m *Manager) setupInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || !directLocal(r) || !sameOrigin(r) {
		fail(w, 403, "초기 설정 경로는 서버의 로컬 CLI에서 확인하세요")
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	tokenPath, e := filepath.Abs(m.setupPath)
	if e != nil {
		fail(w, 503, "초기 설정 경로를 확인하지 못했습니다")
		return
	}
	source := "file"
	if strings.TrimSpace(os.Getenv("SPAWNPOINT_SETUP_TOKEN")) != "" {
		source = "environment"
	}
	v := SetupInfo{Required: len(m.state.Users) == 0, DBPath: strings.TrimSuffix(tokenPath, ".setup-token"), TokenPath: tokenPath, TokenSource: source, TokenError: m.setupError}
	if nonce := r.URL.Query().Get("nonce"); v.Required && m.setupToken != "" && validSetupNonce(nonce) {
		v.Proof = SetupServerProof(m.setupToken, nonce)
	}
	send(w, 200, v)
}
func (m *Manager) initialAdmin(w http.ResponseWriter, r *http.Request, in loginInput) {
	isOS, isCLI := r.URL.Path == "/auth/setup-os", r.URL.Path == "/auth/setup-cli"
	if isCLI && !directLocal(r) {
		fail(w, 403, "CLI 초기 설정은 서버 로컬에서만 가능합니다")
		return
	}
	// Avoid contacting the OS authenticator after enrollment has closed.
	m.mu.RLock()
	enrolled := len(m.state.Users) != 0
	m.mu.RUnlock()
	if enrolled {
		fail(w, 403, "최초 관리자 설정이 이미 완료되었습니다. 일반 로그인을 이용하세요")
		return
	}
	subject := ""
	if isOS {
		var e error
		subject, e = m.verifyAdmin(r.Context(), in.Login, in.Password)
		if e != nil || subject == "" {
			_ = m.audit("anonymous", "최초 관리자 인증 실패", "", "denied", "OS 관리자 인증 거부")
			fail(w, 403, "OS 관리자 계정과 비밀번호를 확인하세요. 정책상 인증이 불가능하면 서버에서 admin init을 실행하세요")
			return
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// All bootstrap methods share this lock and the durable revision CAS.
	if len(m.state.Users) != 0 {
		fail(w, 403, "최초 관리자 설정이 이미 완료되었습니다")
		return
	}
	if !isOS {
		allowed := sameSecret(in.SetupToken, m.setupToken)
		if isCLI {
			a := BootstrapAccount{Login: in.Login, Name: in.Name, Password: in.Password, Nonce: in.Nonce}
			allowed = m.setupToken != "" && validSetupNonce(in.Nonce) && sameSecret(in.Proof, SetupAccountProof(m.setupToken, a))
		}
		if !allowed {
			fail(w, 403, "초기 설정 인증을 확인하세요")
			return
		}
	}
	u := User{ID: newID("u-"), Login: in.Login, Name: in.Name, Role: "admin", Auth: "local", Status: "active", Groups: []string{}, Version: 1, Last: "—"}
	detail := "로컬 초기 설정 토큰 확인"
	if isOS {
		u.Auth = "os"
		u.External = in.Login
		u.Subject = subject
		detail = "OS 인증 및 관리자 권한 확인"
	} else {
		hash, e := hashPassword(in.Password)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		u.Hash = hash
		if isCLI {
			detail = "서버 로컬 CLI 초기 설정"
		}
	}
	if u.Name == "" {
		u.Name = u.Login
	}
	s, e := cloneState(m.state)
	if e != nil {
		fail(w, 503, "초기 설정을 준비하지 못했습니다")
		return
	}
	s.Users = append(s.Users, u)
	if isOS {
		s.Settings.OSLogin = true
	} else {
		s.Settings.LocalLogin = true
	}
	if e = m.commit(s, u.Login, "최초 관리자 생성", u.ID, detail); e != nil {
		fail(w, 400, e.Error())
		return
	}
	m.setupToken = ""
	m.setupError = ""
	_ = os.Remove(m.setupPath)
	if isCLI {
		send(w, 200, map[string]any{"ok": true, "login": u.Login})
		return
	}
	p, e := m.issueSession(w, r, u, in.Remember)
	if e != nil {
		fail(w, 503, "계정이 생성되었습니다. 일반 로그인으로 접속하세요")
		return
	}
	m.me(w, r, p)
}

func setupLocalURL(r *http.Request) string {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, port, e := net.SplitHostPort(addr.String()); e == nil {
			return "http://" + net.JoinHostPort("127.0.0.1", port)
		}
	}
	return ""
}
