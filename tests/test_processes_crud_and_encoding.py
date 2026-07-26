"""실행기(process runner) 조회/수정/삭제 흐름 + 로그 인코딩 회귀 테스트.

CH0017 대화에서 지적된 두 가지 결함을 검증한다:
  1) 목록 항목을 눌러도 재사용(조회/수정/삭제)할 수 없고 실행 전용이었던 문제
     -> PUT(수정)/DELETE(삭제)/run(재실행)이 같은 id를 그대로 재사용하며
        목록에 중복 항목이 생기지 않아야 한다.
  2) 로그에 담긴 비-ASCII(한글) 문자가 깨지던 문제
     -> stdout에 한글을 출력하는 프로세스의 로그를 /processes/<id>/logs로
        읽었을 때 원문과 바이트 단위가 아니라 문자 단위로 정확히 일치해야
        하고 대체문자(U+FFFD)가 섞이지 않아야 한다.

실제 ThreadingHTTPServer를 임시 포트로 기동하고 urllib로 호출한다
(test_processes_http.py와 동일한 방식). 기본 로컬 실행(SPAWNPOINT_API_TOKENS
미설정)과 동일하게 인증을 비활성화한 상태로 검증한다.
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

from runnerview.page import INDEX_HTML
from spawnpoint.auth import AuthValidator
from spawnpoint.http_api import make_server
from spawnpoint.runner import ProcessManager
from spawnpoint.storage import Registry

PY = sys.executable


class ProcessesCrudTestBase(unittest.TestCase):
    def setUp(self):
        self._tmpdir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.registry = Registry.open(os.path.join(self._tmpdir.name, "spawnpoint.db"))
        self.auth = AuthValidator()  # 토큰 미설정 -> 기본 로컬 실행(인증 비활성화)
        self.procman = ProcessManager(os.path.join(self._tmpdir.name, "logs"))
        self.server = make_server(
            "127.0.0.1", 0, self.registry, self.auth,
            index_html=INDEX_HTML, procman=self.procman,
        )
        self.host, self.port = self.server.server_address[:2]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        for p in self.procman.list():
            if p.status == "running":
                self.procman.stop(p.id)
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.registry.close()
        self._tmpdir.cleanup()

    def _url(self, path):
        return f"http://{self.host}:{self.port}{path}"

    def request(self, method, path, body=None):
        data = json.dumps(body).encode("utf-8") if body is not None else None
        req = urllib.request.Request(self._url(path), data=data, method=method)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            try:
                return exc.code, json.loads(exc.read().decode("utf-8"))
            finally:
                exc.close()

    def _wait_until(self, predicate, timeout=5.0, interval=0.05):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if predicate():
                return True
            time.sleep(interval)
        return False


class TestSinglePortServesUiApiAndProcesses(ProcessesCrudTestBase):
    def test_ui_spawn_and_processes_share_one_port_without_auth(self):
        # 화면(UI) - 별도 앱/포트 없이 같은 서버가 서빙한다.
        req = urllib.request.Request(self._url("/"), method="GET")
        with urllib.request.urlopen(req, timeout=5) as resp:
            self.assertEqual(resp.status, 200)
            self.assertIn(b"<html", resp.read().lower())

        # 인스턴스 등록 API - 기본 로컬 실행에서는 토큰 없이 통과한다.
        status, payload = self.request(
            "POST", "/spawn", {"requester": "u", "kind": "task"}
        )
        self.assertEqual(status, 200)
        self.assertTrue(payload["ok"])

        # 실행기 API도 같은 서버·같은 포트에서, 토큰 없이 동작한다.
        status, payload = self.request("GET", "/processes")
        self.assertEqual(status, 200)
        self.assertEqual(payload["processes"], [])


class TestUpdateReusesSameEntry(ProcessesCrudTestBase):
    def test_put_updates_fields_without_creating_duplicate(self):
        status, payload = self.request(
            "POST", "/processes",
            {"cmd": f'"{PY}" -c "import time; time.sleep(30)"', "label": "old-label"},
        )
        self.assertEqual(status, 200)
        ident = payload["process"]["id"]

        status, payload = self.request(
            "PUT", f"/processes/{ident}",
            {
                "cmd": f'"{PY}" -c "print(1)"',
                "label": "new-label",
                "cwd": None,
                "env": {"K": "V"},
            },
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["process"]["id"], ident)
        self.assertEqual(payload["process"]["label"], "new-label")
        self.assertEqual(payload["process"]["cmd"], f'"{PY}" -c "print(1)"')
        self.assertEqual(payload["process"]["env"], {"K": "V"})

        status, payload = self.request("GET", "/processes")
        self.assertEqual(status, 200)
        self.assertEqual(len(payload["processes"]), 1)
        self.assertEqual(payload["processes"][0]["id"], ident)
        self.assertEqual(payload["processes"][0]["label"], "new-label")

    def test_put_unknown_id_404(self):
        status, payload = self.request(
            "PUT", "/processes/proc_nope", {"cmd": "echo hi"}
        )
        self.assertEqual(status, 404)
        self.assertEqual(payload["error"]["code"], "not_found")


class TestDeleteRemovesEntry(ProcessesCrudTestBase):
    def test_delete_removes_from_list_and_blocks_further_actions(self):
        status, payload = self.request(
            "POST", "/processes", {"cmd": f'"{PY}" -c "print(1)"'}
        )
        ident = payload["process"]["id"]
        self._wait_until(
            lambda: self.request("GET", f"/processes/{ident}/logs?offset=0")[1]
            .get("text", "") != "" or True
        )

        status, payload = self.request("DELETE", f"/processes/{ident}")
        self.assertEqual(status, 200)
        self.assertEqual(payload["deleted_id"], ident)

        status, payload = self.request("GET", "/processes")
        self.assertEqual(status, 200)
        self.assertFalse(any(p["id"] == ident for p in payload["processes"]))

        status, _ = self.request("POST", f"/processes/{ident}/stop")
        self.assertEqual(status, 404)

        status, _ = self.request("GET", f"/processes/{ident}/logs?offset=0")
        self.assertEqual(status, 404)

    def test_delete_unknown_id_404(self):
        status, payload = self.request("DELETE", "/processes/proc_nope")
        self.assertEqual(status, 404)
        self.assertEqual(payload["error"]["code"], "not_found")


class TestRunRestartsStoppedEntryWithSameId(ProcessesCrudTestBase):
    def test_run_reuses_id_and_does_not_duplicate(self):
        status, payload = self.request(
            "POST", "/processes", {"cmd": f'"{PY}" -c "print(1)"'}
        )
        ident = payload["process"]["id"]

        self.assertTrue(
            self._wait_until(
                lambda: self.request("GET", "/processes")[1]["processes"][0]["status"]
                != "running"
            )
        )

        status, payload = self.request("POST", f"/processes/{ident}/run")
        self.assertEqual(status, 200)
        self.assertEqual(payload["process"]["id"], ident)
        self.assertEqual(payload["process"]["status"], "running")

        status, payload = self.request("GET", "/processes")
        self.assertEqual(len(payload["processes"]), 1)
        self.assertEqual(payload["processes"][0]["id"], ident)

        self.request("POST", f"/processes/{ident}/stop")


class TestKoreanLogEncodingRoundTrips(ProcessesCrudTestBase):
    def test_korean_stdout_is_not_mangled_in_log_tail(self):
        message = "한글 로그 테스트 화살표 -> 완료"
        status, payload = self.request(
            "POST", "/processes",
            {"cmd": f'"{PY}" -c "print({message!r})"'},
        )
        self.assertEqual(status, 200)
        ident = payload["process"]["id"]

        collected = {"text": ""}

        def has_full_message():
            _, r = self.request("GET", f"/processes/{ident}/logs?offset=0")
            collected["text"] = r.get("text", "")
            return message in collected["text"]

        self.assertTrue(
            self._wait_until(has_full_message),
            f"한글 로그가 그대로 도착하지 않음: {collected['text']!r}",
        )
        self.assertNotIn("�", collected["text"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
