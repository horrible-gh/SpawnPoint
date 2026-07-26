"""Time / timezone utility. Display in KST, store/compare in fixed-width UTC ISO."""

from datetime import datetime, timezone

KST = timezone(datetime.now().astimezone().utcoffset())


def now_kst() -> datetime:
    """Return current time in UTC (can be converted to KST with astimezone)."""
    return datetime.now(timezone.utc)