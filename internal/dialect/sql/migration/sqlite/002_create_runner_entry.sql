CREATE TABLE IF NOT EXISTS runner_entry (
    id         TEXT      NOT NULL,
    label      TEXT      NOT NULL,
    cmd        TEXT      NOT NULL,
    cwd        TEXT,
    env        TEXT      NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    CONSTRAINT pk_runner_entry     PRIMARY KEY (id),
    CONSTRAINT ck_runner_cmd_len   CHECK (length(cmd) BETWEEN 1 AND 4096),
    CONSTRAINT ck_runner_label_len CHECK (length(label) <= 128)
);

CREATE INDEX IF NOT EXISTS ix_runner_created
    ON runner_entry (created_at);
