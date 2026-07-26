"""Top-level handler — spawn().

Assembles the decision flow (Intake→Validator→Allocator→Registrar→Responder).
Implemented as a pure function independent of the transport layer, so tests can call it directly.

request dictionary format (transport-layer-injected token included):
    {
      "token": "<bearer token or None>",
      "requester": "...",
      "kind": "session|worker|task",
      "request_key": "...",          # optional
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
    # Validation already passed, so if it's an int, it's within range. If unset, use default.
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
    # 1. Authentication
    if not auth.check(request.get("token")):
        return error("unauthorized", None, "Valid authentication token required.")

    # 2. Validation
    v = validate_request(request)
    if not v.ok:
        return error("invalid_request", v.field, v.message)

    dedup_window = params.DEDUP_WINDOW_SECONDS
    request_key = request.get("request_key")

    # 3. Duplicate request early detection (idempotent handling)
    if request_key is not None:
        existing = registry.find_active_by_key(request_key, now, dedup_window)
        if existing is not None:
            return ok_instance(existing, deduplicated=True)

    # 4. Allocate ID → construct instance → register
    ident = allocate_id(now, registry)
    ttl = _resolve_ttl(request.get("options") or {})
    instance = build_instance(ident, request, ttl, now)

    written = registry.insert(instance)
    if not written.ok:
        # ID collision → retry key lookup → return existing if found (retry path)
        if written.reason == "duplicate_key" and request_key is not None:
            existing = registry.find_active_by_key(request_key, now, dedup_window)
            if existing is not None:
                return ok_instance(existing, deduplicated=True)
        return error("storage_error", None, "Failed to save instance.")

    return ok_instance(instance, deduplicated=False)
