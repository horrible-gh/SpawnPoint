"""HTTP adapter — endpoint POST /spawn (protocol P-0005) + operational auxiliary routes.

Uses only the standard library (http.server). Core logic is isolated as a pure function in
handle_spawn(), so tests can call it directly without sockets.

The screen (Process Runner UI) is not a separate process but served by this same server on the
same port. Inject HTML bytes via make_server(..., index_html=...) and GET / , GET /index.html
will serve that screen (omit it and those routes also return 404).

Routes:
    POST /spawn                    Instance creation (P-0005)
    GET  / , /index.html           Screen (if index_html injected)
    GET  /healthz                  Liveness check
    GET  /processes                Runner: process list
    POST /processes                Runner: start new process
    POST /processes/<id>/stop      Runner: terminate (process tree)
    POST /processes/<id>/restart   Runner: restart
    GET  /processes/<id>/logs      Runner: log tail (?offset=<bytes>)
    others                         404 (unknown path) / 405 (unsupported method)

/processes* routes use the same auth policy as /spawn. In default local mode (no tokens),
they work without authentication.

HTTP status mapping (POST /spawn):
    ok=true            -> 200
    unauthorized       -> 401
    invalid_request    -> 400
    payload_too_large  -> 413
    storage_error      -> 500
"""
from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .auth import AuthValidator
from .clock import now_kst
from .results import error
from .runner import ProcessManager
from .service import spawn
from .storage import Registry

# Request body limit. /spawn requests are small, so generously limit to 64KiB (resource protection).
MAX_BODY_BYTES = 64 * 1024

# GET paths that serve the static screen view.
_INDEX_PATHS = {"/", "/index.html"}

_STATUS_BY_CODE = {
    "unauthorized": 401,
    "invalid_request": 400,
    "payload_too_large": 413,
    "storage_error": 500,
    "not_found": 404,
    "method_not_allowed": 405,
}


def parse_bearer(headers) -> str | None:
    """Extract Bearer token from Authorization header (None if missing or malformed)."""
    raw = headers.get("Authorization") or headers.get("authorization")
    if not raw:
        return None
    parts = raw.split(None, 1)
    if len(parts) != 2 or parts[0].lower() != "bearer":
        return None
    token = parts[1].strip()
    return token or None


def handle_spawn(headers, body_bytes: bytes, now, registry, auth):
    """Transport-independent handler that returns (status_code, response_dict)."""
    if body_bytes is not None and len(body_bytes) > MAX_BODY_BYTES:
        return 413, error(
            "payload_too_large", None, "Request body is too large."
        )

    token = parse_bearer(headers)

    try:
        payload = json.loads(body_bytes.decode("utf-8")) if body_bytes else {}
    except (ValueError, UnicodeDecodeError):
        return 400, error("invalid_request", None, "Request body is not valid JSON.")
    if not isinstance(payload, dict):
        return 400, error("invalid_request", None, "Request body must be an object.")

    request = dict(payload)
    request["token"] = token

    result = spawn(request, now, registry, auth)
    if result.get("ok"):
        return 200, result

    code = result["error"]["code"]
    return _STATUS_BY_CODE.get(code, 400), result


# Process runner request body limit — includes command/env, so allow more.
MAX_PROCESS_BODY_BYTES = 64 * 1024

_CMD_MAX_LEN = 4096
_LABEL_MAX_LEN = 128


def _check_auth(headers, auth: AuthValidator) -> bool:
    return auth.check(parse_bearer(headers))


def handle_processes_list(headers, procman, auth):
    """(status_code, response_dict) — GET /processes."""
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "Valid authentication token required.")
    processes = [p.to_public() for p in procman.list()]
    return 200, {"ok": True, "processes": processes}


