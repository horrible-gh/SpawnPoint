INSERT INTO runner_entry (id, label, cmd, cwd, env, created_at, updated_at, auto_start, run_as_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    label      = excluded.label,
    cmd        = excluded.cmd,
    cwd        = excluded.cwd,
    env        = excluded.env,
    updated_at = excluded.updated_at,
    auto_start = excluded.auto_start,
    run_as_json = excluded.run_as_json;
