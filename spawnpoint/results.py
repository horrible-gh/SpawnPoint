"""Response builder (P-0005 response format)."""

from . import models


def ok_instance(instance: models.SpawnInstance, deduplicated: bool = False) -> dict:
    """Success response: instance created (or retrieved if deduplicated)."""
    body = {
        "ok": True,
        "instance": {
            "id": instance.id,
            "status": instance.status,
            "kind": instance.kind,
            "requester": instance.requester,
            "created_at": instance.created_at.isoformat(),
        },
    }
    if deduplicated:
        body["deduplicated"] = True
    if instance.label:
        body["instance"]["label"] = instance.label
    return body


def error(code: str, field: str | None, message: str) -> dict:
    """Error response."""
    body = {"ok": False, "error": {"code": code, "message": message}}
    if field is not None:
        body["error"]["field"] = field
    return body