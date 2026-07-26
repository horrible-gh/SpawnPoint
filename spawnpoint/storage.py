"""인스턴스 저장소 (DB-0007).

인스턴스 등록부(Registrar) + 식별자 순번 발급을 담당하는 SQLite 기반 Registry.
DB-0007의 테이블·제약·인덱스는 마이그레이션 파일(spawnpoint/sql/migration/sqlite)로,
쿼리는 sqloader 쿼리 파일(spawnpoint/sql/sqlite)로 코드 밖에 분리한다.

D-0004의 컴포넌트 대응:
- registry.insert(...)             → 인스턴스 등록부(Registrar) 쓰기
- registry.find_active_by_key(...) → 동일 요청 존재 여부 읽기
- registry.next_daily_seq(...)     → 식별자 발급부(Allocator) 지원(원자적 순번)
"""
from __future__ import annotations

import os
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timedelta

from sqloader import SQLiteWrapper, SQLoader, DatabaseMigrator

from .clock import to_utc_iso, from_utc_iso
from .models import SpawnInstance

_PACKAGE_DIR = os.path.dirname(os.path.abspath(__file__))
_SQL_DIR = os.path.join(_PACKAGE_DIR, "sql", "sqlite")
_MIGRATION_DIR = os.path.join(_PACKAGE_DIR, "sql", "migration", "sqlite")


@dataclass(frozen=True)
class WriteResult:
    """insert 결과. reason ∈ {None, 'duplicate_key', 'constraint', 'error'}."""

    ok: bool
    reason: str | None = None


class Registry:
    """sqloader(SQLite 백엔드) 기반 인스턴스 저장소."""

    def __init__(self, db_path: str):
        self._db = SQLiteWrapper(db_name=db_path)
        self._sq = SQLoader(_SQL_DIR, db_type=self._db.db_type, db=self._db)
        # auto_run=True: 미적용 마이그레이션(DB-0007 §2 DDL)을 즉시 적용한다.
        self._migrator = DatabaseMigrator(self._db, _MIGRATION_DIR, auto_run=True)

    @classmethod
    def open(cls, db_path: str) -> "Registry":
        return cls(db_path)

    def close(self) -> None:
        self._db.close()

    # --- 식별자 순번 발급 (L-0006 §2.3, DB-0007 §2.2) --------------------

    def next_daily_seq(self, date_part: str, now: datetime) -> int:
        """해당 날짜의 순번을 원자적으로 1 증가시키고 새 값을 반환한다.

        행이 없으면 1로 생성한다. sqloader의 파일모드 SQLiteWrapper는 호출마다
        새 연결을 여는 방식이라(sqloader 0.2.18 SQLiteWrapper._execute_file),
        UPSERT와 뒤이은 조회를 하나의 원자 단위로 묶으려면 begin_transaction()으로
        직접 감싸야 한다 — 그렇지 않으면 두 호출이 서로 다른 연결/트랜잭션이 된다.
        """
        ts = to_utc_iso(now)
        upsert_sql = self._sq.load_sql("spawn_daily_seq", "upsert")
        select_sql = self._sq.load_sql("spawn_daily_seq", "select_last_seq")
        with self._db.begin_transaction() as txn:
            txn.execute(upsert_sql, (date_part, ts))
            row = txn.fetch_one(select_sql, (date_part,))
        return int(row["last_seq"])

    # --- 인스턴스 등록 (L-0006 §2.1) ------------------------------------

    def insert(self, inst: SpawnInstance) -> WriteResult:
        """새 인스턴스를 저장한다. 식별자 충돌 시 duplicate_key로 보고한다."""
        try:
            self._sq.execute(
                "spawn_instance",
                "insert",
                (
                    inst.id,
                    inst.requester,
                    inst.kind,
                    inst.status,
                    inst.request_key,
                    inst.label,
                    inst.ttl_seconds,
                    to_utc_iso(inst.created_at),
                    to_utc_iso(inst.expires_at),
                ),
            )
            return WriteResult(True, None)
        except sqlite3.IntegrityError as exc:
            msg = str(exc).lower()
            if "unique" in msg or "primary key" in msg:
                return WriteResult(False, "duplicate_key")
            return WriteResult(False, "constraint")
        except sqlite3.Error:
            return WriteResult(False, "error")

    # --- 중복 판정 조회 (L-0006 §2.4) -----------------------------------

    def find_active_by_key(
        self, request_key: str | None, now: datetime, dedup_window: int
    ) -> SpawnInstance | None:
        """created_at 이 (now - dedup_window) '이후'인 동일 request_key 최신 인스턴스.

        하한은 개구간(경계 미포함)이다 — L-0006 §5의 '시간 창 경계' 규칙.
        따라서 비교는 '>' (엄격)로 수행한다.
        """
        if request_key is None:
            return None
        threshold = to_utc_iso(now - timedelta(seconds=dedup_window))
        row = self._sq.fetch_one(
            "spawn_instance", "find_active_by_key", (request_key, threshold)
        )
        return _row_to_instance(row) if row else None


def _row_to_instance(row) -> SpawnInstance:
    return SpawnInstance(
        id=row["id"],
        requester=row["requester"],
        kind=row["kind"],
        status=row["status"],
        request_key=row["request_key"],
        label=row["label"],
        ttl_seconds=int(row["ttl_seconds"]),
        created_at=from_utc_iso(row["created_at"]),
        expires_at=from_utc_iso(row["expires_at"]),
    )
