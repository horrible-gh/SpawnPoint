INSERT INTO spawn_daily_seq (date_part, last_seq, updated_at)
VALUES (?, 1, ?)
ON CONFLICT(date_part) DO UPDATE SET
    last_seq = last_seq + 1,
    updated_at = excluded.updated_at;
