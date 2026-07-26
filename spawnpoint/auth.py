"""인증 검증 — 프로토콜 P-0005 시나리오 4 / L-0006 §2.1 validate_auth.

허용 토큰이 설정된 경우에만 Bearer 토큰을 검증한다. 토큰 설정이 없는
기본 로컬 실행은 인증을 끄므로 브라우저 화면에서 별도 값을 입력할 필요가 없다.
"""
from __future__ import annotations

import hmac
from typing import Iterable


class AuthValidator:
    """허용 토큰 집합과 대조한다.

    허용 토큰이 설정되지 않으면 인증 자체가 비활성화된다. 외부에 서버를
    공개할 때는 tokens를 지정해 인증을 활성화해야 한다.
    """

    def __init__(self, tokens: Iterable[str] | None = None):
        cleaned = {t for t in (tokens or []) if t}
        self._tokens = cleaned or None

    @property
    def enabled(self) -> bool:
        return self._tokens is not None

    def check(self, token: str | None) -> bool:
        if self._tokens is None:
            return True
        if not token:
            return False
        # 타이밍 공격 완화를 위해 상수시간 비교.
        return any(hmac.compare_digest(token, t) for t in self._tokens)
