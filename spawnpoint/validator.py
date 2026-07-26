"""유효성 검증부(Validator) — L-0006 §2.2 / §5.

요청이 생성 조건을 충족하는지 판단하고, 실패 시 위반 필드와 메시지를 돌려준다.
"""
from __future__ import annotations

from dataclasses import dataclass

from . import params


@dataclass(frozen=True)
class Validation:
    ok: bool
    field: str | None = None
    message: str | None = None


def _fail(field: str, message: str) -> Validation:
    return Validation(False, field, message)


def _blank(value) -> bool:
    """문자열이 아니거나 공백뿐이면 비어 있는 것으로 본다."""
    return not isinstance(value, str) or value.strip() == ""


def _is_int(value) -> bool:
    # bool은 int의 하위형이므로 명시적으로 제외한다.
    return isinstance(value, int) and not isinstance(value, bool)


def validate_request(request: dict) -> Validation:
    requester = request.get("requester")
    if _blank(requester):
        return _fail("requester", "requester는 비어 있을 수 없습니다.")
    if len(requester) > params.REQUESTER_MAX_LEN:
        return _fail("requester", "requester가 너무 깁니다.")

    kind = request.get("kind")
    if _blank(kind):
        return _fail("kind", "kind는 비어 있을 수 없습니다.")
    if len(kind) > params.KIND_MAX_LEN:
        return _fail("kind", "kind가 너무 깁니다.")
    if kind not in params.ALLOWED_KINDS:
        return _fail("kind", "허용되지 않은 kind입니다.")

    options = request.get("options") or {}

    label = options.get("label")
    if label is not None:
        if not isinstance(label, str) or len(label) > params.LABEL_MAX_LEN:
            return _fail("options.label", "label이 너무 깁니다.")

    ttl = options.get("ttl_seconds")
    if ttl is not None:
        if not _is_int(ttl) or ttl < params.TTL_MIN or ttl > params.TTL_MAX:
            return _fail(
                "options.ttl_seconds", "ttl_seconds가 허용 범위를 벗어났습니다."
            )

    return Validation(True)
