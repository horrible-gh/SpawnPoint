-- Application policy is versioned atomically. Password hashes and token digests
-- are private persistence data; public snapshots are built from explicit DTOs.
CREATE TABLE IF NOT EXISTS access_state (
 id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, document TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS access_session (
 digest TEXT PRIMARY KEY, user_id TEXT NOT NULL, user_version INTEGER NOT NULL,
 csrf TEXT NOT NULL, created_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
 expires_at INTEGER NOT NULL, auth_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS access_session_user ON access_session(user_id);
CREATE TABLE IF NOT EXISTS access_audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, actor TEXT NOT NULL,
 action TEXT NOT NULL, target TEXT NOT NULL, result TEXT NOT NULL, detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS access_attempt (
 key TEXT PRIMARY KEY, count INTEGER NOT NULL, started INTEGER NOT NULL
);
