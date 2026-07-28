// Package spawnpoint exists for one reason: to carry the SQL asset tree into
// the executable.
//
// 0006-D 3.5 requires the embedded queries and migration scripts to keep the
// same directory layout and the same filenames as the current implementation.
// The safest way to satisfy that is not to copy the files but to embed the very
// ones the Python implementation reads, so the two can never drift apart — and
// drifting migration scripts are precisely how a rewrite ends up re-applying
// history to a live database.
//
// go:embed cannot reach outside the directory of the package that declares it,
// which is why this file sits at the module root rather than under internal/.
// Everything that interprets the tree lives in internal/sqlassets.
package spawnpoint

import "embed"

// SQL is spawnpoint/sql: the per-dialect query files with their .json key
// mappings, and migration/<dialect>/NNN_*.sql.
//
//go:embed spawnpoint/sql
var SQL embed.FS

// SQLRoot is the prefix every path in SQL carries.
const SQLRoot = "spawnpoint/sql"
