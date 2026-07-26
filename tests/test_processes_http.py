"""실행기(process runner) HTTP 라우트 통합 테스트.

실제 ThreadingHTTPServer를 임시 포트로 기동하고 /processes* 엔드포인트를
urllib로 호출해 검증한다(test_http_integration.py와 동일한 방식).
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

from spawnpoint.auth import AuthValidator
from spawnpoint.http_api import make_server
from spawnpoint.runner import ProcessManager
from spawnpoint.storage import Registry

TOKEN = "tok-proc-xyz"
PY = sys.executable


class ProcessesHttpTestBase(unittest.TestCase):
    def setUp(self):
        # ignore_cleanup_errors: Windows의 taskkill 강제종료 직후 핸들 반환 지연 대비.
        self._tmpdir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.registry = Registry.open(os.path.join(self._tmpdir.name, "spawnpoint.db"))
        self.auth = AuthValidator([TOKEN])
        self.procman = ProcessManager(os.path.join(self._tmpdir.name, "logs"))
        self.server = make_server(
            "127.0.0.1", 0, self.registry, self.auth, procman=self.procman
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

    def request(self, method, path, body=None, token=TOKEN):
        data = json.dumps(body).encode("utf-8") if body is not None else None
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

    def _wait_until(self, predicate, timeout=5.0, interval=0.05):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if predicate():
                return True
            time.sleep(interval)
        return False


class TestProcessLifecycleOverHttp(ProcessesHttpTestBase):
    def test_start_list_stop(self):
        status, payload = self.request(
            "POST", "/processes",
            {"cmd": f'"{PY}" -c "import time; time.sleep(30)"', "label": "sleeper"},
        )
        self.assertEqual(status, 200)
        self.assertTrue(payload["ok"])
        ident = payload["process"]["id"]
        self.assertEqual(payload["process"]["status"], "running")

        status, payload = self.request("GET", "/processes")
        self.assertEqual(status, 200)
        self.assertTrue(any(p["id"] == ident for p in payload["processes"]))

        status, payload = self.request("POST", f"/processes/{ident}/stop")
        self.assertEqual(status, 200)
        self.assertEqual(payload["process"]["status"], "stopped")

    def test_restart(self):
        _, payload = self.request(
            "POST", "/processes",
            {"cmd": f'"{PY}" -c "import time; time.sleep(30)"'},
        )
        ident = payload["process"]["id"]
        first_pid = payload["process"]["pid"]

        status, payload = self.request("POST", f"/processes/{ident}/restart")
        self.assertEqual(status, 200)
        self.assertEqual(payload["process"]["status"], "running")
        self.assertNotEqual(payload["process"]["pid"], first_pid)

        self.request("POST", f"/processes/{ident}/stop")

    def test_logs_tail(self):
        _, payload = self.request(
            "POST", "/processes",
            {"cmd": f'"{PY}" -c "print(\'hello-from-log\')"'},
        )
        ident = payload["process"]["id"]

        def has_output():
            _, r = self.request("GET", f"/processes/{ident}/logs?offset=0")
            return "hello-from-log" in r.get("text", "")

        self.assertTrue(self._wait_until(has_output))

    def test_unknown_process_404(self):
        status, payload = self.request("POST", "/processes/proc_nope/stop")
        self.assertEqual(status, 404)
        self.assertEqual(payload["error"]["code"], "not_found")

        status, payload = self.request("GET", "/processes/proc_nope/logs?offset=0")
        self.assertEqual(status, 404)

    def test_missing_cmd_invalid_request(self):
        status, payload = self.request("POST", "/processes", {"label": "x"})
        self.assertEqual(status, 400)
        self.assertEqual(payload["error"]["code"], "invalid_request")
        self.assertEqual(payload["error"]["field"], "cmd")

    def test_unauthorized(self):
        status, payload = self.request("GET", "/processes", token=None)
        self.assertEqual(status, 401)
        self.assertEqual(payload["error"]["code"], "unauthorized")

        status, payload = self.request(
            "POST", "/processes", {"cmd": "echo hi"}, token=None
        )
        self.assertEqual(status, 401)


class TestProcessesRouteDisabledWithoutProcman(unittest.TestCase):
    def setUp(self):
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

    def test_processes_route_404_when_procman_not_injected(self):
        req = urllib.request.Request(
            f"http://{self.host}:{self.port}/processes", method="GET"
        )
        req.add_header("Authorization", f"Bearer {TOKEN}")
        try:
            urllib.request.urlopen(req, timeout=5)
            self.fail("expected HTTPError")
        except urllib.error.HTTPError as exc:
            self.assertEqual(exc.code, 404)
            exc.close()

class TestProcessesWithoutConfiguredAuth(unittest.TestCase):
    def setUp(self):
        self._tmpdir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.registry = Registry.open(os.path.join(self._tmpdir.name, "spawnpoint.db"))
        self.auth = AuthValidator()
        self.procman = ProcessManager(os.path.join(self._tmpdir.name, "logs"))
        self.server = make_server(
            "127.0.0.1", 0, self.registry, self.auth, procman=self.procman
        )
        self.host, self.port = self.server.server_address[:2]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        for process in self.procman.list():
            if process.status == "running":
                self.procman.stop(process.id)
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.registry.close()
        self._tmpdir.cleanup()

    def test_list_and_start_work_without_authorization_header(self):
        url = f"http://{self.host}:{self.port}"
        with urllib.request.urlopen(f"{url}/processes", timeout=5) as resp:
            self.assertEqual(resp.status, 200)

        data = json.dumps({"cmd": f'"{PY}" -c "print(123)"'}).encode("utf-8")
        request = urllib.request.Request(
            f"{url}/processes", data=data, method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=5) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
        self.assertTrue(payload["ok"])

if __name__ == "__main__":
    unittest.main(verbosity=2)
