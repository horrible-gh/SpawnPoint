package httpapi

import (
	"net/http"
	"testing"

	"spawnpoint/internal/runner"
	"spawnpoint/internal/textdec"
)

// The log scenarios of 0007-P, with the measured numbers from the 20.6 MiB
// proc_45c05b99 file. The numbers are kept exactly as measured rather than
// rounded: the line-boundary arithmetic hides precisely behind a round number.
//
// What is being checked here is the response, not the reading. Where the read
// starts, how far it goes and which notation it was decoded as are the runner's
// (T5, and pinned by its own tests); this package's job is to put all seven
// fields on the wire, always, with the right names.

func logServer(t *testing.T, view runner.LogView) (*Server, *fakeRunner) {
	t.Helper()
	rn := &fakeRunner{
		entries: []runner.Info{{ID: "proc_45c05b99", Env: map[string]string{}}},
		view:    view,
		viewOK:  true,
	}
	return newTestServer(t, Options{Runner: rn}), rn
}

// 0007-P [로그 최초 조회 — 끝부분부터]. The request carries no offset at all,
// which is what makes the default the end of the file.
func TestLogTailResponse(t *testing.T) {
	s, rn := logServer(t, runner.LogView{
		Text:        "[2026-07-28 14:41:02] GET /flowgate/api/v1/document/spawnpoint.default.0010.0006-D 200\n[2026-07-28 14:41:06] POST /flowgate/api/v1/remote/read 200\n",
		StartOffset: 21291313,
		NextOffset:  21553390,
		Size:        21553390,
		Encoding:    textdec.NameUTF8,
	})

	status, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true,
	  "text": "[2026-07-28 14:41:02] GET /flowgate/api/v1/document/spawnpoint.default.0010.0006-D 200\n[2026-07-28 14:41:06] POST /flowgate/api/v1/remote/read 200\n",
	  "start_offset": 21291313,
	  "next_offset": 21553390,
	  "size": 21553390,
	  "truncated": false,
	  "reset": false,
	  "encoding": "utf-8"
	}`)
	// The absence of the parameter has to reach the reader as an absence. Turned
	// into a 0 here it would mean "all of it", which is the request that pulled
	// 20 MiB across on every click (0004-NR 1.7).
	if rn.lastLog != "" {
		t.Errorf("offset reached the reader as %q, want the empty string", rn.lastLog)
	}
}

// 0007-P [로그 이어받기 — 증분].
func TestLogResumeResponse(t *testing.T) {
	s, rn := logServer(t, runner.LogView{
		Text:        "[2026-07-28 14:41:09] POST /flowgate/api/v1/remote/glob 200\n",
		StartOffset: 21553390,
		NextOffset:  21553452,
		Size:        21553452,
		Encoding:    textdec.NameUTF8,
	})

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset=21553390", "")
	wantBody(t, body, `{
	  "ok": true,
	  "text": "[2026-07-28 14:41:09] POST /flowgate/api/v1/remote/glob 200\n",
	  "start_offset": 21553390, "next_offset": 21553452, "size": 21553452,
	  "truncated": false, "reset": false, "encoding": "utf-8"
	}`)
	if rn.lastLog != "21553390" {
		t.Errorf("offset reached the reader as %q", rn.lastLog)
	}
}

// Nothing new is still a 200 with the whole structure. 304 is not used: the
// screen reads next_offset out of every answer, and a 304 has no body to read
// it from (0007-P 0.6).
func TestLogWithNothingNewIsStillATwoHundred(t *testing.T) {
	s, _ := logServer(t, runner.LogView{
		StartOffset: 21553452, NextOffset: 21553452, Size: 21553452,
		Encoding: textdec.NameUTF8,
	})

	status, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset=21553452", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true, "text": "",
	  "start_offset": 21553452, "next_offset": 21553452, "size": 21553452,
	  "truncated": false, "reset": false, "encoding": "utf-8"
	}`)
}

