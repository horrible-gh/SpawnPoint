"""Validation layer (Validator) — L-0006 §2.2 / §5.

Determines if a request meets creation conditions. On failure, returns the violated field and message.
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
    """A value is considered empty if it is not a string or contains only whitespace."""
    return not isinstance(value, str) or value.strip() == ""


def _is_int(value) -> bool:
    # bool is a subtype of int, so exclude it explicitly.
    return isinstance(value, int) and not isinstance(value, bool)


def validate_request(request: dict) -> Validation:
    requester = request.get("requester")
    if _blank(requester):
        return _fail("requester", "requester cannot be empty.")
    if len(requester) > params.REQUESTER_MAX_LEN:
        return _fail("requester", "requester is too long.")

    kind = request.get("kind")
    if _blank(kind):
        return _fail("kind", "kind cannot be empty.")
    if len(kind) > params.KIND_MAX_LEN:
        return _fail("kind", "kind is too long.")
    if kind not in params.ALLOWED_KINDS:
        return _fail("kind", "kind is not allowed.")

    options = request.get("options") or {}

    label = options.get("label")
    if label is not None:
        if not isinstance(label, str) or len(label) > params.LABEL_MAX_LEN:
            return _fail("options.label", "label is too long.")

    ttl = options.get("ttl_seconds")
    if ttl is not None:
        if not _is_int(ttl) or ttl < params.TTL_MIN or ttl > params.TTL_MAX:
            return _fail(
                "options.ttl_seconds", "ttl_seconds is outside the allowed range."
            )

    return Validation(True)