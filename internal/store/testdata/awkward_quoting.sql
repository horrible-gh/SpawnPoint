-- Fixture for 0008-L 6.3 item 5: every semicolon below sits inside a string
-- literal, a comment or a quoted identifier, and none of them is a statement
-- separator. A splitter that gets this wrong produces fragments that either
-- fail to execute or execute as something else entirely; this script is applied
-- for real so that either outcome shows up as a failure.

CREATE TABLE quoting_probe (
    id   INTEGER NOT NULL,          -- a comment with a ; in it
    note TEXT    NOT NULL DEFAULT 'default; value',
    CONSTRAINT pk_quoting_probe PRIMARY KEY (id),
    CONSTRAINT ck_note_not_empty CHECK (note <> '')
);

/* A block comment spanning lines;
   with a semicolon inside it;
   and a stray quote ' that must not open a literal. */

CREATE TABLE "semi;colon" (
    [bracketed;name] TEXT,
    `backticked;name` TEXT
);

INSERT INTO quoting_probe (id, note)
VALUES (1, 'a;b -- not a comment /* nor this */');

-- An escaped quote immediately before a semicolon: the '' is data, so the
-- literal is still open when the ; that follows it appears.
INSERT INTO quoting_probe (id, note) VALUES (2, 'it''s; still one statement');

CREATE TABLE note_probe (a INTEGER);
