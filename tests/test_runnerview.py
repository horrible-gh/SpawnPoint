"""화면(실행기 시안 정적 뷰) — 통합 테스트.

화면은 별도 프로세스가 아니라 메인 서버(spawnpoint.http_api)가 같은 포트에서
API(/spawn)와 함께 서빙한다. 실제 ThreadingHTTPServer 를 임시 포트로 기동해
GET / , /index.html 이 화면을 돌려주고, 같은 서버가 /spawn 도 처리함을 검증한다.
"""
from __future__ import annotations

import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request

from runnerview.page import INDEX_HTML
from spawnpoint.auth import AuthValidator
from spawnpoint.http_api import make_server
from spawnpoint.storage import Registry

TOKEN = "tok-ui-xyz"


class TestFrontendServedByMainServer(unittest.TestCase):
    def setUp(self):
        # 파일 기반 SQLite 사용(test_http_integration.py 참고).
        self._tmpdir = tempfile.TemporaryDirectory()
        self.registry = Registry.open(os.path.join(self._tmpdir.name, "spawnpoint.db"))
        self.auth = AuthValidator([TOKEN])
        self.server = make_server(
            "127.0.0.1", 0, self.registry, self.auth, index_html=INDEX_HTML
        )
        self.host, self.port = self.server.server_address[:2]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.registry.close()
        self._tmpdir.cleanup()

    def _url(self, path):
        return f"http://{self.host}:{self.port}{path}"

    def _get(self, path):
        req = urllib.request.Request(self._url(path), method="GET")
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, resp.getheader("Content-Type"), resp.read()
        except urllib.error.HTTPError as exc:
            try:
                return exc.code, exc.headers.get("Content-Type"), exc.read()
            finally:
                exc.close()

    def test_index_serves_static_ui(self):
        status, content_type, body = self._get("/")
        self.assertEqual(status, 200)
        self.assertIn("text/html", content_type)
        self.assertEqual(body, INDEX_HTML)
        self.assertIn(b"Process Runner", body)
        self.assertNotIn(b"tokenInput", body)
        self.assertNotIn("Bearer 토큰".encode("utf-8"), body)

    def test_index_html_alias(self):
        status, _, body = self._get("/index.html")
        self.assertEqual(status, 200)
        self.assertEqual(body, INDEX_HTML)

    def test_same_server_also_serves_spawn_api(self):
        # 화면과 API가 같은 포트에서 함께 동작함을 확인(단일 앱의 핵심).
        data = json.dumps(
            {"requester": "user-a1b2c3", "kind": "session", "options": {}}
        ).encode("utf-8")
        req = urllib.request.Request(self._url("/spawn"), data=data, method="POST")
        req.add_header("Authorization", f"Bearer {TOKEN}")
        req.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(req, timeout=5) as resp:
            status = resp.status
            payload = json.loads(resp.read().decode("utf-8"))
        self.assertEqual(status, 200)
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["instance"]["status"], "created")

    def test_healthz(self):
        status, content_type, body = self._get("/healthz")
        self.assertEqual(status, 200)
        self.assertIn("application/json", content_type)
        self.assertIn(b'"healthy"', body)

    def test_unknown_route_404(self):
        status, _, body = self._get("/nope")
        self.assertEqual(status, 404)
        self.assertIn(b"not_found", body)

    def test_method_not_allowed_405(self):
        req = urllib.request.Request(self._url("/"), method="PUT")
        try:
            urllib.request.urlopen(req, timeout=5)
            self.fail("expected HTTPError")
        except urllib.error.HTTPError as exc:
            self.assertEqual(exc.code, 405)
            exc.close()


if __name__ == "__main__":
    unittest.main(verbosity=2)
