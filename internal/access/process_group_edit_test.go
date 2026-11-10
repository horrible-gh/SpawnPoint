package access

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spawnpoint/internal/runner"
)

type groupEditRunner struct{ info runner.Info }

func (r *groupEditRunner) List() []runner.Info               { return []runner.Info{r.info} }
func (r *groupEditRunner) Get(id string) (runner.Info, bool) { return r.info, id == r.info.ID }
func (r *groupEditRunner) RegisterOnly(string, string, *string, map[string]string, ...bool) (runner.Info, bool) {
	panic("unexpected create")
}
func (r *groupEditRunner) Run(string) (runner.Info, bool) { panic("save must not run") }
func (r *groupEditRunner) Delete(string) (bool, bool)     { panic("save must not delete") }

func TestEditorGroupMoveSaveFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		delegatedStatus         int
		persisted, rejectPolicy bool
		want                    int
	}{
		{"success", 200, true, false, 200},
		{"command_rejected", 400, false, false, 400},
		{"command_not_persisted", 200, false, false, 503},
		{"policy_storage_failure", 200, true, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := bootstrapFixture(t)
			state := fixturePolicy()
			state.ProcessGroups = append(state.ProcessGroups, ProcessGroup{ID: "destination", Name: "개발", Rules: map[string]Grant{}})
			visible := false
			meta := state.Processes["p"]
			meta.Guest = &visible
			meta.Overrides = map[string]Grant{"u:viewer": {Level: 1}}
			state.Processes["p"] = meta
			if err := m.commit(state, "test", "fixture", "p", ""); err != nil {
				t.Fatal(err)
			}
			m.runner = &groupEditRunner{info: runner.Info{ID: "p", Label: "worker", Cmd: "echo old"}}
			req := httptest.NewRequest("GET", "http://localhost/auth/me", nil)
			principal, err := m.issueSession(httptest.NewRecorder(), req, *m.state.user("admin"), false)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			m.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				send(w, tc.delegatedStatus, map[string]any{"ok": tc.delegatedStatus == 200, "persisted": tc.persisted, "process": map[string]any{"id": "p"}})
			})
			if tc.rejectPolicy {
				if _, err = m.db.Exec("CREATE TRIGGER reject_group_update BEFORE UPDATE ON access_state BEGIN SELECT RAISE(FAIL, 'test policy failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(map[string]any{"cmd": "echo new", "group_id": "destination", "revision": m.revision})
			r := httptest.NewRequest("PUT", "http://localhost/processes/p", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			m.editProcess(w, r, principal, "p")
			if w.Code != tc.want || calls != 1 {
				t.Fatalf("code=%d calls=%d response=%s", w.Code, calls, w.Body.String())
			}
			wantGroup := "pg-default"
			if tc.want == 200 {
				wantGroup = "destination"
			}
			got := m.state.meta("p")
			if got.PG != wantGroup || got.Guest == nil || *got.Guest || got.Overrides["u:viewer"].Level != 1 {
				t.Fatalf("metadata not preserved: %+v", got)
			}
			var raw string
			if err = m.db.QueryRow("SELECT document FROM access_state WHERE id=1").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var saved State
			if err = json.Unmarshal([]byte(raw), &saved); err != nil {
				t.Fatal(err)
			}
			if saved.meta("p").PG != wantGroup {
				t.Fatal("memory and durable policy differ")
			}
			if tc.rejectPolicy && !strings.Contains(w.Body.String(), "재시작하지 않았습니다") {
				t.Fatal("partial save was not reported")
			}
		})
	}
}

func TestEditorGroupMoveRequiresRecentAdministratorAuthentication(t *testing.T) {
	m, _ := bootstrapFixture(t)
	state := fixturePolicy()
	state.ProcessGroups = append(state.ProcessGroups, ProcessGroup{ID: "destination", Name: "개발", Rules: map[string]Grant{}})
	state.Settings.Reauth = true
	if err := m.commit(state, "test", "fixture", "p", ""); err != nil {
		t.Fatal(err)
	}
	m.runner = &groupEditRunner{info: runner.Info{ID: "p"}}
	principal, err := m.issueSession(httptest.NewRecorder(), httptest.NewRequest("GET", "http://localhost/auth/me", nil), *m.state.user("admin"), false)
	if err != nil {
		t.Fatal(err)
	}
	principal.AuthAt = m.now().Unix() - 601
	m.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("command changed before reauthentication") })
	body, _ := json.Marshal(map[string]any{"cmd": "echo new", "group_id": "destination", "revision": m.revision})
	r := httptest.NewRequest("PUT", "http://localhost/processes/p", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.editProcess(w, r, principal, "p")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "reauth_required") {
		t.Fatalf("bad reauthentication response %d %s", w.Code, w.Body.String())
	}
	if m.state.meta("p").PG != "pg-default" {
		t.Fatal("group changed before reauthentication")
	}
}