// 0007-P [로그 전량 조회 — 응답 상한에 걸림]: truncated says the response limit
// stopped the read, not that anything was lost.
func TestLogTruncatedResponse(t *testing.T) {
	s, _ := logServer(t, runner.LogView{
		Text: "…", StartOffset: 0, NextOffset: 1048573, Size: 21553452,
		Truncated: true, Encoding: textdec.NameUTF8,
	})

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset=0", "")
	wantBody(t, body, `{
	  "ok": true, "text": "…",
	  "start_offset": 0, "next_offset": 1048573, "size": 21553452,
	  "truncated": true, "reset": false, "encoding": "utf-8"
	}`)
}

// 0007-P [로그 넘겨 쓰기 감지 — 위치 초기화]: reset tells the screen to throw
// away what it has. Without it the head of the new file is appended to the tail
// of the old one and reads as corruption.
func TestLogRotationResponse(t *testing.T) {
	s, _ := logServer(t, runner.LogView{
		Text:        "[2026-07-28 15:02:11] flowgate started\n",
		StartOffset: 0, NextOffset: 4096, Size: 4096,
		Reset: true, Encoding: textdec.NameUTF8,
	})

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset=21553452", "")
	wantBody(t, body, `{
	  "ok": true, "text": "[2026-07-28 15:02:11] flowgate started\n",
	  "start_offset": 0, "next_offset": 4096, "size": 4096,
	  "truncated": false, "reset": true, "encoding": "utf-8"
	}`)
}

// 0007-P [로그 조회 — 여러 표기 혼재]: next_offset short of size is normal, not
// truncation — the tail of a half-written character is carried to the next poll
// (E-10). Korean travels as Korean, which is the escaping rule under test.
func TestLogMixedNotationResponse(t *testing.T) {
	s, _ := logServer(t, runner.LogView{
		Text:        "서버를 시작합니다\n포트 8100 대기 중\n",
		StartOffset: 4096, NextOffset: 4159, Size: 4162,
		Encoding: textdec.NameCP949,
	})

	_, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset=4096", "")
	wantBody(t, body, `{
	  "ok": true, "text": "서버를 시작합니다\n포트 8100 대기 중\n",
	  "start_offset": 4096, "next_offset": 4159, "size": 4162,
	  "truncated": false, "reset": false, "encoding": "cp949"
	}`)
}

// 0007-P [로그 조회 — 아직 아무것도 실행하지 않은 항목] / E-8: a missing log
// file is an empty result, not a 404. 404 is reserved for an entry that is not
// registered, and a poller that received one would stop polling an entry that
// is about to start writing.
func TestLogOfAnEntryThatHasNeverRun(t *testing.T) {
	s, _ := logServer(t, runner.LogView{Encoding: textdec.NameUTF8})

	status, body, _ := do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}
	wantBody(t, body, `{
	  "ok": true, "text": "",
	  "start_offset": 0, "next_offset": 0, "size": 0,
	  "truncated": false, "reset": false, "encoding": "utf-8"
	}`)
}

// The offset parameter is handed down as the string it arrived as. Every form
// 0008-L 4.2 lists has a different meaning, and deciding between them here
// would put the reading contract in two places.
func TestOffsetReachesTheReaderUntouched(t *testing.T) {
	s, rn := logServer(t, runner.LogView{Encoding: textdec.NameUTF8})

	for _, raw := range []string{"tail", "tail:65536", "TAIL", "0", "21553390", "-1", "abc", "9999999999999999999999"} {
		do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset="+raw, "")
		if rn.lastLog != raw {
			t.Errorf("offset=%s reached the reader as %q", raw, rn.lastLog)
		}
	}
	// `offset=` with nothing after it is the same as sending none: both mean a
	// tail read (0008-L 4.2 step 1).
	do(t, s, http.MethodGet, "/processes/proc_45c05b99/logs?offset=", "")
	if rn.lastLog != "" {
		t.Errorf("an empty offset reached the reader as %q", rn.lastLog)
	}
}
