"""Domain model (SpawnInstance / spawn_instance table)."""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

from .clock import KST


@dataclass(frozen=True)
class SpawnInstance:
    """One instance created. Corresponds to one row in spawn_instance table."""

    id: str
    requester: str
    kind: str
    status: str
    request_key: str | None
    label: str | None
    ttl_seconds: int
    created_at: datetime
    expires_at: datetime

    def to_public(self) -> dict:
        """Handle to return to caller (instance block in the spawn response)."""
        return {
            "id": self.id,
            "status": self.status,
            "kind": self.kind,
            "requester": self.requester,
            "created_at": self.created_at.astimezone(KST).isoformat(),
        }
