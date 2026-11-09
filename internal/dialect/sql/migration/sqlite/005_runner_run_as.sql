-- Empty means service identity; legacy commands retain their original account.
ALTER TABLE runner_entry ADD COLUMN run_as_json TEXT NOT NULL DEFAULT '';
