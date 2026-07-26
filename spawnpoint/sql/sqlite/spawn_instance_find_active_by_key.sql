SELECT id, requester, kind, status, request_key, label, ttl_seconds, created_at, expires_at
FROM spawn_instance
WHERE request_key = ? AND created_at > ?
ORDER BY created_at DESC
LIMIT 1;
