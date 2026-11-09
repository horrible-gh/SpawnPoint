package access

import (
	"fmt"
	"testing"
	"time"
)

func fixturePolicy() State {
	s := defaultState()
	s.UserGroups = []UserGroup{{ID: "dev", Name: "개발"}, {ID: "deny", Name: "거부"}}
	for _, role := range []string{"viewer", "reference", "developer", "admin"} {
		s.Users = append(s.Users, User{ID: role, Login: role, Name: role, Role: role, Auth: "local", Hash: "pbkdf2-sha256$test", Status: "active", Groups: []string{"dev"}, Version: 1})
	}
	s.Processes["p"] = ProcessMeta{PG: "pg-default", Overrides: map[string]Grant{}}
	return s
}
func TestRolePermissionMatrix(t *testing.T) {
	for _, role := range []string{"viewer", "reference", "developer", "admin"} {
		for level := 0; level <= 3; level++ {
			for _, protect := range []bool{false, true} {
				for _, identity := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%d/protect=%t/identity=%t", role, level, protect, identity), func(t *testing.T) {
						s := fixturePolicy()
						s.ProcessGroups[0].Rules = map[string]Grant{"g:dev": {level, true}}
						p := s.Processes["p"]
						p.Protected = protect
						s.Processes["p"] = p
						c := evaluate(&s, s.user(role), "p", identity)
						top := level
						if role == "viewer" && top > 1 {
							top = 1
						}
						wantView := top >= 1
						wantRun := top >= 2
						wantEdit := top >= 3 && !protect && !identity
						wantCreate := role == "developer" && level > 0
						if role == "admin" {
							wantView = true
							wantRun = true
							wantEdit = true
							wantCreate = true
						}
						if c.View != wantView || c.Run != wantRun || c.Edit != wantEdit || c.Create != wantCreate || c.Remove != (role == "admin") {
							t.Fatalf("capabilities=%+v", c)
						}
					})
				}
			}
		}
	}
}
func TestInheritanceExceptionsAndDeny(t *testing.T) {
	s := fixturePolicy()
	g := &s.ProcessGroups[0]
	g.Rules = map[string]Grant{"g:dev": {3, true}, "g:deny": {0, false}}
	u := s.user("developer")
	u.Groups = append(u.Groups, "deny")
	if evaluate(&s, u, "p", false).View {
		t.Fatal("group deny lost")
	}
	g.Rules["u:developer"] = Grant{2, false}
	if !evaluate(&s, u, "p", false).Run {
		t.Fatal("direct grant precedence")
	}
	p := s.Processes["p"]
	p.Overrides = map[string]Grant{"u:developer": {0, false}}
	s.Processes["p"] = p
	if evaluate(&s, u, "p", false).View {
		t.Fatal("process deny lost")
	}
	p.Overrides = map[string]Grant{}
	s.Processes["p"] = p
	if !evaluate(&s, u, "p", false).Run {
		t.Fatal("inheritance not restored")
	}
}
func TestGuestCannotMutateAndPrivateDefault(t *testing.T) {
	s := fixturePolicy()
	if evaluate(&s, nil, "p", false).View {
		t.Fatal("default is public")
	}
	s.Settings.Guest = true
	s.ProcessGroups[0].Guest = true
	c := evaluate(&s, nil, "p", true)
	if !c.View || c.Logs || c.Run || c.Edit || c.Create || c.Remove {
		t.Fatalf("guest=%+v", c)
	}
	s.Settings.GuestLogs = true
	if !evaluate(&s, nil, "p", false).Logs {
		t.Fatal("explicit public logs not allowed")
	}
	no := false
	p := s.Processes["p"]
	p.Guest = &no
	s.Processes["p"] = p
	if evaluate(&s, nil, "p", false).View {
		t.Fatal("private exception ignored")
	}
}
func TestTokenCannotEditProtectedOrEscapeScope(t *testing.T) {
	s := fixturePolicy()
	m := &Manager{state: s, now: time.Now}
	p := Principal{UserID: "admin", Version: 1, Token: &Token{Scope: "pg-default", Level: 3, Create: true}}
	if m.capabilities(p, "p", true).Edit {
		t.Fatal("token changed privileged command")
	}
	meta := s.Processes["p"]
	meta.Protected = true
	m.state.Processes["p"] = meta
	if m.capabilities(p, "p", false).Edit {
		t.Fatal("token ignored protection")
	}
	p.Token.Scope = "another"
	if m.capabilities(p, "p", false).View {
		t.Fatal("token escaped scope")
	}
}
func TestPasswordHashing(t *testing.T) {
	password := "a long test password 123!"
	hash, e := hashPassword(password)
	if e != nil {
		t.Fatal(e)
	}
	if hash == password || !checkPassword(hash, password) || checkPassword(hash, password+"x") {
		t.Fatal("hash verification")
	}
	second, _ := hashPassword(password)
	if hash == second {
		t.Fatal("salt reused")
	}
	if _, e = hashPassword("too-short"); e == nil {
		t.Fatal("short password accepted")
	}
	if checkPassword("pbkdf2-sha256$999999999999$salt$key", password) {
		t.Fatal("unbounded iteration accepted")
	}
}
func TestStateRejectsLastAdminAndInvalidRules(t *testing.T) {
	s := fixturePolicy()
	if e := validateState(&s); e != nil {
		t.Fatal(e)
	}
	s.user("admin").Status = "paused"
	if e := validateState(&s); e == nil {
		t.Fatal("last admin disabled")
	}
	s.user("admin").Status = "active"
	s.ProcessGroups[0].Rules["u:missing"] = Grant{3, true}
	if e := validateState(&s); e == nil {
		t.Fatal("unknown subject allowed")
	}
}
