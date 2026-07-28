package dialect

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// TestClassifyTables is the decision table of 0008-L 2.10, written out. The
// PostgreSQL and MySQL rows are here even though neither driver is linked:
// those tables were fixed by the design, and a table nobody exercises until the
// day the engine is switched is a table that is wrong on that day.
func TestClassifyTables(t *testing.T) {
	cases := []struct {
		kind Kind
		code int
		want Class
		note string
	}{
		{SQLite, 1555, DuplicateKey, "primary key violation"},
		{SQLite, 2067, DuplicateKey, "unique constraint violation"},
		{SQLite, 275, Constraint, "check constraint"},
		{SQLite, 787, Constraint, "foreign key"},
		{SQLite, 1299, Constraint, "not null"},
		{SQLite, 1811, Constraint, "trigger"},
		{SQLite, 19, Constraint, "primary code only, no sub-code"},
		{SQLite, 1, ClassError, "generic error"},
		{SQLite, 11, ClassError, "corrupt database is not a constraint"},
		{SQLite, 0, ClassError, "no code"},

		{PostgreSQL, 23505, DuplicateKey, "unique_violation"},
		{PostgreSQL, 23503, Constraint, "foreign_key_violation"},
		{PostgreSQL, 23502, Constraint, "not_null_violation"},
		{PostgreSQL, 23514, Constraint, "check_violation"},
		{PostgreSQL, 42601, ClassError, "syntax_error"},
		{PostgreSQL, 22001, ClassError, "string_data_right_truncation"},

		{MySQL, 1062, DuplicateKey, "ER_DUP_ENTRY"},
		{MySQL, 1586, DuplicateKey, "ER_DUP_ENTRY_WITH_KEY_NAME"},
		{MySQL, 1048, Constraint, "ER_BAD_NULL_ERROR"},
		{MySQL, 1364, Constraint, "ER_NO_DEFAULT_FOR_FIELD"},
		{MySQL, 1451, Constraint, "ER_ROW_IS_REFERENCED_2"},
		{MySQL, 1452, Constraint, "ER_NO_REFERENCED_ROW_2"},
		{MySQL, 3819, Constraint, "ER_CHECK_CONSTRAINT_VIOLATED"},
		{MySQL, 1146, ClassError, "ER_NO_SUCH_TABLE"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.kind, tc.code), func(t *testing.T) {
			adapter := adapters[tc.kind]
			if got := adapter.Classify(tc.code); got != tc.want {
				t.Errorf("%s code %d (%s) = %q, want %q", tc.kind, tc.code, tc.note, got, tc.want)
			}
		})
	}
}

// TestUnknownCodesAreErrors states the default explicitly. 0008-L 2.10 forbids
// reading an unrecognised code optimistically as a constraint violation: doing
// so would turn a database that has genuinely broken into a stream of
// request-level rejections, and nothing would report the real problem.
func TestUnknownCodesAreErrors(t *testing.T) {
	for kind, adapter := range adapters {
		for _, code := range []int{-1, 0, 7, 9999, 123456} {
			if got := adapter.Classify(code); got != ClassError {
				t.Errorf("%s: unknown code %d classified as %q, want %q", kind, code, got, ClassError)
			}
		}
	}
}

// TestInterpretWithoutACode covers the case where the failure did not come from
// the engine at all. The verdict has to be ClassError with a note, never a
// fallback to matching the message text — that fallback is the defect 0006-D
// 3.5 identified.
func TestInterpretWithoutACode(t *testing.T) {
	adapter := mustAdapter(t)

	class, note := adapter.Interpret(errors.New("UNIQUE constraint failed: spawn_instance.id"))
	if class != ClassError {
		t.Errorf("class = %q, want %q — the message text must not decide", class, ClassError)
	}
	if note != NoteCodeUnavailable {
		t.Errorf("note = %q, want %q", note, NoteCodeUnavailable)
	}
}

// TestInterpretPrimaryCodeOnly is the degraded path of 0008-L 2.10: a bare 19
// says a constraint was violated but not which, so the duplicate-request path
// cannot run. That has to be visible in the log, which is what the note is for.
func TestInterpretPrimaryCodeOnly(t *testing.T) {
	adapter := mustAdapter(t)
	stub := *adapter
	stub.code = func(error) (int, bool) { return 19, true }

	class, note := stub.Interpret(errors.New("constraint failed"))
	if class != Constraint {
		t.Errorf("class = %q, want %q", class, Constraint)
	}
	if note != NoteExtendedCodeUnavailable {
		t.Errorf("note = %q, want %q", note, NoteExtendedCodeUnavailable)
	}
}

// TestSQLiteCodesAreExtended is the assumption everything above rests on,
// checked against the real engine instead of taken on trust.
//
// If the driver reported primary codes, every constraint violation would arrive
// as 19 and a duplicate identifier would be indistinguishable from a check
// violation. The duplicate-request contract in 0007-P would then be quietly
// unimplementable, and nothing else in the system would notice.
func TestSQLiteCodesAreExtended(t *testing.T) {
	adapter := mustAdapter(t)
	db, err := sql.Open(adapter.Driver(), adapter.DSN(filepath.Join(t.TempDir(), "codes.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	setup := []string{
		`CREATE TABLE pk (id TEXT NOT NULL, v INTEGER, CONSTRAINT pk_pk PRIMARY KEY (id), CONSTRAINT ck_v CHECK (v > 0))`,
		`CREATE TABLE uq (id TEXT, CONSTRAINT uq_id UNIQUE (id))`,
		`INSERT INTO pk (id, v) VALUES ('a', 1)`,
		`INSERT INTO uq (id) VALUES ('a')`,
	}
	for _, s := range setup {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}

	cases := []struct {
		name  string
		query string
		want  Class
	}{
		{"primary key collision", `INSERT INTO pk (id, v) VALUES ('a', 2)`, DuplicateKey},
		{"unique collision", `INSERT INTO uq (id) VALUES ('a')`, DuplicateKey},
		{"check violation", `INSERT INTO pk (id, v) VALUES ('b', -1)`, Constraint},
		{"not null violation", `INSERT INTO pk (id, v) VALUES (NULL, 1)`, Constraint},
		{"missing table", `INSERT INTO nope (id) VALUES ('a')`, ClassError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(tc.query)
			if err == nil {
				t.Fatalf("%s: expected a failure", tc.name)
			}
			class, note := adapter.Interpret(err)
			if class != tc.want {
				t.Errorf("%s: class = %q, want %q (err: %v)", tc.name, class, tc.want, err)
			}
			if note != "" {
				t.Errorf("%s: unexpected note %q — the extended code should have been readable", tc.name, note)
			}
		})
	}
}

// TestWriteErrorCarriesTheCause keeps the original message reachable. The class
// decides what the caller does; the message is what a person reads afterwards.
func TestWriteErrorCarriesTheCause(t *testing.T) {
	cause := errors.New("engine said no")
	err := error(&WriteError{Class: DuplicateKey, Err: cause})

	we, ok := AsWriteError(fmt.Errorf("insert instance: %w", err))
	if !ok {
		t.Fatal("AsWriteError did not find the verdict through a wrapper")
	}
	if we.Class != DuplicateKey {
		t.Errorf("class = %q, want %q", we.Class, DuplicateKey)
	}
	if !errors.Is(err, cause) {
		t.Error("the original cause is no longer reachable")
	}
}
