SELECT id, label, cmd, cwd, env, created_at, updated_at, auto_start, run_as_json, log_name
FROM runner_entry
ORDER BY created_at, id;
