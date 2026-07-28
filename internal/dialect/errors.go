package dialect

import (
	"errors"
	"fmt"
)

// Class is how a write failure is understood by the code above the store.
type Class string

const (
	// DuplicateKey is a primary-key or unique-constraint collision. It is the
	// only class the issue path treats as recoverable: it either retries with a
	// fresh random tail or, when the request carried a key, returns the caller's
	// existing instance (0008-L 2.11, 2.12).
	DuplicateKey Class = "duplicate_key"
	// Constraint is any other constraint violation — check, foreign key, not
	// null, trigger. The request is rejected; retrying would fail identically.
	Constraint Class = "constraint"
	// ClassError is everything else, and the default. 0008-L 2.10 is explicit
	// that an unrecognised code must not be optimistically read as a constraint
	// violation, because doing so turns a broken database into a stream of
	// ordinary-looking request rejections.
	ClassError Class = "error"
)

// Note is a remark the interpreter wants recorded in the operations log
// alongside its verdict. It is empty when the code was read cleanly.
//
// The interpreter deliberately does not log by itself: 0008-L 2.10 puts it on
// the path of every write, and a package that both decides and reports is one
// that cannot be tested without a log.
type Note string

const (
	// NoteCodeUnavailable means no engine code could be extracted at all. The
	// verdict falls back to ClassError rather than to message matching — the
	// behaviour that is being removed.
	NoteCodeUnavailable Note = "error code unavailable"
	// NoteExtendedCodeUnavailable means only a primary code was available, so a
	// constraint violation could be seen but a duplicate key could not be
	// distinguished within it. The duplicate-request path is inactive whenever
	// this appears, which is why it is worth a WARN of its own (0008-L 2.10).
	NoteExtendedCodeUnavailable Note = "extended_code_unavailable"
)

// Interpret classifies a write failure. 0008-L 2.10.
func (a *Adapter) Interpret(err error) (Class, Note) {
	if err == nil {
		return ClassError, ""
	}
	if a.code == nil {
		return ClassError, NoteCodeUnavailable
	}
	code, ok := a.code(err)
	if !ok {
		return ClassError, NoteCodeUnavailable
	}
	class := a.classify(code)
	if note := a.degraded(code); note != "" {
		return class, note
	}
	return class, ""
}

// degraded reports the case where the engine gave a primary code where an
// extended one was expected. For SQLite that is a bare 19: it says a constraint
// was violated but not which kind, so 1555 and 2067 cannot be told apart from
// the rest and the duplicate path is dark.
func (a *Adapter) degraded(code int) Note {
	if a.kind == SQLite && code == sqliteConstraint {
		return NoteExtendedCodeUnavailable
	}
	return ""
}

// Classify exposes the code-to-class mapping directly. The interpreter uses it
// through Interpret; it is separate so the tables of engines whose drivers are
// not linked can still be exercised.
func (a *Adapter) Classify(code int) Class { return a.classify(code) }

// --- SQLite -----------------------------------------------------------------

// SQLite result codes. The extended codes are the primary code in the low eight
// bits plus a sub-code above them, so 1555 is 19 | (6 << 8).
const (
	sqliteConstraint           = 19   // SQLITE_CONSTRAINT, primary code
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

func classifySQLite(code int) Class {
	switch code {
	case sqliteConstraintPrimaryKey, sqliteConstraintUnique:
		return DuplicateKey
	}
	// Every extended code of a constraint violation shares the primary code in
	// its low byte: 275 (check), 787 (foreign key), 1299 (not null), 1811
	// (trigger) and the rest. Masking is what keeps the list from having to be
	// complete — a sub-code this build has never seen is still a constraint.
	if code&0xff == sqliteConstraint {
		return Constraint
	}
	return ClassError
}

// --- PostgreSQL --------------------------------------------------------------

// PostgreSQL reports SQLSTATE, a five-character code. Class 23 is integrity
// constraint violation and 23505 within it is unique_violation. The code is
// carried here as an integer so that one classify signature covers all three
// engines; 23505 reads the same either way.
const (
	pgUniqueViolation    = 23505
	pgIntegrityClassLow  = 23000
	pgIntegrityClassHigh = 23999
)

func classifyPostgreSQL(code int) Class {
	switch {
	case code == pgUniqueViolation:
		return DuplicateKey
	case code >= pgIntegrityClassLow && code <= pgIntegrityClassHigh:
		return Constraint
	default:
		return ClassError
	}
}

// --- MySQL -------------------------------------------------------------------

// MySQL error numbers, 0008-L 2.10.
const (
	myDupEntry              = 1062 // ER_DUP_ENTRY
	myDupEntryWithKeyName   = 1586 // ER_DUP_ENTRY_WITH_KEY_NAME
	myBadNull               = 1048 // ER_BAD_NULL_ERROR
	myNoDefaultForField     = 1364 // ER_NO_DEFAULT_FOR_FIELD
	myRowIsReferenced       = 1451 // ER_ROW_IS_REFERENCED_2
	myNoReferencedRow       = 1452 // ER_NO_REFERENCED_ROW_2
	myCheckConstraintFailed = 3819 // ER_CHECK_CONSTRAINT_VIOLATED
)

func classifyMySQL(code int) Class {
	switch code {
	case myDupEntry, myDupEntryWithKeyName:
		return DuplicateKey
	case myBadNull, myNoDefaultForField, myRowIsReferenced, myNoReferencedRow, myCheckConstraintFailed:
		return Constraint
	default:
		return ClassError
	}
}

// WriteError couples a failed write with the interpreter's verdict, so a caller
// can branch on the class and still report the original message.
type WriteError struct {
	Class Class
	Note  Note
	Err   error
}

func (e *WriteError) Error() string { return fmt.Sprintf("%s: %v", e.Class, e.Err) }
func (e *WriteError) Unwrap() error { return e.Err }

// AsWriteError extracts the verdict from an error chain.
func AsWriteError(err error) (*WriteError, bool) {
	var we *WriteError
	ok := errors.As(err, &we)
	return we, ok
}
