package access

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Read the bounded request before any early rejection or bodyless action.
// Closing an HTTP/1 connection with unread data can otherwise discard the
// response on Windows, including a successful DELETE or an explicit 403.
// No request is retried, and neither authentication nor authorization changes.
func prepareBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	original := r.Body
	defer original.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, original, 65536))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var netErr net.Error
		switch {
		case errors.As(err, &tooLarge):
			fail(w, http.StatusRequestEntityTooLarge, "요청이 너무 큽니다")
		case errors.As(err, &netErr) && netErr.Timeout():
			fail(w, http.StatusRequestTimeout, "요청 본문 수신 시간이 초과되었습니다")
		default:
			fail(w, http.StatusBadRequest, "요청 본문을 읽지 못했습니다")
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}
