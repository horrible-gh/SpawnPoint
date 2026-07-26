"""Mockup (dx0qx6f7) static page loader.

Read `static/index.html` and cache it as bytes. Uses only the standard library.
"""
from __future__ import annotations

from pathlib import Path

_STATIC_DIR = Path(__file__).parent / "static"
INDEX_HTML: bytes = (_STATIC_DIR / "index.html").read_bytes()