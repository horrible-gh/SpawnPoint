-- Existing registrations stay manual until explicitly enabled.
ALTER TABLE runner_entry
ADD COLUMN auto_start INTEGER NOT NULL DEFAULT 0 CHECK (auto_start IN (0, 1));
