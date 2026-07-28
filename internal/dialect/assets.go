package dialect

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"spawnpoint/internal/sqlsplit"
)

// assetSet is the query loader half of the three-piece set: it resolves one
// engine's queries and migration scripts out of the embedded tree.
//
// The layout is the deployed one, unchanged (0006-D 3.5):
//
//	<dialect>/<group>.json          key -> filename mapping
//	<dialect>/<group>_<key>.sql     the query
//	migration/<dialect>/NNN_*.sql   the migration scripts
type assetSet struct {
	queryDir     string
	migrationDir string
}

func assetsFor(kind Kind) assetSet {
	return assetSet{
		queryDir:     path.Join(sqlRoot, string(kind)),
		migrationDir: path.Join(sqlRoot, "migration", string(kind)),
	}
}

// Migration is one migration script. Name is the bare filename, which is also
// the value recorded in the history table — never a path (0004-NR R3).
type Migration struct {
	Name string
	Text string
}

// migrationNamePattern is the NNN_<name>.sql convention (0008-L 1.5). The fixed
// three digits are what make a plain string sort the right order; a file called
// `10_x.sql` would sort before `9_x.sql` and run out of sequence, so the
// convention is enforced rather than assumed.
var migrationNamePattern = regexp.MustCompile(`^\d{3}_.+\.sql$`)

// Migrations returns this engine's migration scripts in the order they must be
// applied: ascending by filename, compared as strings (0004-NR R4).
func (a *Adapter) Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(sqlFS, a.assets.migrationDir)
	if err != nil {
		return nil, fmt.Errorf("migration scripts for %s: %w", a.kind, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out := make([]Migration, 0, len(names))
	for _, name := range names {
		text, err := a.assets.read(path.Join(a.assets.migrationDir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Name: name, Text: text})
	}
	return out, nil
}

// Query loads one query by group and key, following the group's .json mapping.
//
// The indirection is the deployed one's (sqloader) and is kept so the query
// files stay byte-identical with what a deployed database was built from.
// Resolution happens on every call rather than once at startup; the files are in
// memory already and the cost is a map lookup against a parsed document of five
// entries.
func (a *Adapter) Query(group, key string) (string, error) {
	mapping, err := a.assets.mapping(group)
	if err != nil {
		return "", err
	}
	file, ok := mapping[key]
	if !ok {
		return "", fmt.Errorf("query %s.%s: not listed in %s.json", group, key, group)
	}
	return a.assets.read(path.Join(a.assets.queryDir, file))
}

// MustQuery is Query for the queries the store issues, whose names are fixed at
// build time. A missing one is a defect in the embedded assets, and Validate
// has already refused to start the process by the time any of these run.
func (a *Adapter) MustQuery(group, key string) string {
	q, err := a.Query(group, key)
	if err != nil {
		panic(err)
	}
	return q
}

// mapping parses <group>.json.
func (s assetSet) mapping(group string) (map[string]string, error) {
	name := path.Join(s.queryDir, group+".json")
	raw, err := sqlFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("query group %q: %w", group, err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("query group %q: %w", group, err)
	}
	return m, nil
}

// read returns a file's text, refusing one that carries a byte order mark.
//
// 0006-D 3.5 requires the embedded files to be free of a BOM. A mark would end
// up in front of `CREATE` and the statement would fail to parse, exactly as it
// did for the loader these files were written for. Refusing at the boundary
// means the failure is
// reported as a defective asset at startup rather than as a syntax error in the
// middle of applying a migration.
func (s assetSet) read(name string) (string, error) {
	raw, err := sqlFS.ReadFile(name)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(string(raw), "\uFEFF") {
		return "", fmt.Errorf("%s: file begins with a byte order mark", name)
	}
	return string(raw), nil
}

// Validate checks this engine's embedded assets. It runs at startup, before the
// database is opened, and a failure is unrecoverable: the assets are compiled
// into the binary, so every restart produces the same result (0008-L 2.15, 3.2).
func (a *Adapter) Validate() error {
	entries, err := fs.ReadDir(sqlFS, a.assets.migrationDir)
	if err != nil {
		return fmt.Errorf("migration scripts for %s: %w", a.kind, err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !migrationNamePattern.MatchString(name) {
			return fmt.Errorf("migration %q does not match NNN_<name>.sql", name)
		}
		text, err := a.assets.read(path.Join(a.assets.migrationDir, name))
		if err != nil {
			return err
		}
		// The splitter cannot see inside a BEGIN … END body, so it would cut a
		// compound statement into fragments and execute them one by one. The
		// scripts are required not to contain one, and this is where that
		// requirement is enforced (0008-L 2.8).
		if sqlsplit.BareBegin(text) {
			return fmt.Errorf("migration %q contains a compound statement (BEGIN), which the splitter cannot handle", name)
		}
		if len(sqlsplit.Statements(text)) == 0 {
			return fmt.Errorf("migration %q contains no executable statement", name)
		}
		seen++
	}
	if seen == 0 {
		return fmt.Errorf("no migration scripts embedded for %s", a.kind)
	}
	return a.validateQueries()
}

// validateQueries checks that every key in every .json mapping resolves to a
// file that exists and is free of a BOM. A typo in a mapping would otherwise
// surface as a failed request long after startup.
func (a *Adapter) validateQueries() error {
	entries, err := fs.ReadDir(sqlFS, a.assets.queryDir)
	if err != nil {
		return fmt.Errorf("queries for %s: %w", a.kind, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		group := strings.TrimSuffix(e.Name(), ".json")
		mapping, err := a.assets.mapping(group)
		if err != nil {
			return err
		}
		if len(mapping) == 0 {
			return fmt.Errorf("query group %q: mapping is empty", group)
		}
		for key := range mapping {
			if _, err := a.Query(group, key); err != nil {
				return err
			}
		}
	}
	return nil
}
