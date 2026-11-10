package access

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/runas"
	"spawnpoint/internal/runner"
)

func managerFixture() State {
	s := fixturePolicy()
	s.Users = append(s.Users, User{ID: "manager", Login: "manager", Name: "Manager", Role: "manager", Auth: "local", Hash: "pbkdf2-sha256$test", Status: "active", Groups: []string{"dev"}, Version: 1})
	return s
}

func TestManagerLifecycleRoleMatrix(t *testing.T) {
	for _, role := range []string{"viewer", "reference", "developer", "manager", "admin"} {
		for level := 0; level <= 3; level++ {
			for _, create := range []bool{false, true} {
				for _, remove := range []bool{false, true} {
					for _, protected := range []bool{false, true} {
						for _, specific := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/%d/create=%t/remove=%t/protected=%t/specific=%t", role, level, create, remove, protected, specific), func(t *testing.T) {
								s := managerFixture()
								s.ProcessGroups[0].Rules["g:dev"] = Grant{Level: level, Create: create, Remove: remove}
								p := s.Processes["p"]
								p.Protected = protected
								s.Processes["p"] = p
								cap := evaluate(&s, s.user(role), "p", specific)
								wantCreate := role == "admin" || ((role == "developer" || role == "manager") && level > 0 && create)
								wantRemove := role == "admin" || (role == "manager" && level > 0 && remove && !protected && !specific)
								if cap.Create != wantCreate || cap.Remove != wantRemove {
									t.Fatalf("cap=%+v want create=%t remove=%t", cap, wantCreate, wantRemove)
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestManagerDenyOverridesAndTokenBoundaries(t *testing.T) {
	s := managerFixture()
	g := &s.ProcessGroups[0]
	g.Rules["g:dev"] = Grant{Level: 3, Create: true, Remove: true}
	u := s.user("manager")
	if !evaluate(&s, u, "p", false).Remove {
		t.Fatal("explicit group deletion not granted")
	}
	u.Groups = append(u.Groups, "deny")
	g.Rules["g:deny"] = Grant{Level: 0}
	if evaluate(&s, u, "p", false).Remove {
		t.Fatal("group deny lost")
	}
	g.Rules["u:manager"] = Grant{Level: 1, Remove: true}
	if !evaluate(&s, u, "p", false).Remove || evaluate(&s, u, "p", false).Edit {
		t.Fatal("explicit lifecycle permission is independent of edit")
	}
	meta := s.Processes["p"]
	meta.Overrides["u:manager"] = Grant{Level: 3}
	s.Processes["p"] = meta
	if evaluate(&s, u, "p", false).Remove {
		t.Fatal("process exception did not remove inherited deletion")
	}
	delete(meta.Overrides, "u:manager")
	s.Processes["p"] = meta
	if !evaluate(&s, u, "p", false).Remove {
		t.Fatal("deleting exception did not restore inheritance")
	}
	m := &Manager{state: s, now: time.Now}
	for _, owner := range []string{"manager", "admin"} {
		p := Principal{UserID: owner, Token: &Token{Scope: "pg-default", Level: 3, Create: true}}
		if m.capabilities(p, "p", false).Remove {
			t.Fatal("token unexpectedly gained delete permission")
		}
	}
	delete(g.Rules, "u:manager")
	delete(g.Rules, "g:deny")
	p := Principal{UserID: "manager", Token: &Token{Scope: "pg-default", Level: 3, Create: true}}
	if !m.mayCreate(p, "pg-default") {
		t.Fatal("manager token lost existing scoped create contract")
	}
	if m.mayCreate(p, "other") {
		t.Fatal("manager escaped token scope")
	}
}

func TestLegacyGrantsDefaultDeleteOff(t *testing.T) {
	s := managerFixture()
	var g Grant
	if err := json.Unmarshal([]byte(`{"level":3,"create":true}`), &g); err != nil {
		t.Fatal(err)
	}
	if g.Remove {
		t.Fatal("old policies received delete access")
	}
	s.ProcessGroups[0].Rules["g:dev"] = g
	if evaluate(&s, s.user("manager"), "p", false).Remove {
		t.Fatal("legacy grant allows manager deletion")
	}
	if err := validateState(&s); err != nil {
		t.Fatal(err)
	}
	s.user("admin").Status = "paused"
	if err := validateState(&s); err == nil {
		t.Fatal("manager was treated as replacement for last administrator")
	}
	s.user("admin").Status = "active"
	s.ProcessGroups[0].Rules["g:dev"] = Grant{Level: 0, Remove: true}
	if err := validateState(&s); err == nil {
		t.Fatal("deny plus delete grant accepted")
	}
}

func TestManagerDeleteEnforcedBeforeRunnerSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name, role                                   string
		grant, protected, specific, stale, persisted bool
		want                                         int
	}{
		{"allowed", "manager", true, false, false, false, true, 200},
		{"no_grant", "manager", false, false, false, false, true, 403},
		{"protected", "manager", true, true, false, false, true, 403},
		{"execution_identity", "manager", true, false, true, false, true, 403},
		{"developer", "developer", true, false, false, false, true, 403},
		{"reference", "reference", true, false, false, false, true, 403},
		{"viewer", "viewer", true, false, false, false, true, 403},
		{"stale_session", "manager", true, false, false, true, true, 403},
		{"admin_protected", "admin", false, true, true, false, true, 200},
		{"persistence_failure", "manager", true, false, false, false, false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := bootstrapFixture(t)
			s := managerFixture()
			s.ProcessGroups[0].Rules["g:dev"] = Grant{Level: 3, Create: true, Remove: tc.grant}
			meta := s.Processes["p"]
			meta.Protected = tc.protected
			s.Processes["p"] = meta
			if err := m.commit(s, "test", "fixture", "p", ""); err != nil {
				t.Fatal(err)
			}
			info := runner.Info{ID: "p", Label: "worker", Cmd: "echo safe"}
			if tc.specific {
				info.RunAs = &runas.Summary{Mode: "user", Username: "target"}
			}
			m.runner = &groupEditRunner{info: info}
			p, err := m.issueSession(httptest.NewRecorder(), httptest.NewRequest("GET", "http://localhost/auth/me", nil), *m.state.user(tc.role), false)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stale {
				m.state.user(tc.role).Version++
			}
			calls := 0
			m.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				send(w, 200, map[string]any{"ok": true, "persisted": tc.persisted})
			})
			w := httptest.NewRecorder()
			m.deleteProcess(w, httptest.NewRequest("DELETE", "http://localhost/processes/p", nil), p, "p")
			if w.Code != tc.want {
				t.Fatalf("code=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
			if (tc.want == 403 && calls != 0) || (tc.want != 403 && calls != 1) {
				t.Fatalf("unauthorized deletion delegated: %d", calls)
			}
			_, exists := m.state.Processes["p"]
			if exists == (tc.want == 200) {
				t.Fatal("metadata removed without durable success")
			}
		})
	}
}

func TestDeleteGrantIncludedInImpactAndPersistence(t *testing.T) {
	m, path := bootstrapFixture(t)
	before := managerFixture()
	before.ProcessGroups[0].Rules["g:dev"] = Grant{Level: 3, Create: true}
	after, err := cloneState(before)
	if err != nil {
		t.Fatal(err)
	}
	after.ProcessGroups[0].Rules["g:dev"] = Grant{Level: 3, Create: true, Remove: true}
	m.runner = &groupEditRunner{info: runner.Info{ID: "p", Label: "worker"}}
	impact := m.impact(&before, &after)
	raw, _ := json.Marshal(impact)
	if impact["gained"].(int) != 1 || !strings.Contains(string(raw), "삭제") {
		t.Fatalf("missing delete impact: %s", raw)
	}
	if err = m.commit(after, "test", "fixture", "p", ""); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(m.db, m.runner, http.NotFoundHandler(), nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if !evaluate(&loaded.state, loaded.state.user("manager"), "p", false).Remove {
		t.Fatal("manager or delete grant lost after reopen")
	}
}
