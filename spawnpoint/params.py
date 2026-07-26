"""SpawnPoint 파라미터 정의 (L-0006 §1).

여기의 수치는 초기 개발 단계 기준값이며, DB-0007의 CHECK 제약과 값이 일치한다.
파라미터를 변경할 때는 L-0006 및 DB-0007 두 문서를 함께 갱신한다.
"""
from __future__ import annotations

# 동일 request_key 중복 판정 시간 창 (초)
DEDUP_WINDOW_SECONDS = 300

# 허용되는 생성 유형 (kind)
ALLOWED_KINDS = frozenset({"session", "worker", "task"})

# 길이 제한 (문자)
REQUESTER_MAX_LEN = 64
KIND_MAX_LEN = 32
LABEL_MAX_LEN = 128

# TTL 범위 (초)
TTL_MIN = 60
TTL_MAX = 86400
TTL_DEFAULT = 3600

# 식별자 구성 규칙 (형식: spwn_YYYYMMDD_{seq}{rand})
ID_PREFIX = "spwn"
ID_RANDOM_LEN = 6      # 무작위 접미부 16진수 자리수 (충돌 회피 보조)
ID_DAILY_SEQ_PAD = 4   # 일자별 순번 0 패딩 자리수
