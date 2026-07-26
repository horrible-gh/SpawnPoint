"""최상위 처리 — L-0006 §2.1 spawn().

D-0004의 판단 흐름(Intake→Validator→Allocator→Registrar→Responder)을 조립한다.
전송 계층에 독립적인 순수 함수로 구현하여 테스트가 직접 호출할 수 있게 한다.

request 딕셔너리 형태(P-0005 + 전송 계층이 주입하는 token):
    {
      "token": "<bearer 토큰 또는 None>",
      "requester": "...",
      "kind": "session|worker|task",
      "request_key": "...",          # 선택
      "options": {"label": "...", "ttl_seconds": 3600}
    }
"""
from __future__ import annotations

from datetime import datetime, timedelta

from . import params
from .allocator import allocate_id
from .auth import AuthValidator
from .clock import KST
from .models import SpawnInstance
from .results import error, ok_instance
from .storage import Registry
from .validator import validate_request


def _resolve_ttl(options: dict) -> int:
    ttl = options.get("ttl_seconds")
    # 검증을 이미 통과했으므로 정수면 범위 내 값이다. 미지정이면 기본값.
    if isinstance(ttl, int) and not isinstance(ttl, bool):
        return ttl
    return params.TTL_DEFAULT


def build_instance(
    ident: str, request: dict, ttl: int, now: datetime
) -> SpawnInstance:
    options = request.get("options") or {}
    created = now.astimezone(KST)
    expires = created + timedelta(seconds=ttl)
    return SpawnInstance(
        id=ident,
        requester=request["requester"],
        kind=request["kind"],
        status="created",
        request_key=request.get("request_key"),
        label=options.get("label"),
        ttl_seconds=ttl,
        created_at=created,
        expires_at=expires,
    )


def spawn(
    request: dict,
    now: datetime,
    registry: Registry,
    auth: AuthValidator,
) -> dict:
    # 1. 인증
    if not auth.check(request.get("token")):
        return error("unauthorized", None, "유효한 인증 토큰이 필요합니다.")

    # 2. 유효성 검증
    v = validate_request(request)
    if not v.ok:
        return error("invalid_request", v.field, v.message)

    dedup_window = params.DEDUP_WINDOW_SECONDS
    request_key = request.get("request_key")

    # 3. 중복 요청 사전 판정 (멱등 처리)
    if request_key is not None:
        existing = registry.find_active_by_key(request_key, now, dedup_window)
        if existing is not None:
            return ok_instance(existing, deduplicated=True)

    # 4. 식별자 발급 → 인스턴스 구성 → 등록
    ident = allocate_id(now, registry)
    ttl = _resolve_ttl(request.get("options") or {})
    instance = build_instance(ident, request, ttl, now)

    written = registry.insert(instance)
    if not written.ok:
        # 식별자 충돌 → 동일 key 재조회 후 있으면 기존 반환 (L-0006 §2.1 재시도 경로)
        if written.reason == "duplicate_key" and request_key is not None:
            existing = registry.find_active_by_key(request_key, now, dedup_window)
            if existing is not None:
                return ok_instance(existing, deduplicated=True)
        return error("storage_error", None, "인스턴스 저장에 실패했습니다.")

    return ok_instance(instance, deduplicated=False)
