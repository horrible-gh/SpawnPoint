"""SpawnPoint application entry point.

Read environment settings to configure storage and authentication, then start server.
Serves API (POST /spawn), UI (GET /), and process runner on one process, one port.

Environment variables:
    SPAWNPOINT_HOST        Default: 127.0.0.1
    SPAWNPOINT_PORT        Default: 8091
    SPAWNPOINT_DB_PATH     Default: spawnpoint.db (SQLite file path)
    SPAWNPOINT_LOG_DIR     Default: logs (log directory for runner-managed processes)
    SPAWNPOINT_API_TOKENS  Comma-separated list of allowed Bearer tokens.
                           If unset, authentication is disabled (default local mode).
"""
from __future__ import annotations

import os
import sys

from runnerview.page import INDEX_HTML
from spawnpoint.auth import AuthValidator
from spawnpoint.http_api import make_server
from spawnpoint.runner import ProcessManager
from spawnpoint.storage import Registry


def _tokens_from_env() -> list[str]:
    raw = os.environ.get("SPAWNPOINT_API_TOKENS", "")
    return [t.strip() for t in raw.split(",") if t.strip()]


def build() -> tuple[Registry, AuthValidator, ProcessManager, str, int]:
    host = os.environ.get("SPAWNPOINT_HOST", "127.0.0.1")
    port = int(os.environ.get("SPAWNPOINT_PORT", "8091"))
    db_path = os.environ.get("SPAWNPOINT_DB_PATH", "spawnpoint.db")
    log_dir = os.environ.get("SPAWNPOINT_LOG_DIR", "logs")

    registry = Registry.open(db_path)
    auth = AuthValidator(_tokens_from_env())
    procman = ProcessManager(log_dir)
    return registry, auth, procman, host, port


def main() -> None:
    # Windows default console (cp949/cp932) cannot encode startup messages with
    # non-ASCII characters, causing UnicodeEncodeError at start. Switch to UTF-8 if possible.
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

    registry, auth, procman, host, port = build()
    server = make_server(
        host, port, registry, auth, index_html=INDEX_HTML, procman=procman
    )
    mode = "token validation" if auth.enabled else "disabled (local default)"
    print(f"SpawnPoint listening on http://{host}:{port}  [auth: {mode}]")
    print(f"  UI:     http://{host}:{port}/")
    print(f"  API:    POST http://{host}:{port}/spawn")
    print(f"  Runner: http://{host}:{port}/processes")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        registry.close()


if __name__ == "__main__":
    main()