"""시안(dx0qx6f7) 정적 페이지 로더.

`static/index.html` 을 그대로 읽어 바이트로 캐시해 둔다. 표준 라이브러리만 쓴다.
"""
from __future__ import annotations

from pathlib import Path

_STATIC_DIR = Path(__file__).parent / "static"
INDEX_HTML: bytes = (_STATIC_DIR / "index.html").read_bytes()
