"""HTTP 어댑터 — 엔드포인트 POST /spawn (프로토콜 P-0005) + 운영 부가 라우트.

표준 라이브러리(http.server)만 사용한다. 핵심 처리는 handle_spawn()에
순수 함수로 분리하여, 소켓 없이도 테스트가 직접 호출할 수 있게 한다.

화면(실행기 시안 정적 뷰)은 별도 프로세스가 아니라 이 서버가 같은 포트에서
함께 서빙한다. make_server(..., index_html=...) 로 HTML 바이트를 주입하면
GET / , GET /index.html 에서 그 화면을 돌려준다(미주입 시 해당 경로도 404).

라우트:
    POST /spawn                    인스턴스 생성 (P-0005)
    GET  / , /index.html           화면(index_html 주입 시)
    GET  /healthz                  라이브니스 점검
    GET  /processes                실행기: 프로세스 목록
    POST /processes                실행기: 새 프로세스 실행
    POST /processes/<id>/stop      실행기: 정지(프로세스 트리 종료)
    POST /processes/<id>/restart   실행기: 재시작
    GET  /processes/<id>/logs      실행기: 로그 tail (?offset=<바이트>)
    그 외                          404 (알 수 없는 경로) / 405 (미지원 메서드)

/processes* 라우트는 /spawn과 동일한 인증 정책을 사용한다. 토큰 설정이 없는
기본 로컬 실행에서는 인증 없이 동작한다.

HTTP 상태 매핑(POST /spawn):
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

# 요청 본문 상한. /spawn 요청은 작으므로 넉넉히 64KiB 로 제한한다(자원 보호).
MAX_BODY_BYTES = 64 * 1024

# 화면(정적 뷰)을 서빙할 GET 경로.
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
    """Authorization 헤더에서 Bearer 토큰을 추출한다(없거나 형식 불량이면 None)."""
    raw = headers.get("Authorization") or headers.get("authorization")
    if not raw:
        return None
    parts = raw.split(None, 1)
    if len(parts) != 2 or parts[0].lower() != "bearer":
        return None
    token = parts[1].strip()
    return token or None


def handle_spawn(headers, body_bytes: bytes, now, registry, auth):
    """(status_code, response_dict) 를 돌려주는 전송-독립 처리기."""
    if body_bytes is not None and len(body_bytes) > MAX_BODY_BYTES:
        return 413, error(
            "payload_too_large", None, "요청 본문이 너무 큽니다."
        )

    token = parse_bearer(headers)

    try:
        payload = json.loads(body_bytes.decode("utf-8")) if body_bytes else {}
    except (ValueError, UnicodeDecodeError):
        return 400, error("invalid_request", None, "요청 본문이 올바른 JSON이 아닙니다.")
    if not isinstance(payload, dict):
        return 400, error("invalid_request", None, "요청 본문은 객체여야 합니다.")

    request = dict(payload)
    request["token"] = token

    result = spawn(request, now, registry, auth)
    if result.get("ok"):
        return 200, result

    code = result["error"]["code"]
    return _STATUS_BY_CODE.get(code, 400), result


# 실행기(process runner) 요청 본문 상한 — 명령/env가 포함되므로 조금 더 넉넉히 둔다.
MAX_PROCESS_BODY_BYTES = 64 * 1024

_CMD_MAX_LEN = 4096
_LABEL_MAX_LEN = 128


def _check_auth(headers, auth: AuthValidator) -> bool:
    return auth.check(parse_bearer(headers))


def handle_processes_list(headers, procman, auth):
    """(status_code, response_dict) — GET /processes."""
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "유효한 인증 토큰이 필요합니다.")
    processes = [p.to_public() for p in procman.list()]
    return 200, {"ok": True, "processes": processes}


def handle_process_start(
    headers, body_bytes: bytes, procman, auth, ident: str | None = None
):
    """(status_code, response_dict) — POST /processes 또는 PUT /processes/<id>."""
    if body_bytes is not None and len(body_bytes) > MAX_PROCESS_BODY_BYTES:
        return 413, error("payload_too_large", None, "요청 본문이 너무 큽니다.")
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "유효한 인증 토큰이 필요합니다.")

    try:
        payload = json.loads(body_bytes.decode("utf-8")) if body_bytes else {}
    except (ValueError, UnicodeDecodeError):
        return 400, error("invalid_request", None, "요청 본문이 올바른 JSON이 아닙니다.")
    if not isinstance(payload, dict):
        return 400, error("invalid_request", None, "요청 본문은 객체여야 합니다.")

    cmd = payload.get("cmd")
    if not isinstance(cmd, str) or not cmd.strip():
        return 400, error("invalid_request", "cmd", "cmd는 비어 있을 수 없습니다.")
    if len(cmd) > _CMD_MAX_LEN:
        return 400, error("invalid_request", "cmd", "cmd가 너무 깁니다.")

    label = payload.get("label")
    if label is not None and (not isinstance(label, str) or not label.strip()):
        return 400, error("invalid_request", "label", "label은 문자열이어야 합니다.")
    if isinstance(label, str) and len(label) > _LABEL_MAX_LEN:
        return 400, error("invalid_request", "label", "label이 너무 깁니다.")
    if not label:
        label = cmd.split(None, 1)[0]

    cwd = payload.get("cwd")
    if cwd is not None and (not isinstance(cwd, str) or not cwd.strip()):
        return 400, error("invalid_request", "cwd", "cwd는 문자열이어야 합니다.")

    env = payload.get("env")
    if env is not None:
        if not isinstance(env, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in env.items()
        ):
            return 400, error(
                "invalid_request", "env", "env는 문자열 키/값의 객체여야 합니다."
            )

    if ident is None:
        info = procman.start(label, cmd, cwd=cwd, env=env)
    else:
        info = procman.update(ident, label, cmd, cwd=cwd, env=env)
        if info is None:
            return 404, error("not_found", None, "해당 프로세스를 찾을 수 없습니다.")
    return 200, {"ok": True, "process": info.to_public()}


def handle_process_action(ident: str, action: str, headers, procman, auth):
    """(status_code, response_dict) — POST /processes/<id>/stop|restart."""
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "유효한 인증 토큰이 필요합니다.")
    if action == "stop":
        info = procman.stop(ident)
    elif action == "restart":
        info = procman.restart(ident)
    elif action == "run":
        info = procman.run(ident)
    else:
        return 404, error("not_found", None, "알 수 없는 엔드포인트입니다.")
    if info is None:
        return 404, error("not_found", None, "해당 프로세스를 찾을 수 없습니다.")
    return 200, {"ok": True, "process": info.to_public()}


def handle_process_logs(ident: str, headers, offset: int, procman, auth):
    """(status_code, response_dict) — GET /processes/<id>/logs?offset=<n>."""
    if not _check_auth(headers, auth):
        return 401, error("unauthorized", None, "유효한 인증 토큰이 필요합니다.")
    result = procman.read_log(ident, offset)
    if result is None:
        return 404, error("not_found", None, "해당 프로세스를 찾을 수 없습니다.")
    text, next_offset = result
    return 200, {"ok": True, "text": text, "next_offset": next_offset}


class _SpawnHandler(BaseHTTPRequestHandler):
    server_version = "SpawnPoint/0.1"

    # 서버 인스턴스가 주입한다.
    registry: Registry
    auth: AuthValidator
    procman: ProcessManager | None = None  # 실행기(process runner), 미주입 시 /processes 404
    index_html: bytes | None = None  # 화면 HTML(미주입 시 화면 라우트도 404)

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

    def do_POST(self) -> None:  # noqa: N802 (http.server 규약)
        path = self._path()
        parts = [p for p in path.split("/") if p]

        if path == "/spawn":
            body = self._read_body(MAX_BODY_BYTES)
            if body is None:
                self._send_json(
                    413, error("payload_too_large", None, "요청 본문이 너무 큽니다.")
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
                        413, error("payload_too_large", None, "요청 본문이 너무 큽니다.")
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

        self._send_json(404, error("not_found", None, "알 수 없는 엔드포인트입니다."))

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

        self._send_json(404, error("not_found", None, "알 수 없는 엔드포인트입니다."))

    def do_PUT(self) -> None:  # noqa: N802
        path = self._path()
        parts = [p for p in path.split("/") if p]
        if self.procman is not None and len(parts) == 2 and parts[0] == "processes":
            body = self._read_body(MAX_PROCESS_BODY_BYTES)
            if body is None:
                self._send_json(
                    413, error("payload_too_large", None, "요청 본문이 너무 큽니다.")
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
                self._send_json(401, error("unauthorized", None, "유효한 인증 토큰이 필요합니다."))
                return
            if not self.procman.delete(parts[1]):
                self._send_json(404, error("not_found", None, "해당 프로세스를 찾을 수 없습니다."))
                return
            self._send_json(200, {"ok": True, "deleted_id": parts[1]})
            return
        self._method_not_allowed()

    def do_HEAD(self) -> None:  # noqa: N802
        self.do_GET()

    def _method_not_allowed(self) -> None:
        self._send_json(
            405, error("method_not_allowed", None, "허용되지 않은 메서드입니다.")
        )

    # 미지원 메서드는 501 대신 명시적 405(JSON)로 응답한다.
    do_PATCH = _method_not_allowed

    def log_message(self, fmt, *args):  # 기본 stderr 접근 로그 억제
        return


def make_server(
    host: str,
    port: int,
    registry: Registry,
    auth: AuthValidator,
    index_html: bytes | None = None,
    procman: ProcessManager | None = None,
):
    """POST /spawn + (index_html 주입 시) 화면 + (procman 주입 시) /processes* 를 함께 처리한다.

    아직 serve하지 않는다. index_html 을 넘기면 GET / , /index.html 에서 그 HTML을
    같은 포트로 서빙한다 — 화면을 위해 별도 프로세스를 띄울 필요가 없다. procman을
    넘기면 실행기 라우트(/processes*)가 활성화된다(미주입 시 404).
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
