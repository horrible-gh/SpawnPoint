"""SpawnPoint — 새 작업 인스턴스 생성 진입점 모듈.

공개 API:
    spawn(request, now, registry, auth) -> dict   # 최상위 처리
    Registry / AuthValidator                       # 의존성
"""
from __future__ import annotations

from .auth import AuthValidator
from .models import SpawnInstance
from .service import spawn, build_instance
from .storage import Registry, WriteResult

__all__ = [
    "spawn",
    "build_instance",
    "SpawnInstance",
    "Registry",
    "WriteResult",
    "AuthValidator",
]

__version__ = "0.1.0"
