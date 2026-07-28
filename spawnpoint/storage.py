"""Instance storage.

Registry backed by SQLite, responsible for instance registration (Registrar) + ID sequence allocation
+ runner entry persistence (so registered commands survive a server restart).
Tables, constraints, indexes are in migration files (spawnpoint/sql/migration/sqlite);
queries are in sqloader query files (spawnpoint/sql/sqlite), separate from code.

Component mapping:
- registry.insert(...)              → Instance Registrar write
- registry.find_active_by_key(...)  → Duplicate request detection read
- registry.next_daily_seq(...)      → Allocator support (atomic sequence)
- registry.save_runner_entry(...)   → Runner entry store write (ProcessManager)
- registry.list_runner_entries()    → Runner entry restore read (ProcessManager)
"""
from __future__ import annotations

import json
import os
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

from sqloader import SQLiteWrapper, SQLoader, DatabaseMigrator

from .clock import from_utc_iso, to_utc_iso
from .models import SpawnInstance

_PACKAGE_DIR = os.path.dirname(os.path.abspath(__file__))
_SQL_DIR = os.path.join(_PACKAGE_DIR, "sql", "sqlite")
_MIGRATION_DIR = os.path.join(_PACKAGE_DIR, "sql", "migration", "sqlite")


# The two renderings live in spawnpoint.clock so that callers outside this
# module — and the tests — have one name for them rather than reaching into a
# private helper here.
_to_utc_iso = to_utc_iso
_from_utc_iso = from_utc_iso


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
        # auto_run=True: immediately apply unapplied migrations (DDL)
        self._migrator = DatabaseMigrator(self._db, _MIGRATION_DIR, auto_run=True)

    @classmethod
    def open(cls, db_path: str) -> "Registry":
        return cls(db_path)

    def close(self) -> None:
        self._db.close()

    # --- ID sequence allocation ----------

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

    # --- Instance registration ----

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

    # --- Duplicate detection query ---

    def find_active_by_key(
        self, request_key: str | None, now: datetime, dedup_window: int
    ) -> SpawnInstance | None:
        """Fetch the latest instance with same request_key where created_at > (now - dedup_window).

        Lower bound is open interval (boundary excluded) per the 'time window boundary' rule.
        Comparison uses '>' (strict).
        """
        if request_key is None:
            return None
        threshold = _to_utc_iso(now - timedelta(seconds=dedup_window))
        row = self._sq.fetch_one(
            "spawn_instance", "find_active_by_key", (request_key, threshold)
        )
        return _row_to_instance(row) if row else None

    # --- Runner entry persistence ---

    def save_runner_entry(
        self,
        ident: str,
        label: str,
        cmd: str,
        cwd: str | None,
        env: dict | None,
    ) -> bool:
        """Insert or update one runner entry (registration data only, no live state).

        Returns False on storage failure — the runner keeps working in memory, it
        just loses the entry on the next restart.
        """
        ts = _to_utc_iso(datetime.now(timezone.utc))
        payload = json.dumps(dict(env or {}), ensure_ascii=False)
        try:
            self._sq.execute(
                "runner_entry",
                "upsert",
                (ident, label, cmd, cwd, payload, ts, ts),
            )
            return True
        except sqlite3.Error:
            return False

    def delete_runner_entry(self, ident: str) -> bool:
        try:
            self._sq.execute("runner_entry", "delete", (ident,))
            return True
        except sqlite3.Error:
            return False

    def list_runner_entries(self) -> list[dict]:
        """Return persisted runner entries (env decoded). Empty list on read failure."""
        try:
            rows = self._sq.fetch_all("runner_entry", "list")
        except sqlite3.Error:
            return []
        return [
            {
                "id": row["id"],
                "label": row["label"],
                "cmd": row["cmd"],
                "cwd": row["cwd"],
                "env": _decode_env(row["env"]),
            }
            for row in rows or []
        ]


def _decode_env(raw) -> dict[str, str]:
    """Parse the stored env JSON. Corrupted values degrade to {} rather than blocking restore."""
    if not raw:
        return {}
    try:
        env = json.loads(raw)
    except (ValueError, TypeError):
        return {}
    if not isinstance(env, dict):
        return {}
    return {str(k): str(v) for k, v in env.items()}


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
