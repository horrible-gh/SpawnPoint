"""Bearer token authentication."""

from __future__ import annotations


class AuthValidator:
    """Validate Bearer tokens against an allowed list."""

    def __init__(self, tokens: list[str] | None = None):
        """Initialize with a list of allowed tokens.

        Empty list / omitted = auth disabled, which is the default local mode.
        """
        self.enabled = bool(tokens)
        self._tokens = set(tokens or [])

    def check(self, token: str | None) -> bool:
        """Return True if token is valid, or if auth is disabled."""
        if not self.enabled:
            return True
        if token is None:
            return False
        return token in self._tokens