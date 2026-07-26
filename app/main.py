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
    SPAWNPOINT_KILL_CHILDREN_ON_EXIT
                           Default: 1. Terminate processes started by the runner when the
                           server exits (on Windows this holds even for a hard kill, via a
                           job object). Set to 0 to leave them running — note that the
                           restarted server can no longer stop or restart them from the UI.
"""
from __future__ import annotations

import atexit
import os
import signal
import sys

from runnerview.page import INDEX_HTML
from spawnpoint.auth import AuthValidator
from spawnpoint.http_api import make_server
from spawnpoint.runner import ProcessManager
from spawnpoint.storage import Registry


def _tokens_from_env() -> list[str]:
    raw = os.environ.get("SPAWNPOINT_API_TOKENS", "")
    return [t.strip() for t in raw.split(",") if t.strip()]


def _kill_children_on_exit() -> bool:
    raw = os.environ.get("SPAWNPOINT_KILL_CHILDREN_ON_EXIT", "1").strip().lower()
    return raw not in ("0", "false", "no", "off")


def build() -> tuple[Registry, AuthValidator, ProcessManager, str, int]:
    host = os.environ.get("SPAWNPOINT_HOST", "127.0.0.1")
    port = int(os.environ.get("SPAWNPOINT_PORT", "8091"))
    db_path = os.environ.get("SPAWNPOINT_DB_PATH", "spawnpoint.db")
    log_dir = os.environ.get("SPAWNPOINT_LOG_DIR", "logs")

    registry = Registry.open(db_path)
    auth = AuthValidator(_tokens_from_env())
    # Registry doubles as the runner's store: registered commands are reloaded here,
    # so a restart no longer starts with an empty list.
    procman = ProcessManager(
        log_dir, store=registry, kill_children_on_exit=_kill_children_on_exit()
    )
    return registry, auth, procman, host, port


def _raise_keyboard_interrupt(signum, frame):
    raise KeyboardInterrupt


def _install_shutdown_signals() -> None:
    """Route SIGTERM / console-break to the same path as Ctrl-C.

    Without this the cleanup in main()'s finally block is skipped and the runner's
    children are orphaned — they keep holding ports and file handles while the
    restarted server no longer knows about them.
    """
    for name in ("SIGTERM", "SIGBREAK", "SIGHUP"):
        sig = getattr(signal, name, None)
        if sig is None:
            continue
        try:
            signal.signal(sig, _raise_keyboard_interrupt)
        except (ValueError, OSError, RuntimeError):
            pass


def main() -> None:
    # Windows default console (cp949/cp932) cannot encode startup messages with
    # non-ASCII characters, causing UnicodeEncodeError at start. Switch to UTF-8 if possible.
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

    registry, auth, procman, host, port = build()
    kill_children = _kill_children_on_exit()
    atexit.register(procman.shutdown, kill_children)  # backstop; shutdown() is idempotent

    try:
        server = make_server(
            host, port, registry, auth, index_html=INDEX_HTML, procman=procman
        )
    except OSError as exc:
        # Most often the port is already taken by another SpawnPoint instance.
        # Reporting it here beats starting a server that binds but never answers.
        print(f"Cannot bind {host}:{port} -- {exc}", file=sys.stderr)
        print(
            "  Another instance is probably already listening on this port."
            " Stop it, or set SPAWNPOINT_PORT to a free port.",
            file=sys.stderr,
        )
        registry.close()
        raise SystemExit(1) from exc

    mode = "token validation" if auth.enabled else "disabled (local default)"
    print(f"SpawnPoint listening on http://{host}:{port}  [auth: {mode}]")
    print(f"  UI:     http://{host}:{port}/")
    print(f"  API:    POST http://{host}:{port}/spawn")
    print(f"  Runner: http://{host}:{port}/processes")
    _install_shutdown_signals()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("Shutting down...")
    finally:
        server.server_close()
        procman.shutdown(kill_children=kill_children)
        registry.close()


if __name__ == "__main__":
    main()