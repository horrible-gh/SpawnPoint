SELECT id, label, cmd, cwd, env, created_at, updated_at
FROM runner_entry
ORDER BY created_at, id;
