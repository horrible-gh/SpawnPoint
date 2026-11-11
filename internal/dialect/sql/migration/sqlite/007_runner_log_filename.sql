-- Stable, human-readable child-log file stem; old entries migrate on restore.
ALTER TABLE runner_entry ADD COLUMN log_name TEXT NOT NULL DEFAULT '';
