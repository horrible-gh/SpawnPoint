"""러너 엔트리 영속화 회귀 테스트(서버 재시작 시나리오).

버그 "Failed to fetch / 커맨드가 다 날아감"의 핵심은 ProcessManager 상태가
프로세스 메모리에만 있었다는 점이다. 서버 재시작은 같은 db_path/log_dir 로
Registry + ProcessManager 를 새로 만드는 것으로 모사한다.
"""
from __future__ import annotations

import os
import sys
import tempfile
import time
import unittest

from spawnpoint.runner import ProcessManager
from spawnpoint.storage import Registry

PY = sys.executable


def _cmd(code: str) -> str:
    return f'"{PY}" -c "{code}"'


class RunnerPersistenceTestBase(unittest.TestCase):
    def setUp(self):
        # ignore_cleanup_errors: Windows는 taskkill 직후 로그 파일 핸들 반환이 늦을 수 있다.
        self._tmpdir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.db_path = os.path.join(self._tmpdir.name, "spawnpoint.db")
        self.log_dir = os.path.join(self._tmpdir.name, "logs")
        self._registries: list[Registry] = []
        self._managers: list[ProcessManager] = []
        self.pm = self._boot()

    def tearDown(self):
        for manager in self._managers:
            manager.shutdown(kill_children=True)
        for registry in self._registries:
            registry.close()
        self._tmpdir.cleanup()

    def _boot(self) -> ProcessManager:
        """서버 기동 1회분: 같은 파일을 보는 새 Registry + 새 ProcessManager."""
        registry = Registry.open(self.db_path)
        manager = ProcessManager(self.log_dir, store=registry)
        self._registries.append(registry)
        self._managers.append(manager)
        return manager

    def _restart(self) -> ProcessManager:
        """현재 서버를 내리고(자식 정리 포함) 새 서버를 올린다."""
        self.pm.shutdown(kill_children=True)
        self.pm = self._boot()
        return self.pm

    def _wait_until(self, predicate, timeout=5.0, interval=0.05):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if predicate():
                return True
            time.sleep(interval)
        return False


class TestEntriesSurviveRestart(RunnerPersistenceTestBase):
    def test_registration_is_restored_after_restart(self):
        info = self.pm.start(
            "sleeper",
            _cmd("import time; time.sleep(30)"),
            cwd=self._tmpdir.name,
            env={"MY_KEY": "my-value"},
        )

        restarted = self._restart().get(info.id)

        self.assertIsNotNone(restarted, "entry disappeared after restart")
        self.assertEqual(restarted.id, info.id)
        self.assertEqual(restarted.label, "sleeper")
        self.assertEqual(restarted.cmd, info.cmd)
        self.assertEqual(restarted.cwd, self._tmpdir.name)
        self.assertEqual(restarted.env, {"MY_KEY": "my-value"})

    def test_restored_entry_is_stopped_without_pid(self):
        # 재시작 시점에 그 pid 가 여전히 같은 커맨드인지 보증할 수 없으므로
        # 복원 엔트리는 항상 stopped/pid=None 이어야 한다.
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))

        restored = self._restart().get(info.id)

        self.assertEqual(restored.status, "stopped")
        self.assertIsNone(restored.pid)
        self.assertIsNone(restored.exit_code)

    def test_restored_entry_appears_in_list(self):
        first = self.pm.start("a", _cmd("print('a')"))
        second = self.pm.start("b", _cmd("import time; time.sleep(30)"))

        ids = {p.id for p in self._restart().list()}

        self.assertEqual(ids, {first.id, second.id})

    def test_restored_entry_can_be_resumed(self):
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        pm = self._restart()

        resumed = pm.run(info.id)

        self.assertIsNotNone(resumed)
        self.assertEqual(resumed.id, info.id)
        self.assertEqual(resumed.status, "running")
        self.assertIsNotNone(resumed.pid)

    def test_logs_remain_readable_after_restart(self):
        info = self.pm.start("echoer", _cmd("print('before-restart')"))
        self.assertTrue(self._wait_until(lambda: self.pm.get(info.id).status != "running"))

        pm = self._restart()
        result = pm.read_log(info.id, 0)

        self.assertIsNotNone(result, "log read returned not_found after restart")
        self.assertIn("before-restart", result[0])


class TestPersistedMutations(RunnerPersistenceTestBase):
    def test_update_is_persisted(self):
        info = self.pm.start("old-label", _cmd("import time; time.sleep(30)"))
        self.pm.stop(info.id)
        self.pm.update(
            info.id,
            "new-label",
            _cmd("print('updated')"),
            cwd=self._tmpdir.name,
            env={"K": "V"},
        )

        restored = self._restart().get(info.id)

        self.assertEqual(restored.label, "new-label")
        self.assertIn("updated", restored.cmd)
        self.assertEqual(restored.cwd, self._tmpdir.name)
        self.assertEqual(restored.env, {"K": "V"})

    def test_delete_is_persisted(self):
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        self.assertTrue(self.pm.delete(info.id))

        pm = self._restart()

        self.assertIsNone(pm.get(info.id))
        self.assertEqual(pm.list(), [])

    def test_entries_are_isolated_per_database(self):
        self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        other_dir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.addCleanup(other_dir.cleanup)
        other_registry = Registry.open(os.path.join(other_dir.name, "other.db"))
        self.addCleanup(other_registry.close)

        other = ProcessManager(os.path.join(other_dir.name, "logs"), store=other_registry)

        self.assertEqual(other.list(), [])


class TestWithoutStore(unittest.TestCase):
    """store 미주입 시에는 기존과 동일하게 메모리 전용으로 동작해야 한다."""

    def setUp(self):
        self._tmpdir = tempfile.TemporaryDirectory(ignore_cleanup_errors=True)
        self.log_dir = os.path.join(self._tmpdir.name, "logs")

    def tearDown(self):
        self._tmpdir.cleanup()

    def test_entries_are_not_restored_without_store(self):
        first = ProcessManager(self.log_dir)
        first.start("echoer", _cmd("print('x')"))
        first.shutdown(kill_children=True)

        second = ProcessManager(self.log_dir)

        self.assertEqual(second.list(), [])


class TestShutdown(RunnerPersistenceTestBase):
    def test_shutdown_terminates_running_children(self):
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        popen = self.pm._entries[info.id].popen

        self.pm.shutdown(kill_children=True)

        self.assertIsNotNone(popen.poll(), "child process survived shutdown()")

    def test_shutdown_is_idempotent(self):
        self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        self.pm.shutdown(kill_children=True)
        self.pm.shutdown(kill_children=True)  # 두 번째 호출이 예외를 내면 안 된다

    def test_shutdown_can_leave_children_running(self):
        info = self.pm.start("sleeper", _cmd("import time; time.sleep(30)"))
        popen = self.pm._entries[info.id].popen

        self.pm.shutdown(kill_children=False)

        self.assertIsNone(popen.poll(), "child was killed despite kill_children=False")
        self.pm.stop(info.id)


if __name__ == "__main__":
    unittest.main(verbosity=2)
