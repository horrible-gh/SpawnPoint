CREATE TABLE spawn_instance_v2 (
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
    CONSTRAINT ck_label_len     CHECK (label IS NULL OR length(label) <= 256),
    CONSTRAINT ck_expires_after CHECK (expires_at > created_at)
);

INSERT INTO spawn_instance_v2 (
    id, requester, kind, status, request_key,
    label, ttl_seconds, created_at, expires_at
)
SELECT
    id, requester, kind, status, request_key,
    label, ttl_seconds, created_at, expires_at
FROM spawn_instance;

DROP TABLE spawn_instance;

ALTER TABLE spawn_instance_v2 RENAME TO spawn_instance;

CREATE INDEX ix_reqkey_created
    ON spawn_instance (request_key, created_at DESC)
    WHERE request_key IS NOT NULL;

CREATE INDEX ix_requester_created
    ON spawn_instance (requester, created_at DESC);

CREATE INDEX ix_expires
    ON spawn_instance (expires_at);