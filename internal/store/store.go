// Package store is SpawnPoint's database access.
//
// It reads and writes the same file the current implementation uses, with the
// same schema and the same migration history (0006-D 2.3, 3.6). That constraint
// shapes everything here: the history table keeps its name and its columns, the
// values written into it are bare filenames, and the applied-at timestamp is
// left for the column default to fill in. Get any of those wrong and the
// rewrite re-applies migrations that ran months ago.
//
// Everything engine-specific — the driver, the queries, the meaning of an error
// code — comes from internal/dialect, so this file contains no SQL text of its
// own except the history table's own definition.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"spawnpoint/internal/dialect"
	"spawnpoint/internal/sqlsplit"
)

// Store is an open database.
type Store struct {
	db      *sql.DB
	adapter *dialect.Adapter
}

// Open connects to location and verifies the connection is usable.
//
// database/sql opens lazily, so without the ping a wrong path or an
// unreadable file would not surface here but at the first query — after
// startup had already reported success (0008-L 2.15).
func Open(location string) (*Store, error) {
	return OpenDialect(dialect.Default, location)
}

// OpenDialect is Open for an explicitly chosen engine.
func OpenDialect(kind dialect.Kind, location string) (*Store, error) {
	adapter, err := dialect.Select(kind)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(adapter.Driver(), adapter.DSN(location))
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", location, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", location, err)
	}
	return &Store{db: db, adapter: adapter}, nil
}

// Close releases the database. Step ⑤ of the shutdown sequence.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Adapter exposes the dialect adapter, so callers that need to interpret an
// error of their own do not have to select one again.
func (s *Store) Adapter() *dialect.Adapter { return s.adapter }

// --- Initial configuration (migrations) --------------------------------------

// migrationsTable is the history table's name. It is the current
// implementation's and is not changed: a new name would leave the existing
// history unread and every migration would look pending (0004-NR R1).
const migrationsTable = "migrations"

// migrationsDDL creates the history table.
//
// The indentation is not an accident and should not be tidied. SQLite stores
// the text of a CREATE statement verbatim in its schema, so this is what the
// deployed implementation's copy looks like on disk (sqloader's
// create_migrations_table). Reproducing it byte for byte is what lets the
// schema of a database built by this code be compared against one built by the
// deployed implementation and come out identical — which is the check 0008-L 6.3
// item 4 asks for. 0004-NR R2 requires the definition to be the same; this
// makes "the same" mechanically verifiable rather than a matter of reading.
const migrationsDDL = `
                CREATE TABLE IF NOT EXISTS migrations (
                    filename TEXT PRIMARY KEY,
                    applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
                );
            `

// Migrate applies the embedded scripts that are not recorded as applied yet.
//
// It reports how many were already recorded and how many it has just applied,
// which are the two numbers the `migrations` operations record carries
// (0008-L 2.9).
func (s *Store) Migrate() (already, applied int, err error) {
	scripts, err := s.adapter.Migrations()
	if err != nil {
		return 0, 0, err
	}
	if _, err := s.db.Exec(migrationsDDL); err != nil {
		return 0, 0, fmt.Errorf("ensure %s table: %w", migrationsTable, err)
	}
	done, err := s.appliedMigrations()
	if err != nil {
		return 0, 0, err
	}

	for _, m := range scripts {
		// The comparison is by filename alone, as a set membership test. Not by
		// count, not by the highest number seen: the current implementation
		// records bare filenames and nothing else, so this is the only test that
		// agrees with the rows already in the database (0004-NR R3, R5).
		if _, ok := done[m.Name]; ok {
			continue
		}
		if err := s.applyOne(m); err != nil {
			return len(done), applied, err
		}
		applied++
	}
	return len(done), applied, nil
}

// applyOne runs one script's statements and records it, in a single
// transaction.
//
// The current implementation commits the statements first and then inserts the
// history row separately, which leaves a window where the schema changed but
// nothing says so — and the next start would run the same script again against
// a database that already has the tables. Merging the two closes that window
// and is compatible with the existing rows, since nothing about them depends on
// how they were committed (0004-NR R6).
func (s *Store) applyOne(m dialect.Migration) error {
	statements := sqlsplit.Statements(m.Text)
	if len(statements) == 0 {
		return fmt.Errorf("migration %s: no executable statement", m.Name)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	for i, stmt := range statements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migration %s, statement %d: %w", m.Name, i+1, err)
		}
	}
	// filename only, never a path, and applied_at is left out so the column
	// default fills it in the engine's own format (0004-NR R3, R8).
	if _, err := tx.Exec("INSERT INTO "+migrationsTable+" (filename) VALUES (?)", m.Name); err != nil {
		return fmt.Errorf("migration %s: record history: %w", m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	return nil
}

// appliedMigrations reads the filenames already recorded.
func (s *Store) appliedMigrations() (map[string]struct{}, error) {
	rows, err := s.db.Query("SELECT filename FROM " + migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", migrationsTable, err)
	}
	defer rows.Close()

	done := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read %s: %w", migrationsTable, err)
		}
		done[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", migrationsTable, err)
	}
	return done, nil
}

// --- Identifier sequence ------------------------------------------------------

