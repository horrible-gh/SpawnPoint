"""SpawnPoint 애플리케이션 진입점.

환경변수로 설정을 읽어 저장소·인증을 구성하고 서버를 기동한다. 하나의
프로세스·하나의 포트에서 API(POST /spawn)와 화면(GET /)을 함께 서빙한다.

환경변수:
    SPAWNPOINT_HOST        기본 127.0.0.1
    SPAWNPOINT_PORT        기본 8091
    SPAWNPOINT_DB_PATH     기본 spawnpoint.db (SQLite 파일 경로)
    SPAWNPOINT_LOG_DIR     기본 logs (실행기가 관리하는 프로세스의 로그 디렉토리)
    SPAWNPOINT_API_TOKENS  쉼표로 구분한 허용 Bearer 토큰 목록.
                           미설정 시 인증 비활성화(기본 로컬 실행).
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
    # Windows 기본 콘솔(cp949/cp932)은 한글 기동 메시지를 인코딩하지 못해
    # 시작하자마자 UnicodeEncodeError로 죽는다. 가능하면 UTF-8로 전환한다.
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

    registry, auth, procman, host, port = build()
    server = make_server(
        host, port, registry, auth, index_html=INDEX_HTML, procman=procman
    )
    mode = "토큰 검증" if auth.enabled else "없음(로컬 기본값)"
    print(f"SpawnPoint listening on http://{host}:{port}  [auth: {mode}]")
    print(f"  화면(UI):  http://{host}:{port}/")
    print(f"  API:       POST http://{host}:{port}/spawn")
    print(f"  실행기:    http://{host}:{port}/processes")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        registry.close()


if __name__ == "__main__":
    main()
