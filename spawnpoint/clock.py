"""Time / timezone utility. Display in KST, store/compare in fixed-width UTC ISO."""

from datetime import datetime, timezone

KST = timezone(datetime.now().astimezone().utcoffset())


def now_kst() -> datetime:
    """Return current time in UTC (can be converted to KST with astimezone)."""
    return datetime.now(timezone.utc)


def to_utc_iso(dt: datetime) -> str:
    """Render a datetime for a database column: UTC, fixed width, 3 fraction digits.

    The fraction is truncated, not rounded — the duplicate-request check compares
    these values as strings, so rounding would move a row across the window
    boundary relative to its neighbours.
    """
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def from_utc_iso(s: str) -> datetime:
    """Parse a value written by to_utc_iso back into an aware datetime."""
    return datetime.fromisoformat(s.replace("Z", "+00:00"))