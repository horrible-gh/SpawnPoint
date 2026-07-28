"""Capture the schema and migration history the current Python implementation produces.

This is the reference generator for the Go rewrite's initial-configuration
contract (0008-L 2.9 / 6.3 item 4). It opens a Registry against an empty
temporary file — which runs sqloader's DatabaseMigrator over the real migration
scripts — and dumps what ended up on disk.

The Go migration runner is then required to produce the same thing from the same
scripts. That is what makes "the schema matches the existing database" a check a
machine performs rather than a claim someone reads, and it is what protects the
rule that matters most: an existing database must not have its migrations
applied a second time (0004-NR R1-R8).

Output: JSON on stdout, written to internal/store/testdata/python_schema.json.

    python tools/pyref/dump_schema.py > internal/store/testdata/python_schema.json

The temporary database is discarded. Nothing touches the deployed file.
"""
from __future__ import annotations

import contextlib
import json
import os
import sqlite3
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))


def dump(db_path: str) -> dict:
    """Read the schema and the migration history out of a finished database."""
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    try:
        objects = [
            {
                "type": row["type"],
                "name": row["name"],
                "tbl_name": row["tbl_name"],
                # sql is NULL for the indexes SQLite creates on its own for a
                # PRIMARY KEY or UNIQUE constraint. Those are part of the schema
                # but have no text, so the comparison has to tolerate the null
                # rather than skip the row.
                "sql": row["sql"],
            }
            # sqlite_master's own row order depends on the order objects were
            # created, which is exactly what is under test — but comparing it
            # would also make the result depend on how SQLite happens to append.
            # Sorting by (type, name) compares the content of the schema and
            # leaves the ordering to the separate migration-order check.
            for row in conn.execute(
                "SELECT type, name, tbl_name, sql FROM sqlite_master"
                " WHERE name NOT LIKE 'sqlite_autoindex%'"
                " ORDER BY type, name"
            )
        ]
        migrations = [
            row["filename"]
            for row in conn.execute(
                "SELECT filename FROM migrations ORDER BY filename"
            )
        ]
    finally:
        conn.close()
    return {"objects": objects, "migrations": migrations}


def main() -> int:
    from spawnpoint.storage import Registry

    with tempfile.TemporaryDirectory(prefix="pyref-schema-") as work:
        db_path = os.path.join(work, "reference.db")
        # sqloader's migrator announces each applied file on stdout, which is
        # where the fixture is going. Send it to stderr so the redirect that
        # writes this file captures JSON and nothing else.
        with contextlib.redirect_stdout(sys.stderr):
            registry = Registry.open(db_path)
        try:
            result = dump(db_path)
        finally:
            registry.close()

    result = {
        "_generator": "tools/pyref/dump_schema.py",
        "_python": sys.version.split()[0],
        "_sqlite": sqlite3.sqlite_version,
        **result,
    }
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
