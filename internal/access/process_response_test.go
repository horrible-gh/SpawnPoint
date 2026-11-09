package access

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreatedProcessUsesCanonicalResponseAndRedaction(t *testing.T) {
	for _, role := range []string{"developer", "viewer"} {
		s := fixturePolicy()
		s.ProcessGroups[0].Rules = map[string]Grant{"g:dev": {3, true}}
		m := &Manager{state: s, next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/processes/p" || r.URL.RawQuery != "" || r.ContentLength != 0 || !Managed(r) {
				t.Errorf("incorrect canonical read: %s %s", r.Method, r.URL)
			}
			send(w, 200, map[string]any{"ok": true, "process": map[string]any{
				"id": "p", "label": "worker", "cmd": "echo ok", "cwd": nil,
				"env": map[string]string{"SECRET": "hidden"}, "auto_start": true,
				"status": "stopped", "pid": nil, "exit_code": nil, "started_at": nil, "ended_at": nil,
			}})
		})}
		r := httptest.NewRequest("POST", "/processes?ignored=1", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		m.sendCreatedProcess(w, r, Principal{UserID: role, Version: 1}, "p")
		if w.Code != 200 {
			t.Fatalf("creation response status=%d", w.Code)
		}
		var response map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		row := response["process"].(map[string]any)
		for _, key := range []string{"id", "label", "cmd", "cwd", "auto_start", "pid", "status", "exit_code", "started_at", "ended_at", "group_id", "capabilities"} {
			if _, ok := row[key]; !ok {
				t.Errorf("%s response lost %s", role, key)
			}
		}
		_, hasEnv := row["env"]
		if hasEnv != (role == "developer") || response["persisted"] != true {
			t.Fatalf("wrong redaction/persistence: %s", w.Body.String())
		}
		if r.Method != "POST" || r.URL.Path != "/processes" || r.URL.RawQuery != "ignored=1" {
			t.Fatal("internal read mutated original request")
		}
	}
}
