package httpapi

import (
	"net/http"
	"strconv"
	"testing"
)

func TestHelpReturnsTheInjectedBytesAndHeaders(t *testing.T) {
	body := []byte("{\n  \"ok\": true, \"escaped\": \"<>&\"\n}\n")
	s := newTestServer(t, Options{Help: body})

	status, got, header := do(t, s, http.MethodGet, "/help", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	if got != string(body) {
		t.Errorf("body %q, want injected bytes %q", got, body)
	}
	if value := header.Get("Content-Type"); value != contentTypeJSON {
		t.Errorf("Content-Type %q, want %q", value, contentTypeJSON)
	}
	if value := header.Get("Cache-Control"); value != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", value)
	}
	if value := header.Get("Content-Length"); value != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length %q, want %d", value, len(body))
	}
}

func TestHelpHeadMatchesGetWithoutABody(t *testing.T) {
	body := []byte(`{"ok":true}`)
	s := newTestServer(t, Options{Help: body})
	getStatus, _, getHeader := do(t, s, http.MethodGet, "/help", "")
	headStatus, headBody, headHeader := do(t, s, http.MethodHead, "/help", "")
	if headStatus != getStatus || headBody != "" {
		t.Errorf("HEAD status/body = %d/%q, GET status = %d", headStatus, headBody, getStatus)
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "Content-Length"} {
		if got, want := headHeader.Get(name), getHeader.Get(name); got != want {
			t.Errorf("HEAD %s %q, GET %q", name, got, want)
		}
	}
}

func TestHelpRejectsWrongMethodsAndLookalikePaths(t *testing.T) {
	s := newTestServer(t, Options{Help: []byte(`{"ok":true}`)})
	status, body, _ := do(t, s, http.MethodPost, "/help", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /help status %d, want 405", status)
	}
	wantBody(t, body, `{"ok":false,"error":{"code":"method_not_allowed","message":"Method not allowed."}}`)
	for _, path := range []string{"/help/", "/help/x", "/HELP"} {
		status, body, _ = do(t, s, http.MethodGet, path, "")
		if status != http.StatusNotFound {
			t.Errorf("GET %s status %d, want 404", path, status)
		}
		wantBody(t, body, `{"ok":false,"error":{"code":"not_found","message":"Unknown endpoint."}}`)
	}
}

func TestHelpIsUndefinedWhenNotInjected(t *testing.T) {
	s := newTestServer(t, Options{})
	status, body, _ := do(t, s, http.MethodGet, "/help", "")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
	wantBody(t, body, `{"ok":false,"error":{"code":"not_found","message":"Unknown endpoint."}}`)
}

func TestHelpUsesConditionalBearerAuthentication(t *testing.T) {
	const token = "secret"
	if status, _, _ := do(t, newTestServer(t, Options{Help: []byte(`{}`)}), http.MethodGet, "/help", ""); status != http.StatusOK {
		t.Errorf("tokens disabled: status %d, want 200", status)
	}
	s := newTestServer(t, Options{Config: authConfig(token), Help: []byte(`{}`)})
	for _, tc := range []struct {
		name   string
		header string
		status int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "Bearer wrong", http.StatusUnauthorized},
		{"valid", "Bearer " + token, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var headers [][2]string
			if tc.header != "" {
				headers = append(headers, [2]string{"Authorization", tc.header})
			}
			status, _, _ := do(t, s, http.MethodGet, "/help", "", headers...)
			if status != tc.status {
				t.Errorf("status %d, want %d", status, tc.status)
			}
		})
	}
}
