"""엔드투엔드 HTTP 통합 테스트.

실제 ThreadingHTTPServer 를 임시 포트로 기동하고 urllib 로 요청을 보내
프로토콜 P-0005 시나리오와 운영 라우트(healthz/405/413/404)를 검증한다.
"""
from __future__ import annotations

import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request

from spawnpoint.auth import AuthValidator
from spawnpoint.http_api import MAX_BODY_BYTES, make_server
from spawnpoint.storage import Registry

TOKEN = "tok-int-xyz"


class HttpServerTestBase(unittest.TestCase):
    def setUp(self):
        # sqloader(0.2.18)의 자동 마이그레이션은 파일 기반 SQLite에서만 안정적으로
        # 동작하므로(test_spawnpoint.py의 SpawnTestBase.setUp 참고) 임시 파일 DB를 쓴다.
        self._tmpdir = tempfile.TemporaryDirectory()
        self.registry = Registry.open(os.path.join(self._tmpdir.name, "spawnpoint.db"))
        self.auth = AuthValidator([TOKEN])
        self.server = make_server("127.0.0.1", 0, self.registry, self.auth)
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

    def request(self, method, path, body=None, token=TOKEN, raw=None):
        data = None
        if raw is not None:
            data = raw
        elif body is not None:
            data = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(self._url(path), data=data, method=method)
        if token is not None:
            req.add_header("Authorization", f"Bearer {token}")
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


class TestSpawnOverHttp(HttpServerTestBase):
    def test_scenario1_success(self):
        status, payload = self.request(
            "POST", "/spawn",
            {"requester": "user-a1b2c3", "kind": "session", "options": {}},
        )
        self.assertEqual(status, 200)
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["instance"]["status"], "created")

    def test_scenario2_invalid(self):
        status, payload = self.request(
            "POST", "/spawn", {"requester": "u", "kind": ""}
        )
        self.assertEqual(status, 400)
        self.assertEqual(payload["error"]["code"], "invalid_request")
        self.assertEqual(payload["error"]["field"], "kind")

    def test_scenario3_dedup(self):
        body = {"requester": "u", "kind": "task", "request_key": "rk-http"}
        s1, p1 = self.request("POST", "/spawn", body)
        s2, p2 = self.request("POST", "/spawn", body)
        self.assertEqual(s1, 200)
        self.assertEqual(s2, 200)
        self.assertTrue(p2.get("deduplicated"))
        self.assertEqual(p1["instance"]["id"], p2["instance"]["id"])

    def test_scenario4_unauthorized(self):
        status, payload = self.request(
            "POST", "/spawn", {"requester": "u", "kind": "task"}, token=None
        )
        self.assertEqual(status, 401)
        self.assertEqual(payload["error"]["code"], "unauthorized")

    def test_healthz(self):
        status, payload = self.request("GET", "/healthz", token=None)
        self.assertEqual(status, 200)
        self.assertEqual(payload["status"], "healthy")

    def test_unknown_route_404(self):
        status, payload = self.request("GET", "/nope", token=None)
        self.assertEqual(status, 404)
        self.assertEqual(payload["error"]["code"], "not_found")

    def test_method_not_allowed_405(self):
        status, payload = self.request(
            "DELETE", "/spawn", body={"x": 1}
        )
        self.assertEqual(status, 405)
        self.assertEqual(payload["error"]["code"], "method_not_allowed")

    def test_payload_too_large_413(self):
        big = b'{"requester":"u","kind":"task","options":{"label":"' + b"a" * (
            MAX_BODY_BYTES + 10
        ) + b'"}}'
        status, payload = self.request("POST", "/spawn", raw=big)
        self.assertEqual(status, 413)
        self.assertEqual(payload["error"]["code"], "payload_too_large")

    def test_bad_json_400(self):
        status, payload = self.request("POST", "/spawn", raw=b"{not json")
        self.assertEqual(status, 400)
        self.assertEqual(payload["error"]["code"], "invalid_request")


if __name__ == "__main__":
    unittest.main(verbosity=2)
