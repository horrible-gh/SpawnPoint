"""도메인 모델 (D-0004 §5 출력 핸들, DB-0007 §2.1 spawn_instance)."""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

from .clock import KST


@dataclass(frozen=True)
class SpawnInstance:
    """생성된 인스턴스 1건. spawn_instance 테이블 1행에 대응한다."""

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
        """호출자에게 돌려줄 핸들 (프로토콜 P-0005 응답의 instance 블록)."""
        return {
            "id": self.id,
            "status": self.status,
            "kind": self.kind,
            "requester": self.requester,
            "created_at": self.created_at.astimezone(KST).isoformat(),
        }
