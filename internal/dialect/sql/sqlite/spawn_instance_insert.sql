INSERT INTO spawn_instance (
    id, requester, kind, status, request_key,
    label, ttl_seconds, created_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
