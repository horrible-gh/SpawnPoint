"""ProcessManager(실행기) 단위 테스트.

실제 서브프로세스를 짧게 띄워 start/stop/restart/list/read_log를 검증한다.
플랫폼 독립적으로 sys.executable(-c ...)을 사용한다.
"""
from __future__ import annotations

import os
import sys
import tempfile
import time
import unittest

from spawnpoint.runner import ProcessManager

PY = sys.executable


def _cmd(code: str) -> str:
    return f'"{PY}" -c "{code}"'


class ProcessManagerTestBase(unittest.TestCase):
    def setUp(self):
        # ignore_cleanup_errors: Windows는 강제 종료된 자식 프로세스가 물려받은
        # 로그 파일 핸들을 taskkill 반환 직후 살짝 늦게 놓아줄 때가 있어,
        # cleanup() 직후 rmtree가 드물게 PermissionError를 낼 수 있다.
        self._tmpdir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.pm = ProcessManager(os.path.join(self._tmpdir.name, "logs"))

    def tearDown(self):
        self._tmpdir.cleanup()

    def _wait_until(self, predicate, timeout=5.0, interval=0.05):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if predicate():
                return True
            time.sleep(interval)
        return False


class TestStartAndExit(ProcessManagerTestBase):
    def test_short_lived_process_reports_exited(self):
        info = self.pm.start("echoer", _cmd("print('hello')"))
        self.assertEqual(info.status, "running")
        self.assertIsNotNone(info.pid)

        ok = self._wait_until(lambda: self.pm.get(info.id).status != "running")
        self.assertTrue(ok, "process did not finish in time")

        final = self.pm.get(info.id)
        self.assertEqual(final.status, "exited")
        self.assertEqual(final.exit_code, 0)
        self.assertIsNotNone(final.ended_at)

    def test_log_captures_stdout(self):
        info = self.pm.start("echoer", _cmd("print('line-a'); print('line-b')"))
        self._wait_until(lambda: self.pm.get(info.id).status != "running")
        text, next_offset = self.pm.read_log(info.id, 0)
        self.assertIn("line-a", text)
        self.assertIn("line-b", text)
        self.assertGreater(next_offset, 0)

        # 다시 읽으면(같은 offset부터) 새 내용이 없어야 한다.
        more, _ = self.pm.read_log(info.id, next_offset)
        self.assertEqual(more, "")

    def test_nonzero_exit_reports_killed_status(self):
        info = self.pm.start("failer", _cmd("import sys; sys.exit(3)"))
        ok = self._wait_until(lambda: self.pm.get(info.id).status != "running")
        self.assertTrue(ok)
        final = self.pm.get(info.id)
        self.assertEqual(final.status, "killed")
        self.assertEqual(final.exit_code, 3)


class TestStopAndRestart(ProcessManagerTestBase):
    def test_stop_terminates_long_running_process(self):
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        self.assertEqual(self.pm.get(info.id).status, "running")

        stopped = self.pm.stop(info.id)
        self.assertEqual(stopped.status, "stopped")
        self.assertIsNotNone(stopped.ended_at)

    def test_restart_reuses_same_id(self):
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        first_pid = info.pid

        restarted = self.pm.restart(info.id)
        self.assertEqual(restarted.id, info.id)
        self.assertEqual(restarted.status, "running")
        self.assertNotEqual(restarted.pid, first_pid)

        self.pm.stop(info.id)

    def test_restart_unknown_id_returns_none(self):
        self.assertIsNone(self.pm.restart("proc_doesnotexist"))

    def test_stop_unknown_id_returns_none(self):
        self.assertIsNone(self.pm.stop("proc_doesnotexist"))


class TestList(ProcessManagerTestBase):
    def test_list_reflects_all_started_processes(self):
        a = self.pm.start("a", _cmd("print('a')"))
        b = self.pm.start("b", _cmd("import time; time.sleep(30)"))

        ids = {p.id for p in self.pm.list()}
        self.assertEqual(ids, {a.id, b.id})

        self.pm.stop(b.id)


if __name__ == "__main__":
    unittest.main(verbosity=2)
