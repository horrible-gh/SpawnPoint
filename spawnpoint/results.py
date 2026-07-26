"""응답 구성부(Responder) — 프로토콜 P-0005 응답 형태를 만든다."""
from __future__ import annotations

from .models import SpawnInstance


def ok_instance(inst: SpawnInstance, deduplicated: bool = False) -> dict:
    """성공 응답. 중복 처리로 기존 인스턴스를 반환할 때만 deduplicated=true를 싣는다."""
    resp: dict = {"ok": True}
    if deduplicated:
        resp["deduplicated"] = True
    resp["instance"] = inst.to_public()
    return resp


def error(code: str, field: str | None, message: str) -> dict:
    """거부 응답. field는 invalid_request에서 위반 필드를 가리킬 때만 포함한다."""
    err: dict = {"code": code}
    if field is not None:
        err["field"] = field
    err["message"] = message
    return {"ok": False, "error": err}
