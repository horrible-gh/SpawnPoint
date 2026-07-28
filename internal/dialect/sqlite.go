package dialect

import (
	"errors"

	"modernc.org/sqlite"
)

// sqliteDriver is the name modernc.org/sqlite registers with database/sql.
//
// The driver is a pure-Go translation of SQLite, chosen over the cgo binding
// because 0006-D 2.3 wants one executable that builds and runs without a C
// toolchain on the machine. It is the first external dependency the rewrite has
// taken; writing a storage engine by hand was the only alternative.
const sqliteDriver = "sqlite"

// sqliteDSN builds the data source name for a database file.
//
// The path is passed through as-is, with no `file:` prefix. That is deliberate:
// the driver only treats a DSN as a URI when it is prefixed, and a Windows path
// such as `C:\ProgramData\SpawnPoint\spawnpoint.db` is not one — as a URI its
// drive letter reads as a scheme and its separators need escaping. Without the
// prefix the driver hands the path to the engine unchanged and still applies
// the query parameters, which is exactly what is wanted here.
//
// Two pragmas are pinned rather than left at their defaults:
//
//   - busy_timeout, because the server can have more than one connection open
//     on the same file and the default behaviour is to fail the write
//     immediately rather than wait. A write that failed on a lock would reach
//     the error interpreter as an unclassifiable code and be reported to the
//     caller as a storage error.
//   - foreign_keys, because SQLite leaves enforcement off per connection. The
//     schema declares no foreign keys today, so this changes nothing now and
//     keeps a future one from being silently ignored.
//
// Nothing else is set. In particular the journal mode is left alone: the file
// is an existing database shared with the current implementation (0006-D 2.3),
// and switching it to WAL would change the file's format for every other reader.
func sqliteDSN(location string) string {
	return location + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}

// sqliteCode pulls the extended result code out of a driver error.
//
// Extended codes are what makes 0008-L 2.10 work at all: the primary code for
// every constraint violation is 19, so a duplicate key (1555, 2067) is only
// distinguishable from a check violation (275) at the extended level. The
// driver reports the extended code, which was verified against the real engine
// rather than assumed — see TestSQLiteCodesAreExtended.
func sqliteCode(err error) (int, bool) {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return 0, false
	}
	return se.Code(), true
}
