package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"spawnpoint/internal/runner"
)

func TestAutoStartRegistrationIsIndependentOfImmediateStart(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, auto := range []bool{false, true} {
			t.Run(fmt.Sprintf("start=%v/auto=%v", start, auto), func(t *testing.T) {
				f := &fakeRunner{persisted: true}
				s := newTestServer(t, Options{Runner: f})
				status, body, _ := do(t, s, http.MethodPost, "/processes",
					fmt.Sprintf(`{"cmd":"echo hello","start":%t,"auto_start":%t}`, start, auto))
				var out writeResponse
				if err := json.Unmarshal([]byte(body), &out); err != nil {
					t.Fatal(err)
				}
				wantStatus := runner.StatusStopped
				if start {
					wantStatus = runner.StatusRunning
				}
				if status != http.StatusOK || !out.Persisted || out.Process.AutoStart != auto || out.Process.Status != wantStatus {
					t.Fatalf("unexpected response: %d %s", status, body)
				}
			})
		}
	}
}

func TestAutoStartUpdateOmissionPreservesAndFalseDisables(t *testing.T) {
	f := &fakeRunner{persisted: true, entries: []runner.Info{{
		ID: "proc_12345678", Label: "keep", Cmd: "echo hi", AutoStart: true, Status: runner.StatusRunning,
	}}}
	s := newTestServer(t, Options{Runner: f})
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"cmd":"echo changed"}`, true},
		{`{"cmd":"echo changed","auto_start":false}`, false},
		{`{"cmd":"echo changed","auto_start":true}`, true},
	} {
		status, body, _ := do(t, s, http.MethodPut, "/processes/proc_12345678", tc.body)
		var out writeResponse
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || out.Process.AutoStart != tc.want || out.Process.Status != runner.StatusRunning {
			t.Fatalf("unexpected update: %d %s", status, body)
		}
		status, body, _ = do(t, s, http.MethodGet, "/processes/proc_12345678", "")
		var got processResponse
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || got.Process.AutoStart != tc.want {
			t.Fatalf("preference missing from GET: %d %s", status, body)
		}
	}
	for _, call := range f.calls {
		if call == "run" || call == "stop" || call == "restart" {
			t.Fatalf("saving a preference performed %s", call)
		}
	}
}

func TestAutoStartRejectsNonBooleansBeforeMutation(t *testing.T) {
	for _, value := range []string{`null`, `"true"`, `0`, `1`, `{}`, `[]`} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			t.Run(method+"/"+value, func(t *testing.T) {
				f := &fakeRunner{persisted: true}
				s := newTestServer(t, Options{Runner: f})
				path := "/processes"
				if method == http.MethodPut {
					path += "/proc_12345678"
				}
				status, body, _ := do(t, s, method, path, `{"cmd":"echo hello","auto_start":`+value+`}`)
				if status != http.StatusBadRequest || len(f.calls) != 0 {
					t.Fatalf("invalid input reached runner: %d %s calls=%v", status, body, f.calls)
				}
				var out struct {
					Error struct {
						Field string `json:"field"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(body), &out); err != nil || out.Error.Field != "auto_start" {
					t.Fatalf("wrong field error: %s (%v)", body, err)
				}
			})
		}
	}
}