// NextSeq increments the counter for datePart and returns the new value.
//
// The increment and the read are one transaction. Split apart — which is what
// happens with a connection pool if they are two statements — two concurrent
// requests can both read the value after both increments and be handed the same
// sequence number (0008-L 2.11).
func (s *Store) NextSeq(datePart string) (int, error) {
	upsert, err := s.adapter.Query("spawn_daily_seq", "upsert")
	if err != nil {
		return 0, err
	}
	selectLast, err := s.adapter.Query("spawn_daily_seq", "select_last_seq")
	if err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.Exec(upsert, datePart, storageNow()); err != nil {
		return 0, fmt.Errorf("daily sequence for %s: %w", datePart, err)
	}
	var last int
	if err := tx.QueryRow(selectLast, datePart).Scan(&last); err != nil {
		return 0, fmt.Errorf("daily sequence for %s: %w", datePart, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("daily sequence for %s: %w", datePart, err)
	}
	return last, nil
}

// --- Instances ----------------------------------------------------------------

// Insert stores one instance.
//
// A failure is returned as a *dialect.WriteError carrying the interpreter's
// verdict, because the caller's next move depends on it: a duplicate key is
// retried with a fresh random tail or resolved into the caller's existing
// instance, anything else is a storage error (0008-L 2.11).
func (s *Store) Insert(inst Instance) error {
	query, err := s.adapter.Query("spawn_instance", "insert")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(query,
		inst.ID,
		inst.Requester,
		inst.Kind,
		inst.Status,
		nullString(inst.RequestKey),
		nullString(inst.Label),
		inst.TTLSeconds,
		storageTime(inst.CreatedAt),
		storageTime(inst.ExpiresAt),
	)
	if err == nil {
		return nil
	}
	class, note := s.adapter.Interpret(err)
	return &dialect.WriteError{Class: class, Note: note, Err: err}
}

// FindActiveByKey returns the most recent instance carrying requestKey that was
// created inside the deduplication window, or nil.
//
// The window boundary is compared as a fixed-width string, which is why the
// stored timestamp format is truncated to three fraction digits rather than
// rounded: rounding would move a row across the boundary relative to the value
// the current implementation wrote (0008-L 1.6, 2.12).
func (s *Store) FindActiveByKey(requestKey string, now time.Time, window time.Duration) (*Instance, error) {
	query, err := s.adapter.Query("spawn_instance", "find_active_by_key")
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(query, requestKey, storageTime(now.Add(-window)))

	var (
		inst                    Instance
		requestKeyCol, labelCol sql.NullString
		createdAt, expiresAt    timestamp
	)
	err = row.Scan(&inst.ID, &inst.Requester, &inst.Kind, &inst.Status,
		&requestKeyCol, &labelCol, &inst.TTLSeconds, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find instance by request key: %w", err)
	}
	inst.RequestKey = fromNull(requestKeyCol)
	inst.Label = fromNull(labelCol)
	inst.CreatedAt = createdAt.Time
	inst.ExpiresAt = expiresAt.Time
	return &inst, nil
}

// --- Runner entries -----------------------------------------------------------

// SaveEntry inserts or updates one registered command.
//
// Only the registration is stored, never the live state: a restart brings the
// entries back as stopped because nothing can prove a process the previous
// server started is still the one running under that identifier (0006-D 3.1).
func (s *Store) SaveEntry(e RunnerEntry) error {
	query, err := s.adapter.Query("runner_entry", "upsert")
	if err != nil {
		return err
	}
	env, err := encodeEnv(e.Env)
	if err != nil {
		return fmt.Errorf("runner entry %s: %w", e.ID, err)
	}
	ts := storageNow()
	// created_at and updated_at are given the same value; the upsert keeps the
	// original created_at and takes only the new updated_at, so an update does
	// not disturb the ordering restore relies on (0008-L 6.5).
	_, err = s.db.Exec(query, e.ID, e.Label, e.Cmd, nullString(e.Cwd), env, ts, ts)
	if err != nil {
		class, note := s.adapter.Interpret(err)
		return &dialect.WriteError{Class: class, Note: note, Err: err}
	}
	return nil
}

// DeleteEntry removes one registered command.
func (s *Store) DeleteEntry(id string) error {
	query, err := s.adapter.Query("runner_entry", "delete")
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(query, id); err != nil {
		class, note := s.adapter.Interpret(err)
		return &dialect.WriteError{Class: class, Note: note, Err: err}
	}
	return nil
}

// ListEntries returns the registered commands in restore order: registration
// time ascending, identifier ascending on a tie (0008-L 6.5). The ordering is
// the query's, not this function's, so it stays with the rest of the SQL.
func (s *Store) ListEntries() ([]RunnerEntry, error) {
	query, err := s.adapter.Query("runner_entry", "list")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RunnerEntry
	for rows.Next() {
		var (
			e                    RunnerEntry
			cwd, env             sql.NullString
			createdAt, updatedAt timestamp
		)
		if err := rows.Scan(&e.ID, &e.Label, &e.Cmd, &cwd, &env, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("list runner entries: %w", err)
		}
		e.Cwd = fromNull(cwd)
		// A corrupted env decodes to an empty map rather than failing the
		// restore: losing one entry's environment is recoverable, refusing to
		// start is not (spawnpoint/storage.py _decode_env).
		e.Env = decodeEnv(env.String)
		e.CreatedAt = createdAt.Time
		e.UpdatedAt = updatedAt.Time
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- helpers ------------------------------------------------------------------

// PingContext lets the caller confirm the database is still answering.
func (s *Store) PingContext(ctx context.Context) error { return s.db.PingContext(ctx) }

// DB exposes the handle for tests and for the parts of the server that need to
// run a query the asset tree does not contain.
func (s *Store) DB() *sql.DB { return s.db }
