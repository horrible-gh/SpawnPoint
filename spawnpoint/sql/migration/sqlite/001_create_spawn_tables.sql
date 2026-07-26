CREATE TABLE IF NOT EXISTS spawn_instance (
    id           TEXT      NOT NULL,
    requester    TEXT      NOT NULL,
    kind         TEXT      NOT NULL,
    status       TEXT      NOT NULL DEFAULT 'created',
    request_key  TEXT,
    label        TEXT,
    ttl_seconds  INTEGER   NOT NULL DEFAULT 3600,
    created_at   TIMESTAMP NOT NULL,
    expires_at   TIMESTAMP NOT NULL,
    CONSTRAINT pk_spawn_instance PRIMARY KEY (id),
    CONSTRAINT ck_kind          CHECK (kind IN ('session', 'worker', 'task')),
    CONSTRAINT ck_status        CHECK (status IN ('created', 'active', 'expired')),
    CONSTRAINT ck_ttl_range     CHECK (ttl_seconds BETWEEN 60 AND 86400),
    CONSTRAINT ck_requester_len CHECK (length(requester) <= 64),
    CONSTRAINT ck_kind_len      CHECK (length(kind) <= 32),
    CONSTRAINT ck_label_len     CHECK (label IS NULL OR length(label) <= 128),
    CONSTRAINT ck_expires_after CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS ix_reqkey_created
    ON spawn_instance (request_key, created_at DESC)
    WHERE request_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS ix_requester_created
    ON spawn_instance (requester, created_at DESC);

CREATE INDEX IF NOT EXISTS ix_expires
    ON spawn_instance (expires_at);

CREATE TABLE IF NOT EXISTS spawn_daily_seq (
    date_part  TEXT      NOT NULL,
    last_seq   INTEGER   NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL,
    CONSTRAINT pk_spawn_daily_seq  PRIMARY KEY (date_part),
    CONSTRAINT ck_last_seq_nonneg  CHECK (last_seq >= 0)
);