def handle_process_start(
    headers, body_bytes: bytes, procman, auth, ident: str | None = None
):
    """(status_code, response_dict) — POST /processes or PUT /processes/<id>."""
    if body_bytes is not None and len(body_bytes) > MAX_PROCESS_BODY_BYTES:
        return 413, error("payload_too_large", None, "Request body is too large.")
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "Valid authentication token required.")

    try:
        payload = json.loads(body_bytes.decode("utf-8")) if body_bytes else {}
    except (ValueError, UnicodeDecodeError):
        return 400, error("invalid_request", None, "Request body is not valid JSON.")
    if not isinstance(payload, dict):
        return 400, error("invalid_request", None, "Request body must be an object.")

    cmd = payload.get("cmd")
    if not isinstance(cmd, str) or not cmd.strip():
        return 400, error("invalid_request", "cmd", "cmd cannot be empty.")
    if len(cmd) > _CMD_MAX_LEN:
        return 400, error("invalid_request", "cmd", "cmd is too long.")

    label = payload.get("label")
    if label is not None and (not isinstance(label, str) or not label.strip()):
        return 400, error("invalid_request", "label", "label must be a string.")
    if isinstance(label, str) and len(label) > _LABEL_MAX_LEN:
        return 400, error("invalid_request", "label", "label is too long.")
    if not label:
        label = cmd.split(None, 1)[0]

    cwd = payload.get("cwd")
    if cwd is not None and (not isinstance(cwd, str) or not cwd.strip()):
        return 400, error("invalid_request", "cwd", "cwd must be a string.")

    env = payload.get("env")
    if env is not None:
        if not isinstance(env, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in env.items()
        ):
            return 400, error(
                "invalid_request", "env", "env must be an object with string keys and values."
            )

    if ident is None:
        info = procman.start(label, cmd, cwd=cwd, env=env)
    else:
        info = procman.update(ident, label, cmd, cwd=cwd, env=env)
        if info is None:
            return 404, error("not_found", None, "Process not found.")
    return 200, {"ok": True, "process": info.to_public()}


def handle_process_action(ident: str, action: str, headers, procman, auth):
    """(status_code, response_dict) — POST /processes/<id>/stop|restart."""
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "Valid authentication token required.")
    if action == "stop":
        info = procman.stop(ident)
    elif action == "restart":
        info = procman.restart(ident)
    elif action == "run":
        info = procman.run(ident)
    else:
        return 404, error("not_found", None, "Unknown endpoint.")
    if info is None:
        return 404, error("not_found", None, "Process not found.")
    return 200, {"ok": True, "process": info.to_public()}


def handle_process_logs(ident: str, headers, offset: int, procman, auth):
    """(status_code, response_dict) — GET /processes/<id>/logs?offset=<n>."""
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "Valid authentication token required.")
    result = procman.read_log(ident, offset)
    if result is None:
        return 404, error("not_found", None, "Process not found.")
    text, next_offset = result
    return 200, {"ok": True, "text": text, "next_offset": next_offset}


