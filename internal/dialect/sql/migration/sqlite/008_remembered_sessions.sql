-- Preserve existing sessions without silently granting persistent login.
-- The old implementation did not record the user's remember choice.
ALTER TABLE access_session ADD COLUMN remembered INTEGER NOT NULL DEFAULT 0 CHECK (remembered IN (0,1));
ALTER TABLE access_session ADD COLUMN token_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE access_session ADD COLUMN previous_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE access_session ADD COLUMN previous_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE access_session ADD COLUMN rotated_at INTEGER NOT NULL DEFAULT 0;
UPDATE access_session SET token_digest=digest, rotated_at=created_at;
CREATE UNIQUE INDEX access_session_token ON access_session(token_digest) WHERE token_digest<>'';
CREATE INDEX access_session_previous ON access_session(previous_digest) WHERE previous_digest<>'';
