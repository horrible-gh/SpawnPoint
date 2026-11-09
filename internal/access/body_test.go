package access

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPreparedBodyIsAvailableToAuthorizationHandlers(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(method, "/processes", strings.NewReader(`{"cmd":"echo ok"}`))
		w := httptest.NewRecorder()
		if !prepareBody(w, r) {
			t.Fatalf("%s request rejected", method)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != `{"cmd":"echo ok"}` || r.ContentLength != int64(len(b)) {
			t.Fatalf("%s body changed: %q / %v", method, b, err)
		}
	}
}

func TestPreparedBodyRemainsSizeBounded(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(strings.Repeat("x", 65537)))
	w := httptest.NewRecorder()
	if prepareBody(w, r) || w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body accepted: %d", w.Code)
	}
}

// Send the body after its headers with Connection: close, reproducing the
// early-response/unread-body race without retrying or weakening a 403 check.
func TestCrossOriginCloseResponseConsumesSmallBody(t *testing.T) {
	server := httptest.NewServer(&Manager{})
	defer server.Close()
	for i := 0; i < 8; i++ {
		conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprintf(conn, "POST /processes HTTP/1.1\r\nHost: %s\r\nOrigin: https://evil.invalid\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n", server.Listener.Addr())
		time.Sleep(10 * time.Millisecond)
		if _, err = io.WriteString(conn, "{}"); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		conn.Close()
		if err != nil || response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), `"ok":false`) {
			t.Fatalf("response was lost or denial changed: %d %q %v", response.StatusCode, body, err)
		}
	}
}
