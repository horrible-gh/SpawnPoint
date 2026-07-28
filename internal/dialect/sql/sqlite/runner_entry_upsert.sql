INSERT INTO runner_entry (id, label, cmd, cwd, env, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    label      = excluded.label,
    cmd        = excluded.cmd,
    cwd        = excluded.cwd,
    env        = excluded.env,
    updated_at = excluded.updated_at;
