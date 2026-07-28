"""SpawnPoint 단위 테스트.

spawn 프로토콜의 네 시나리오와 경계 조건을 검증한다.
표준 라이브러리 unittest만 사용한다. 저장소는 sqloader(SQLite, 임시 파일)를 쓴다.
"""
from __future__ import annotations

import os
import re
import tempfile
import unittest
from datetime import datetime, timedelta

from spawnpoint.auth import AuthValidator
from spawnpoint.clock import KST
from spawnpoint.http_api import handle_spawn, parse_bearer
from spawnpoint.models import SpawnInstance
from spawnpoint.service import spawn
from spawnpoint.storage import Registry

TOKEN = "tok-test-123"
ID_RE = re.compile(r"^spwn_\d{8}_\d{4}[0-9a-f]{6}$")


def _now(h=13, m=40, s=12):
    return datetime(2026, 7, 25, h, m, s, tzinfo=KST)


class SpawnTestBase(unittest.TestCase):
    def setUp(self):
        # sqloader(0.2.18)의 자동 마이그레이션은 파일 기반 SQLite에서만 안정적으로
        # 동작한다(memory_mode 연결은 begin_transaction()이 별도의 독립된
        # ":memory:" 연결을 새로 열어 마이그레이션 결과가 보이지 않는다).
        # 그래서 테스트마다 임시 디렉터리의 파일 DB를 사용한다.
        self._tmpdir = tempfile.TemporaryDirectory()
        self.registry = Registry.open(os.path.join(self._tmpdir.name, "spawnpoint.db"))
        self.auth = AuthValidator([TOKEN])

    def tearDown(self):
        self.registry.close()
        self._tmpdir.cleanup()

    def call(self, request, now=None):
        return spawn(request, now or _now(), self.registry, self.auth)

    def base_request(self, **overrides):
        req = {
            "token": TOKEN,
            "requester": "user-a1b2c3",
            "kind": "session",
            "options": {"label": "nightly-build", "ttl_seconds": 3600},
        }
        req.update(overrides)
        return req


class TestScenario1Success(SpawnTestBase):
    def test_success_returns_created_handle(self):
        res = self.call(self.base_request())
        self.assertTrue(res["ok"])
        self.assertNotIn("deduplicated", res)
        inst = res["instance"]
        self.assertEqual(inst["status"], "created")
        self.assertEqual(inst["kind"], "session")
        self.assertEqual(inst["requester"], "user-a1b2c3")
        self.assertTrue(ID_RE.match(inst["id"]), inst["id"])
        self.assertIn("+09:00", inst["created_at"])

    def test_persisted_row_and_expiry(self):
        res = self.call(self.base_request(request_key="rk-1"))
        ident = res["instance"]["id"]
        row = self.registry._db.fetch_one(
            "SELECT ttl_seconds, created_at, expires_at FROM spawn_instance WHERE id=?",
            (ident,),
        )
        self.assertEqual(row["ttl_seconds"], 3600)
        # expires_at == created_at + ttl
        from spawnpoint.clock import from_utc_iso

        created = from_utc_iso(row["created_at"])
        expires = from_utc_iso(row["expires_at"])
        self.assertEqual(expires - created, timedelta(seconds=3600))


class TestScenario2Invalid(SpawnTestBase):
    def test_empty_kind_rejected(self):
        res = self.call(self.base_request(kind=""))
        self.assertFalse(res["ok"])
        self.assertEqual(res["error"]["code"], "invalid_request")
        self.assertEqual(res["error"]["field"], "kind")

    def test_disallowed_kind_rejected(self):
        res = self.call(self.base_request(kind="daemon"))
        self.assertEqual(res["error"]["field"], "kind")

    def test_empty_requester_rejected(self):
        res = self.call(self.base_request(requester="   "))
        self.assertEqual(res["error"]["field"], "requester")

    def test_requester_too_long(self):
        res = self.call(self.base_request(requester="x" * 65))
        self.assertEqual(res["error"]["field"], "requester")

    def test_label_too_long(self):
        res = self.call(self.base_request(options={"label": "y" * 257}))
        self.assertEqual(res["error"]["field"], "options.label")

    def test_ttl_below_min(self):
        res = self.call(self.base_request(options={"ttl_seconds": 59}))
        self.assertEqual(res["error"]["field"], "options.ttl_seconds")

    def test_ttl_above_max(self):
        res = self.call(self.base_request(options={"ttl_seconds": 86401}))
        self.assertEqual(res["error"]["field"], "options.ttl_seconds")

    def test_ttl_bool_rejected(self):
        # bool은 int 하위형이지만 ttl로 허용하지 않는다.
        res = self.call(self.base_request(options={"ttl_seconds": True}))
        self.assertEqual(res["error"]["field"], "options.ttl_seconds")

    def test_ttl_default_when_missing(self):
        res = self.call(self.base_request(options={}))
        self.assertTrue(res["ok"])
        row = self.registry._db.fetch_one(
            "SELECT ttl_seconds FROM spawn_instance WHERE id=?",
            (res["instance"]["id"],),
        )
        self.assertEqual(row["ttl_seconds"], 3600)  # ttl_default


