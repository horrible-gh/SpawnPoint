// Package dialect is the three-piece set 0006-D 3.5 asked for: a dialect
// selector, a query loader and an error interpreter, bundled so that swapping
// the database engine is a change here and nowhere else.
//
// The third piece is the one this rewrite adds. The current implementation
// decides whether a write failed on a duplicate key by looking for words in the
// driver's error message (spawnpoint/storage.py: `"unique" in msg`). That works
// only for SQLite's English text, so on any other engine the check stops
// matching — silently, since a missed duplicate looks exactly like an ordinary
// failure. The duplicate-request path that returns the caller's existing
// instance goes with it. 0008-L 2.10 replaces the text matching with the
// engine's numeric result code, and this package holds the tables.
package dialect

import "fmt"

// Kind names a database engine. The value is also the directory name inside
// the embedded asset tree, which is what keeps the query files for an engine
// discoverable without a second registry.
type Kind string

const (
	SQLite     Kind = "sqlite"
	PostgreSQL Kind = "postgres"
	MySQL      Kind = "mysql"
)

// Default is the engine in use. The current deployment is SQLite and 0006-D 2.3
// keeps the existing database file, so nothing selects anything else yet; the
// constant is the single place a future switch would touch.
const Default = SQLite

// Adapter is one engine's three pieces bound together.
type Adapter struct {
	kind Kind
	// driver is the database/sql driver name for this engine, empty when no
	// driver is linked into the binary.
	driver string
	// dsn turns a configured database location into the driver's data source
	// name.
	dsn func(location string) string
	// code extracts the engine's error code from a driver error. It reports
	// false when the error did not come from the engine or carries no code.
	code func(err error) (int, bool)
	// classify maps a code to a write outcome.
	classify func(code int) Class
	// assets serves this engine's queries and migration scripts.
	assets assetSet
}

// Select returns the adapter for kind.
func Select(kind Kind) (*Adapter, error) {
	a, ok := adapters[kind]
	if !ok {
		return nil, fmt.Errorf("unknown database dialect %q", kind)
	}
	if a.driver == "" {
		// The error tables and query files for an engine are present long
		// before anyone links its driver, so this has to be a clear message
		// rather than a confusing failure inside database/sql.
		return nil, fmt.Errorf("database dialect %q has no driver linked into this build", kind)
	}
	return a, nil
}

// adapters is the registry. Entries without a driver still carry a complete
// error table: 0008-L 2.10 fixed the classification for all three engines, and
// a table that is written and tested now cannot be forgotten at the moment the
// engine is actually switched.
var adapters = map[Kind]*Adapter{
	SQLite: {
		kind:     SQLite,
		driver:   sqliteDriver,
		dsn:      sqliteDSN,
		code:     sqliteCode,
		classify: classifySQLite,
		assets:   assetsFor(SQLite),
	},
	PostgreSQL: {
		kind:     PostgreSQL,
		classify: classifyPostgreSQL,
		assets:   assetsFor(PostgreSQL),
	},
	MySQL: {
		kind:     MySQL,
		classify: classifyMySQL,
		assets:   assetsFor(MySQL),
	},
}

// Kind reports which engine this adapter drives.
func (a *Adapter) Kind() Kind { return a.kind }

// Driver is the database/sql driver name to open with.
func (a *Adapter) Driver() string { return a.driver }

// DSN turns the configured database location into a data source name.
func (a *Adapter) DSN(location string) string { return a.dsn(location) }
