package webui

import (
	"strings"
	"testing"
)

// The screen is JavaScript in a string, so there is no way to run it here. What
// can be checked is that the four contract changes it had to make are actually
// in the file, and that the behaviour each one replaced is gone.
//
// These are grep assertions and they are worth exactly what grep is worth: they
// cannot show the screen works. They can show that someone who reverts one of
// these changes — or copies the old screen back over this one — finds out
// immediately rather than through a report about a 20 MiB page load.

func TestTheScreenIsEmbedded(t *testing.T) {
	if !Valid() {
		t.Fatal("the embedded screen did not pass its own check")
	}
	if Size() < 10000 {
		t.Errorf("the screen is %d bytes, which is too small to be the screen", Size())
	}
	if len(Index()) != Size() {
		t.Errorf("Index() is %d bytes and Size() says %d", len(Index()), Size())
	}
}

// Index returns a copy. The caller hands it to net/http, and a handler given
// the embedded backing array could write through it.
func TestIndexReturnsACopy(t *testing.T) {
	first := Index()
	if len(first) == 0 {
		t.Fatal("empty screen")
	}
	first[0] = 'X'
	if Index()[0] == 'X' {
		t.Error("Index() aliases the embedded screen")
	}
}

// 0007-P [로그 최초 조회]: the first request for a newly selected entry carries
// no offset. The old screen sent offset=0 and made the server read whole files
// (0004-NR 1.7).
func TestTheScreenAsksForTheTailFirst(t *testing.T) {
	s := string(Index())

	if strings.Contains(s, `"/logs?offset=" + state.logOffset`) {
		t.Error("the screen still sends an offset on every log request")
	}
	if !strings.Contains(s, "state.logOffset !== null") {
		t.Error("the screen does not distinguish an absent offset from zero")
	}
	if !strings.Contains(s, "state.logOffset = null") {
		t.Error("selecting an entry does not reset the offset to absent")
	}
}

// 0007-P [로그 넘겨 쓰기 감지]: reset means throw away what is on screen. Without
// this the head of the new file is drawn onto the tail of the old one.
func TestTheScreenHandlesRotation(t *testing.T) {
	s := string(Index())
	if !strings.Contains(s, "data.reset") {
		t.Error("the screen ignores the reset flag")
	}
}

// 0007-P [로그 전량 조회]: truncated needs a way to ask for the rest, and E-12
// says the percentage must not divide by a zero size.
func TestTheScreenHandlesTruncation(t *testing.T) {
	s := string(Index())
	if !strings.Contains(s, "data.truncated") {
		t.Error("the screen ignores the truncated flag")
	}
	if !strings.Contains(s, "data.size === 0") {
		t.Error("the screen divides by size without guarding zero (E-12)")
	}
	if !strings.Contains(s, "logMoreBtn") {
		t.Error("the screen offers no way to continue a truncated read")
	}
}

// The two fields the rewrite added to the runner responses: a start that never
// happened, and a registration that was not written down (0006-D 3.3,
// 0004-NR F7).
func TestTheScreenShowsTheNewFields(t *testing.T) {
	s := string(Index())
	if !strings.Contains(s, `p.status === "failed"`) {
		t.Error("the screen has no rendering for the failed status")
	}
	if !strings.Contains(s, "data.persisted === false") {
		t.Error("the screen ignores persisted:false")
	}
	if !strings.Contains(s, "data.encoding") {
		t.Error("the screen does not report which notation the log was read as")
	}
}

// 0007-P [인증 실패]: stop polling. A screen that kept going would produce one
// rejected request per second for as long as the tab is open.
func TestTheScreenStopsPollingOnAuthenticationFailure(t *testing.T) {
	s := string(Index())
	if !strings.Contains(s, "markUnauthorised") {
		t.Error("the screen has no handling for a 401")
	}
	if !strings.Contains(s, "state.authRequired") {
		t.Error("the screen does not latch the authentication failure")
	}
}
