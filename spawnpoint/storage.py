"""Instance storage (DB-0007).

Registry backed by SQLite, responsible for instance registration (Registrar) + ID sequence allocation.
DB-0007 tables, constraints, indexes are in migration files (spawnpoint/sql/migration/sqlite);
queries are in sqloader query files (spawnpoint/sql/sqlite), separate from code.

D-0004 component mapping:
- registry.insert(...)             → Instance Registrar write
- registry.find_active_by_key(...) → Duplicate request detection read
- registry.next_daily_seq(...)     → Allocator support (atomic sequence)
"""
from __future__ import annotations

import os
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

from sqloader import SQLiteWrapper, SQLoader, DatabaseMigrator

from .models import SpawnInstance

_PACKAGE_DIR = os.path.dirname(os.path.abspath(__file__))
_SQL_DIR = os.path.join(_PACKAGE_DIR, "sql", "sqlite")
_MIGRATION_DIR = os.path.join(_PACKAGE_DIR, "sql", "migration", "sqlite")


def _to_utc_iso(dt: datetime) -> str:
    """Convert datetime to UTC ISO 8601 fixed-width format."""
    utc_dt = dt.astimezone(timezone.utc)
    return utc_dt.strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def _from_utc_iso(s: str) -> datetime:
    """Parse UTC ISO 8601 string to datetime."""
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


@dataclass(frozen=True)
class WriteResult:
    """insert result. reason ∈ {None, 'duplicate_key', 'constraint', 'error'}."""

    ok: bool
    reason: str | None = None


class Registry:
    """SQLite-backed instance store using sqloader."""

    def __init__(self, db_path: str):
        self._db = SQLiteWrapper(db_name=db_path)
        self._sq = SQLoader(_SQL_DIR, db_type=self._db.db_type, db=self._db)
        # auto_run=True: immediately apply unapplied migrations (DB-0007 §2 DDL)
        self._migrator = DatabaseMigrator(self._db, _MIGRATION_DIR, auto_run=True)

    @classmethod
    def open(cls, db_path: str) -> "Registry":
        return cls(db_path)

    def close(self) -> None:
        self._db.close()

    # --- ID sequence allocation (L-0006 §2.3, DB-0007 §2.2) ----------

    def next_seq(self, date_part: str) -> int:
        """Atomically increment the sequence for this date and return new value.

        If row does not exist, create it with 1. sqloader's file-mode SQLiteWrapper opens
        a new connection per call (sqloader 0.2.18 SQLiteWrapper._execute_file), so to
        wrap UPSERT and subsequent query as one atomic unit, use begin_transaction()—
        otherwise the two calls happen in separate connections/transactions.
        """
        now_utc = datetime.now(timezone.utc)
        ts = _to_utc_iso(now_utc)
        upsert_sql = self._sq.load_sql("spawn_daily_seq", "upsert")
        select_sql = self._sq.load_sql("spawn_daily_seq", "select_last_seq")
        with self._db.begin_transaction() as txn:
            txn.execute(upsert_sql, (date_part, ts))
            row = txn.fetch_one(select_sql, (date_part,))
        return int(row["last_seq"])

    # --- Instance registration (L-0006 §2.1) ----

    def insert(self, inst: SpawnInstance) -> WriteResult:
        """Save new instance. Report duplicate_key on ID collision."""
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
                    _to_utc_iso(inst.created_at),
                    _to_utc_iso(inst.expires_at),
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

    # --- Duplicate detection query (L-0006 §2.4) ---

    def find_active_by_key(
        self, request_key: str | None, now: datetime, dedup_window: int
    ) -> SpawnInstance | None:
        """Fetch the latest instance with same request_key where created_at > (now - dedup_window).

        Lower bound is open interval (boundary excluded) per L-0006 §5 'time window boundary' rule.
        Comparison uses '>' (strict).
        """
        if request_key is None:
            return None
        threshold = _to_utc_iso(now - timedelta(seconds=dedup_window))
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
        created_at=_from_utc_iso(row["created_at"]),
        expires_at=_from_utc_iso(row["expires_at"]),
    )