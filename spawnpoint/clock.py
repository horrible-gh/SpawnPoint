"""시간·타임존 유틸.

- 인스턴스의 표시용 시각(created_at/expires_at)은 KST(+09:00)로 렌더링한다
  (프로토콜 P-0005 예시와 일치).
- 저장·비교용 시각은 UTC 고정폭 ISO 8601 문자열로 직렬화하여,
  사전식(lexicographic) 비교가 곧 시간순 비교가 되도록 한다.
"""
from __future__ import annotations

from datetime import datetime, timezone, timedelta

KST = timezone(timedelta(hours=9))
UTC = timezone.utc

# 고정폭 포맷 — 마이크로초를 항상 6자리로 채워 문자열 정렬이 시간순과 일치하게 한다.
_UTC_SUFFIX = "+00:00"


def now_kst() -> datetime:
    """현재 시각을 KST 기준 타임존 인식 datetime으로 반환한다."""
    return datetime.now(KST)


def to_utc_iso(dt: datetime) -> str:
    """타임존 인식 datetime을 고정폭 UTC ISO 8601 문자열로 직렬화한다."""
    if dt.tzinfo is None:
        raise ValueError("naive datetime은 직렬화할 수 없습니다.")
    u = dt.astimezone(UTC)
    return u.strftime("%Y-%m-%dT%H:%M:%S.%f") + _UTC_SUFFIX


def from_utc_iso(s: str) -> datetime:
    """to_utc_iso가 만든 문자열을 타임존 인식(UTC) datetime으로 역직렬화한다."""
    base = s[: -len(_UTC_SUFFIX)] if s.endswith(_UTC_SUFFIX) else s
    return datetime.strptime(base, "%Y-%m-%dT%H:%M:%S.%f").replace(tzinfo=UTC)