class _SpawnHandler(BaseHTTPRequestHandler):
    server_version = "SpawnPoint/0.1"

    # Injected by server instance.
    registry: Registry
    auth: AuthValidator
    procman: ProcessManager | None = None  # Runner (process runner); 404 if not injected
    index_html: bytes | None = None  # Screen HTML; screen routes also 404 if not injected

    def _send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _send_html(self, status: int, body: bytes) -> None:
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _path(self) -> str:
        return self.path.split("?", 1)[0]

    def _query(self) -> dict:
        if "?" not in self.path:
            return {}
        from urllib.parse import parse_qs

        return {k: v[-1] for k, v in parse_qs(self.path.split("?", 1)[1]).items()}

    def _read_body(self, limit: int) -> bytes | None:
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = 0
        if length > limit:
            return None
        return self.rfile.read(length) if length > 0 else b""

    def do_POST(self) -> None:  # noqa: N802 (http.server convention)
        path = self._path()
        parts = [p for p in path.split("/") if p]

        if path == "/spawn":
            body = self._read_body(MAX_BODY_BYTES)
            if body is None:
                self._send_json(
                    413, error("payload_too_large", None, "Request body is too large.")
                )
                return
            status, payload = handle_spawn(
                self.headers, body, now_kst(), self.registry, self.auth
            )
            self._send_json(status, payload)
            return

        if self.procman is not None and parts and parts[0] == "processes":
            if len(parts) == 1:
                body = self._read_body(MAX_PROCESS_BODY_BYTES)
                if body is None:
                    self._send_json(
                        413, error("payload_too_large", None, "Request body is too large.")
                    )
                    return
                status, payload = handle_process_start(
                    self.headers, body, self.procman, self.auth
                )
                self._send_json(status, payload)
                return
            if len(parts) == 3 and parts[2] in ("run", "stop", "restart"):
                status, payload = handle_process_action(
                    parts[1], parts[2], self.headers, self.procman, self.auth
                )
                self._send_json(status, payload)
                return

        self._send_json(404, error("not_found", None, "Unknown endpoint."))

    def do_GET(self) -> None:  # noqa: N802
        path = self._path()
        if path in _INDEX_PATHS and self.index_html is not None:
            self._send_html(200, self.index_html)
            return
        if path == "/healthz":
            self._send_json(200, {"ok": True, "status": "healthy"})
            return

        parts = [p for p in path.split("/") if p]
        if self.procman is not None and parts and parts[0] == "processes":
            if len(parts) == 1:
                status, payload = handle_processes_list(
                    self.headers, self.procman, self.auth
                )
                self._send_json(status, payload)
                return
            if len(parts) == 3 and parts[2] == "logs":
                try:
                    offset = int(self._query().get("offset", "0"))
                except ValueError:
                    offset = 0
                status, payload = handle_process_logs(
                    parts[1], self.headers, max(offset, 0), self.procman, self.auth
                )
                self._send_json(status, payload)
                return

        self._send_json(404, error("not_found", None, "Unknown endpoint."))

    def do_PUT(self) -> None:  # noqa: N802
        path = self._path()
        parts = [p for p in path.split("/") if p]
        if self.procman is not None and len(parts) == 2 and parts[0] == "processes":
            body = self._read_body(MAX_PROCESS_BODY_BYTES)
            if body is None:
                self._send_json(
                    413, error("payload_too_large", None, "Request body is too large.")
                )
                return
            status, payload = handle_process_start(
                self.headers,
                body,
                self.procman,
                self.auth,
                ident=parts[1],
            )
            self._send_json(status, payload)
            return
        self._method_not_allowed()

    def do_DELETE(self) -> None:  # noqa: N802
        path = self._path()
        parts = [p for p in path.split("/") if p]
        if self.procman is not None and len(parts) == 2 and parts[0] == "processes":
            if not _check_auth(self.headers, self.auth):
                self._send_json(401, error("unauthorized", None, "Valid authentication token required."))
                return
            if not self.procman.delete(parts[1]):
                self._send_json(404, error("not_found", None, "Process not found."))
                return
            self._send_json(200, {"ok": True, "deleted_id": parts[1]})
            return
        self._method_not_allowed()

    def do_HEAD(self) -> None:  # noqa: N802
        self.do_GET()

    def _method_not_allowed(self) -> None:
        self._send_json(
            405, error("method_not_allowed", None, "Method not allowed.")
        )

    # Unsupported methods return 501 by default; instead send explicit 405 (JSON).
    do_PATCH = _method_not_allowed

    def log_message(self, fmt, *args):  # Suppress default stderr access logs
        return


def make_server(
    host: str,
    port: int,
    registry: Registry,
    auth: AuthValidator,
    index_html: bytes | None = None,
    procman: ProcessManager | None = None,
):
    """Handle POST /spawn + (if index_html injected) screen + (if procman injected) /processes* together.

    Does not start serving yet. Pass index_html to serve that HTML on GET / , /index.html on the same port—
    no need to start a separate process for the screen. Pass procman to enable runner routes (/processes*);
    omit it and those return 404.
    """
    handler = type(
        "BoundSpawnHandler",
        (_SpawnHandler,),
        {
            "registry": registry,
            "auth": auth,
            "index_html": index_html,
            "procman": procman,
        },
    )
    return ThreadingHTTPServer((host, port), handler)