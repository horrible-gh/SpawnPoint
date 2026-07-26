"""Parameters and limits."""

REQUESTER_MAX_LEN = 64
KIND_MAX_LEN = 32
LABEL_MAX_LEN = 256
TTL_MIN = 60
TTL_MAX = 86400  # 24 hours
TTL_DEFAULT = 3600  # 1 hour
DEDUP_WINDOW_SECONDS = 300
ALLOWED_KINDS = ["session", "worker", "task"]
