//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/lifecycle"
)

// The request front end against the real executable, over a real socket.
//
// The package tests in internal/httpapi pin every response shape; what these
// add is that the shapes come out of a process that was started the way the
// service manager starts it, listening on a port it bound during the startup
// sequence, and that the whole thing still shuts down cleanly afterwards with a
// request in flight.
//
// Registering a command starts a process, so these run under the same opt-in as
// the rest of the live checks:
//
//	SPAWNPOINT_LIVE_SHUTDOWN=1 go test ./cmd/spawnpoint/

// get performs a request against the instance and returns the status and body.
func (i *instance) request(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", i.port, path)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return res.StatusCode, string(raw)
}

// waitForHealthz blocks until the server answers, which is the only way to know
// the listener is up without reading the log for it.
func (i *instance) waitForHealthz(t *testing.T, timeout time.Duration) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", i.port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server never answered /healthz within %v\nlog:\n%s\nstderr:\n%s",
		timeout, i.log(), i.stderr)
}

// A whole session against the real binary: the screen, liveness, an empty
// listing, a registration that runs, its log, and a delete that removes it.
func TestLiveRequestFrontEnd(t *testing.T) {
	requireLive(t)
	inst := start(t, freePort(t))
	inst.waitForHealthz(t, 20*time.Second)

	// The screen is served from the executable, on both paths.
	for _, path := range []string{"/", "/index.html"} {
		status, body := inst.request(t, http.MethodGet, path, "")
		if status != http.StatusOK || !strings.Contains(body, "<html") {
			t.Fatalf("GET %s: %d, %.60q", path, status, body)
		}
	}

	// Liveness carries the two fields the rewrite added.
	status, body := inst.request(t, http.MethodGet, "/healthz", "")
	var health struct {
		OK        bool   `json:"ok"`
		Status    string `json:"status"`
		Version   string `json:"version"`
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("healthz: %d %s — %v", status, body, err)
	}
	if !health.OK || health.Status != "healthy" || health.Version == "" || health.StartedAt == "" {
		t.Errorf("healthz: %+v", health)
	}

	// A fresh database has nothing registered.
	if status, body := inst.request(t, http.MethodGet, "/processes", ""); status != http.StatusOK ||
		!strings.Contains(body, `"processes":[]`) {
		t.Fatalf("GET /processes: %d %s", status, body)
	}

	// Issue an instance through the real database. The 256-character label is
	// the old schema mismatch: reaching 200 here proves migration 003 and the
	// request contract agree. Repeating the request key must return the same ID.
	spawnBody, err := json.Marshal(map[string]any{
		"requester":   "live-probe",
		"kind":        "worker",
		"request_key": "live-request-key",
		"options": map[string]any{
			"label":       strings.Repeat("가", 256),
			"ttl_seconds": 7200,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body = inst.request(t, http.MethodPost, "/spawn", string(spawnBody))
	var issued struct {
		OK           bool `json:"ok"`
		Deduplicated bool `json:"deduplicated"`
		Instance     struct {
			ID string `json:"id"`
		} `json:"instance"`
	}
	if err := json.Unmarshal([]byte(body), &issued); err != nil {
		t.Fatalf("decode issued instance: %v — %s", err, body)
	}
	if status != http.StatusOK || !issued.OK || issued.Deduplicated || !strings.HasPrefix(issued.Instance.ID, "spwn_") {
		t.Fatalf("POST /spawn: %d %+v — %s", status, issued, body)
	}
	firstID := issued.Instance.ID

	status, body = inst.request(t, http.MethodPost, "/spawn", string(spawnBody))
	if err := json.Unmarshal([]byte(body), &issued); err != nil {
		t.Fatalf("decode duplicate instance: %v — %s", err, body)
	}
	if status != http.StatusOK || !issued.Deduplicated || issued.Instance.ID != firstID {
		t.Fatalf("duplicate POST /spawn: %d %+v — %s", status, issued, body)
	}
	// Register something that writes a line and stops. The registration is
	// saved before the process starts, so `persisted` says the row is there.
	const marker = "spawnpoint-live-front-end"
	status, body = inst.request(t, http.MethodPost, "/processes",
		`{"label": "LiveProbe", "cmd": "cmd /c echo `+marker+`"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /processes: %d %s", status, body)
	}
	var reg struct {
		OK        bool `json:"ok"`
		Persisted bool `json:"persisted"`
		Process   struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Status string `json:"status"`
			PID    *int   `json:"pid"`
		} `json:"process"`
	}
	if err := json.Unmarshal([]byte(body), &reg); err != nil {
		t.Fatalf("decode the registration: %v — %s", err, body)
	}
	if !reg.OK || !reg.Persisted {
		t.Errorf("registration: ok=%v persisted=%v — %s", reg.OK, reg.Persisted, body)
	}
	if reg.Process.Label != "LiveProbe" || !strings.HasPrefix(reg.Process.ID, "proc_") {
		t.Errorf("process: %+v", reg.Process)
	}
	id := reg.Process.ID

	// The child's output reaches the log query, which is the collector, the
	// rotation writer and the reader all in one path.
	deadline := time.Now().Add(15 * time.Second)
	var logged string
	for time.Now().Before(deadline) {
		_, body := inst.request(t, http.MethodGet, "/processes/"+id+"/logs", "")
		var view struct {
			Text     string `json:"text"`
			Size     int64  `json:"size"`
			Encoding string `json:"encoding"`
		}
		json.Unmarshal([]byte(body), &view)
		if strings.Contains(view.Text, marker) {
			logged = view.Text
			if view.Encoding == "" {
				t.Errorf("the log answer carried no encoding: %s", body)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if logged == "" {
		t.Fatalf("the child's output never appeared in the log query")
	}

	// The listing now has it, and the registration outlives the run.
	if _, body := inst.request(t, http.MethodGet, "/processes", ""); !strings.Contains(body, id) {
		t.Errorf("the registered entry is not in the listing: %s", body)
	}

	// Delete removes the entry and its log file.
	if status, body := inst.request(t, http.MethodDelete, "/processes/"+id, ""); status != http.StatusOK ||
		!strings.Contains(body, `"deleted_id":"`+id+`"`) {
		t.Fatalf("DELETE: %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(inst.logDir, id+".log")); !os.IsNotExist(err) {
		t.Errorf("the child log survived the delete: %v", err)
	}
	if status, _ := inst.request(t, http.MethodGet, "/processes/"+id+"/logs", ""); status != http.StatusNotFound {
		t.Errorf("a deleted entry still answers log queries: %d", status)
	}
}

// The error paths over the wire. These are the ones a proxy or a client library
// is most likely to get wrong, so they are measured against the real stack
// rather than only against the handler.
func TestLiveErrorResponses(t *testing.T) {
	requireLive(t)
	inst := start(t, freePort(t))
	inst.waitForHealthz(t, 20*time.Second)

	cases := []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{http.MethodGet, "/api/v2/processes", "", http.StatusNotFound, "Unknown endpoint."},
		{http.MethodPost, "/processes/proc_00000000/stop", "", http.StatusNotFound, "Process not found."},
		{http.MethodPost, "/processes/proc_00000000/pause", "", http.StatusNotFound, "Unknown endpoint."},
		{http.MethodPatch, "/processes/proc_00000000", "", http.StatusMethodNotAllowed, "Method not allowed."},
		{http.MethodDelete, "/spawn", "", http.StatusMethodNotAllowed, "Method not allowed."},
		{http.MethodPost, "/processes", `{"cmd": "   "}`, http.StatusBadRequest, "cmd cannot be empty."},
		{http.MethodPost, "/processes", `{"cmd":`, http.StatusBadRequest, "Request body is not valid JSON."},
		{http.MethodPost, "/spawn", `["a"]`, http.StatusBadRequest, "Request body must be an object."},
		{http.MethodPost, "/spawn", `{"requester": "r", "kind": "daemon"}`, http.StatusBadRequest, "kind is not allowed."},
		{http.MethodPost, "/processes", `{"label": "Big", "cmd": "` + strings.Repeat("x", 70000) + `"}`,
			http.StatusRequestEntityTooLarge, "Request body is too large."},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			status, body := inst.request(t, c.method, c.path, c.body)
			if status != c.status {
				t.Errorf("status %d, want %d — %s", status, c.status, body)
			}
			if !strings.Contains(body, c.contains) {
				t.Errorf("body %s, want it to contain %q", body, c.contains)
			}
		})
	}
}

// A shutdown with a request in flight. The listener stops accepting at step ②
// and the request already being served is given its grace period, so it
// completes rather than being cut off (E-29, 0008-L 2.4.1).
func TestLiveShutdownWaitsForRequestsInFlight(t *testing.T) {
	requireLive(t)
	inst := start(t, freePort(t))
	inst.waitForHealthz(t, 20*time.Second)

	// A request and a shutdown, started together. Whichever order they land in,
	// the request must either be refused at the socket or answered in full —
	// never answered with a truncated body.
	type result struct {
		status int
		body   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		url := fmt.Sprintf("http://127.0.0.1:%d/processes", inst.port)
		res, err := http.Get(url)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		done <- result{status: res.StatusCode, body: string(raw), err: err}
	}()

	if err := sendConsoleCtrl(ctrlBreak, inst.cmd.Process.Pid); err != nil {
		t.Fatalf("cannot deliver the console event: %v", err)
	}

	select {
	case r := <-done:
		if r.err == nil {
			if r.status != http.StatusOK {
				t.Errorf("in-flight request: status %d", r.status)
			}
			// A body that was cut off mid-write is the failure being looked
			// for: it parses as neither valid JSON nor an empty response.
			if !strings.HasSuffix(strings.TrimSpace(r.body), "}") {
				t.Errorf("in-flight request came back truncated: %q", r.body)
			}
		}
		// A connection error is acceptable: the listener may have closed before
		// the request reached it, which is the same thing as never having been
		// accepted.
	case <-time.After(20 * time.Second):
		t.Fatal("the in-flight request never finished")
	}

	if code := inst.waitExit(t, 30*time.Second); code != lifecycle.ExitNormal {
		t.Errorf("exit code %d, want %d\nlog:\n%s", code, lifecycle.ExitNormal, inst.log())
	}
	if !strings.Contains(inst.log(), "stopped exit_code=0") {
		t.Errorf("the shutdown did not complete:\n%s", inst.log())
	}
}