class TestScenario3Dedup(SpawnTestBase):
    def test_same_key_within_window_deduplicated(self):
        first = self.call(self.base_request(request_key="req-7f3a90"), now=_now())
        second = self.call(
            self.base_request(request_key="req-7f3a90"),
            now=_now() + timedelta(seconds=120),
        )
        self.assertTrue(second["ok"])
        self.assertTrue(second["deduplicated"])
        self.assertEqual(second["instance"]["id"], first["instance"]["id"])
        # 저장소에는 1건만 존재해야 한다.
        row = self.registry._db.fetch_one("SELECT COUNT(*) AS n FROM spawn_instance")
        self.assertEqual(row["n"], 1)

    def test_no_request_key_always_new(self):
        a = self.call(self.base_request())
        b = self.call(self.base_request())
        self.assertNotEqual(a["instance"]["id"], b["instance"]["id"])

    def test_window_lower_bound_is_open(self):
        # created_at == now - dedup_window (정확히 경계) → 창 밖 → 신규 생성.
        first = self.call(self.base_request(request_key="rk-edge"), now=_now())
        later = _now() + timedelta(seconds=300)  # DEDUP_WINDOW_SECONDS
        second = self.call(self.base_request(request_key="rk-edge"), now=later)
        self.assertTrue(second["ok"])
        self.assertNotIn("deduplicated", second)
        self.assertNotEqual(second["instance"]["id"], first["instance"]["id"])

    def test_just_inside_window_deduplicated(self):
        first = self.call(self.base_request(request_key="rk-in"), now=_now())
        later = _now() + timedelta(seconds=299)
        second = self.call(self.base_request(request_key="rk-in"), now=later)
        self.assertTrue(second.get("deduplicated"))
        self.assertEqual(second["instance"]["id"], first["instance"]["id"])


class TestScenario4Auth(SpawnTestBase):
    def test_missing_token_unauthorized(self):
        res = self.call(self.base_request(token=None))
        self.assertFalse(res["ok"])
        self.assertEqual(res["error"]["code"], "unauthorized")

    def test_wrong_token_unauthorized(self):
        res = self.call(self.base_request(token="nope"))
        self.assertEqual(res["error"]["code"], "unauthorized")

    def test_auth_disabled_accepts_missing_token(self):
        local = AuthValidator()  # 토큰 미설정 → 인증 비활성화
        res = spawn(self.base_request(token=None), _now(), self.registry, local)
        self.assertTrue(res["ok"])


class TestAllocator(SpawnTestBase):
    def test_daily_seq_increments(self):
        a = self.call(self.base_request())["instance"]["id"]
        b = self.call(self.base_request())["instance"]["id"]
        seq_a = int(a.split("_")[2][:4])
        seq_b = int(b.split("_")[2][:4])
        self.assertEqual(seq_b, seq_a + 1)

    def test_id_contains_local_date(self):
        ident = self.call(self.base_request())["instance"]["id"]
        self.assertEqual(ident.split("_")[1], "20260725")


class TestStorageDuplicateKey(SpawnTestBase):
    def test_insert_duplicate_id_reports_duplicate_key(self):
        inst = SpawnInstance(
            id="spwn_20260725_0001abcdef",
            requester="u",
            kind="task",
            status="created",
            request_key=None,
            label=None,
            ttl_seconds=3600,
            created_at=_now(),
            expires_at=_now() + timedelta(seconds=3600),
        )
        self.assertTrue(self.registry.insert(inst).ok)
        second = self.registry.insert(inst)
        self.assertFalse(second.ok)
        self.assertEqual(second.reason, "duplicate_key")

    def test_check_constraint_rejects_bad_kind(self):
        inst = SpawnInstance(
            id="spwn_20260725_0002abcdef",
            requester="u",
            kind="bogus",  # ck_kind 위반
            status="created",
            request_key=None,
            label=None,
            ttl_seconds=3600,
            created_at=_now(),
            expires_at=_now() + timedelta(seconds=3600),
        )
        res = self.registry.insert(inst)
        self.assertFalse(res.ok)
        self.assertEqual(res.reason, "constraint")


class TestHttpAdapter(SpawnTestBase):
    def test_parse_bearer(self):
        self.assertEqual(parse_bearer({"Authorization": "Bearer abc"}), "abc")
        self.assertIsNone(parse_bearer({"Authorization": "Basic abc"}))
        self.assertIsNone(parse_bearer({}))

    def test_handle_spawn_success(self):
        import json

        body = json.dumps(
            {"requester": "u", "kind": "worker", "options": {}}
        ).encode()
        status, payload = handle_spawn(
            {"Authorization": f"Bearer {TOKEN}"},
            body,
            _now(),
            self.registry,
            self.auth,
        )
        self.assertEqual(status, 200)
        self.assertTrue(payload["ok"])

    def test_handle_spawn_unauthorized_status(self):
        import json

        body = json.dumps({"requester": "u", "kind": "worker"}).encode()
        status, payload = handle_spawn({}, body, _now(), self.registry, self.auth)
        self.assertEqual(status, 401)

    def test_handle_spawn_invalid_json(self):
        status, payload = handle_spawn(
            {"Authorization": f"Bearer {TOKEN}"},
            b"{not json",
            _now(),
            self.registry,
            self.auth,
        )
        self.assertEqual(status, 400)
        self.assertEqual(payload["error"]["code"], "invalid_request")

    def test_handle_spawn_invalid_field_status(self):
        import json

        body = json.dumps({"requester": "u", "kind": ""}).encode()
        status, payload = handle_spawn(
            {"Authorization": f"Bearer {TOKEN}"},
            body,
            _now(),
            self.registry,
            self.auth,
        )
        self.assertEqual(status, 400)
        self.assertEqual(payload["error"]["code"], "invalid_request")


if __name__ == "__main__":
    unittest.main(verbosity=2)
