package access

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"spawnpoint/internal/runner"
)

type processOrderRunner struct {
	groupEditRunner
	items []runner.Info
}

func (r *processOrderRunner) List() []runner.Info { return r.items }
func (r *processOrderRunner) Get(id string) (runner.Info, bool) {
	for _, item := range r.items {
		if item.ID == id {
			return item, true
		}
	}
	return runner.Info{}, false
}

func processOrderFixture(t *testing.T) (*Manager, Principal) {
	t.Helper()
	m, _ := bootstrapFixture(t)
	state := fixturePolicy()
	state.Processes["q"] = ProcessMeta{PG: "pg-default", Overrides: map[string]Grant{}}
	protected := state.Processes["p"]
	protected.Protected = true
	protected.Overrides = map[string]Grant{"u:viewer": {Level: 1}}
	state.Processes["p"] = protected
	if err := m.commit(state, "test", "fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	m.runner = &processOrderRunner{items: []runner.Info{
		{ID: "p", Label: "first", Cmd: "echo first"},
		{ID: "q", Label: "second", Cmd: "echo second"},
	}}
	principal, err := m.issueSession(httptest.NewRecorder(), httptest.NewRequest("GET", "http://localhost/auth/me", nil), *m.state.user("admin"), false)
	if err != nil {
		t.Fatal(err)
	}
	return m, principal
}

func TestProcessOrderPersistsWithoutChangingCommandsOrPermissions(t *testing.T) {
	m, principal := processOrderFixture(t)
	revision := m.revision
	body, _ := json.Marshal(AdminInput{Action: "process.order", Revision: revision, IDs: []string{"q", "p"}})
	req := httptest.NewRequest("POST", "http://localhost/api/access/admin", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.adminAction(w, req, principal, false)
	if w.Code != 200 {
		t.Fatalf("reorder status=%d body=%s", w.Code, w.Body.String())
	}
	if m.revision != revision+1 || m.state.meta("q").SortOrder != 1 || m.state.meta("p").SortOrder != 2 {
		t.Fatalf("invalid persisted order: revision=%d, q=%+v, p=%+v", m.revision, m.state.meta("q"), m.state.meta("p"))
	}
	if !m.state.meta("p").Protected || m.state.meta("p").Overrides["u:viewer"].Level != 1 {
		t.Fatalf("process protections changed: %+v", m.state.meta("p"))
	}
	if info, ok := m.runner.Get("p"); !ok || info.Cmd != "echo first" {
		t.Fatal("display reorder changed runner commands")
	}
	snapshot, err := m.snapshot(principal)
	if err != nil {
		t.Fatal(err)
	}
	rows := snapshot["db"].(map[string]any)["processes"].([]map[string]any)
	got := []string{rows[0]["id"].(string), rows[1]["id"].(string)}
	if !reflect.DeepEqual(got, []string{"q", "p"}) {
		t.Fatalf("snapshot display order=%v", got)
	}
	var raw string
	if err := m.db.QueryRow("SELECT document FROM access_state WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var persisted State
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.meta("q").SortOrder != 1 || persisted.meta("p").SortOrder != 2 {
		t.Fatal("order was not stored durably")
	}
}

func TestProcessOrderRejectsInvalidListsAndNonAdmins(t *testing.T) {
	m, principal := processOrderFixture(t)
	for _, ids := range [][]string{{"p"}, {"p", "p"}, {"p", "unknown"}} {
		before := m.revision
		body, _ := json.Marshal(AdminInput{Action: "process.order", Revision: before, IDs: ids})
		req := httptest.NewRequest("POST", "http://localhost/api/access/admin", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		m.adminAction(w, req, principal, false)
		if w.Code != 400 || m.revision != before || m.state.meta("p").SortOrder != 0 {
			t.Fatalf("invalid ids=%v accepted: code=%d revision=%d body=%s", ids, w.Code, m.revision, w.Body.String())
		}
	}
	dev, err := m.issueSession(httptest.NewRecorder(), httptest.NewRequest("GET", "http://localhost/auth/me", nil), *m.state.user("developer"), false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(AdminInput{Action: "process.order", Revision: m.revision, IDs: []string{"q", "p"}})
	req := httptest.NewRequest("POST", "http://localhost/api/access/admin", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.adminAction(w, req, dev, false)
	if w.Code != 403 {
		t.Fatalf("non-admin reordering was permitted: %d %s", w.Code, w.Body.String())
	}
}
