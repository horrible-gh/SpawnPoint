package dialect

import "embed"

// sqlFS carries the SQL asset tree into the executable: the per-dialect query
// files with their .json key mappings, and migration/<dialect>/NNN_*.sql.
//
// 0006-D 3.5 requires the embedded queries and migration scripts to keep the
// same directory layout and the same filenames. The tree used to live outside
// the Go packages so that the Python implementation and this one read the very
// same files and could not drift apart; with the Python implementation gone,
// there is no second reader, and the assets belong next to the only code that
// interprets them.
//
// go:embed cannot reach outside the directory of the package that declares it,
// which is why the tree sits here rather than under a shared directory.
//
//go:embed sql
var sqlFS embed.FS

// sqlRoot is the prefix every path in sqlFS carries.
const sqlRoot = "sql"
