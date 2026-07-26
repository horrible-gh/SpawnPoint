"""ID allocator."""

from __future__ import annotations

import secrets
from datetime import datetime

from . import params
from .clock import KST
from .storage import Registry


def allocate_id(now: datetime, registry: Registry) -> str:
    """Allocate a unique ID: spwn_{YYYYMMDD}_{seq}{rand}."""
    date_str = now.astimezone(KST).strftime("%Y%m%d")
    seq = registry.next_seq(date_str)
    rand = secrets.token_hex(3)
    return f"spwn_{date_str}_{seq:04d}{rand}"
