// Package access owns application identities, sessions and process authorization.
// OS login identities and OS process execution credentials are deliberately separate.
package access

import (
	"context"
	"database/sql"
	"net/http"
	"spawnpoint/internal/runas"
	"spawnpoint/internal/runner"
	"sync"
	"time"
)

type User struct {
	ID       string   `json:"id"`
	Login    string   `json:"login"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Groups   []string `json:"groups"`
	Auth     string   `json:"auth"`
	Status   string   `json:"status"`
	External string   `json:"external"`
	Subject  string   `json:"subject,omitempty"`
	Hash     string   `json:"hash,omitempty"` // persistence only; never returned by Snapshot
	Version  int      `json:"version"`
	Last     string   `json:"last"`
}
type UserGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	Icon string `json:"icon"`
}
type Grant struct {
	Level  int  `json:"level"`
	Create bool `json:"create"`
	Remove bool `json:"remove,omitempty"` // additive; absent in older saved policies means false
}
type ProcessGroup struct {
	ID    string           `json:"id"`
	Name  string           `json:"name"`
	Desc  string           `json:"desc"`
	Rules map[string]Grant `json:"rules"`
	Guest bool             `json:"guest"`
}
type ProcessMeta struct {
	PG        string           `json:"pg"`
	Overrides map[string]Grant `json:"overrides"`
	Protected bool             `json:"protected"`
	Guest     *bool            `json:"guest"`
}
type Settings struct {
	OSLogin    bool   `json:"osLogin"`
	LocalLogin bool   `json:"localLogin"`
	OSAuto     bool   `json:"osAuto"`
	Guest      bool   `json:"guest"`
	GuestLogs  bool   `json:"guestLogs"`
	Session    string `json:"session"`
	Reauth     bool   `json:"reauth"`
	Lockout    bool   `json:"lockout"`
}
type Token struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	Scope   string `json:"scope"`
	Level   int    `json:"level"`
	Create  bool   `json:"create"`
	Expiry  string `json:"expiry"`
	Last    string `json:"last"`
	Hash    string `json:"hash,omitempty"`
	Version int    `json:"version"`
}
type State struct {
	Users         []User                 `json:"users"`
	UserGroups    []UserGroup            `json:"userGroups"`
	ProcessGroups []ProcessGroup         `json:"processGroups"`
	Processes     map[string]ProcessMeta `json:"processMeta"`
	Settings      Settings               `json:"settings"`
	Tokens        []Token                `json:"tokens"`
}
type Capabilities struct {
	Level  int    `json:"level"`
	View   bool   `json:"view"`
	Logs   bool   `json:"logs"`
	Run    bool   `json:"run"`
	Edit   bool   `json:"edit"`
	Create bool   `json:"create"`
	Remove bool   `json:"remove"`
	Raw    int    `json:"raw"`
	Source string `json:"source"`
}
type Principal struct {
	UserID  string
	Version int
	Session string
	CSRF    string
	AuthAt  int64
	Token   *Token
}

func (p Principal) Guest() bool { return p.UserID == "" }

type Runner interface {
	List() []runner.Info
	Get(string) (runner.Info, bool)
	RegisterOnly(string, string, *string, map[string]string, ...bool) (runner.Info, bool)
	Run(string) (runner.Info, bool)
	Delete(string) (bool, bool)
}
type identityRunner interface {
	RegisterIdentity(string, string, *string, map[string]string, runas.Input, bool, ...bool) (runner.Info, bool, error)
}
type Manager struct {
	mu          sync.RWMutex
	rateMu      sync.Mutex
	db          *sql.DB
	runner      Runner
	next        http.Handler
	ui          []byte
	state       State
	revision    int64
	setupToken  string
	setupPath   string
	setupError  string
	verifyAdmin func(context.Context, string, string) (string, error)
	authSlots   chan struct{}
	now         func() time.Time
	verifyOS    func(context.Context, string, string) (string, error)
	resolveOS   func(string) (string, error)
}
type contextKey struct{}

// Managed marks a request accepted by the outer authorization gateway, including
// explicitly allowed read-only public requests. This cannot be supplied via HTTP.
func Managed(r *http.Request) bool { _, ok := r.Context().Value(contextKey{}).(Principal); return ok }
func Authenticated(r *http.Request) bool {
	p, ok := r.Context().Value(contextKey{}).(Principal)
	return ok && !p.Guest()
}
func contextRequest(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), contextKey{}, p))
}
func defaultState() State {
	return State{Users: []User{}, UserGroups: []UserGroup{}, ProcessGroups: []ProcessGroup{{ID: "pg-default", Name: "기존 프로세스", Desc: "관리자가 그룹 권한을 부여하기 전까지 비공개", Rules: map[string]Grant{}}}, Processes: map[string]ProcessMeta{}, Settings: Settings{OSLogin: true, LocalLogin: true, Session: "60", Reauth: true, Lockout: true}, Tokens: []Token{}}
}
func (s *State) user(id string) *User {
	for i := range s.Users {
		if s.Users[i].ID == id {
			return &s.Users[i]
		}
	}
	return nil
}
func (s *State) group(id string) *ProcessGroup {
	for i := range s.ProcessGroups {
		if s.ProcessGroups[i].ID == id {
			return &s.ProcessGroups[i]
		}
	}
	return nil
}
func (s *State) meta(id string) ProcessMeta {
	if v, ok := s.Processes[id]; ok {
		return v
	}
	return ProcessMeta{PG: "pg-default", Overrides: map[string]Grant{}}
}
func active(u *User) bool { return u != nil && u.Status == "active" }
func admin(u *User) bool  { return active(u) && u.Role == "admin" }
func validRole(role string) bool {
	return role == "admin" || role == "manager" || role == "developer" || role == "reference" || role == "viewer"
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
