"""식별자 발급부(Allocator) — L-0006 §2.3.

형식: spwn_{YYYYMMDD}_{seq}{rand}
- YYYYMMDD : now의 로컬(KST) 날짜
- seq      : 해당 날짜 내 생성 순번 (원자적 증가, 0 패딩)
- rand     : 16진수 무작위값 (충돌 회피 보조)
"""
from __future__ import annotations

import secrets
from datetime import datetime

from . import params
from .clock import KST


def _random_hex(n_chars: int) -> str:
    # token_hex(k)는 2k자리 hex를 만든다. 홀수 자리 요청 시 잘라 맞춘다.
    return secrets.token_hex((n_chars + 1) // 2)[:n_chars]


def allocate_id(now: datetime, registry) -> str:
    date_part = now.astimezone(KST).strftime("%Y%m%d")
    seq = registry.next_daily_seq(date_part, now)  # 원자적 증가
    seq_part = str(seq).zfill(params.ID_DAILY_SEQ_PAD)
    rand_part = _random_hex(params.ID_RANDOM_LEN)
    return f"{params.ID_PREFIX}_{date_part}_{seq_part}{rand_part}"
