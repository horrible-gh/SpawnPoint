package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// liveEnv gates the tests that run the current Python implementation. The
// convention is the one T1 established for the tests that create processes:
// an ordinary `go test ./...` starts nothing, so the suite stays runnable on a
// machine with no Python and no dependencies installed.
const liveEnv = "SPAWNPOINT_LIVE_PYREF"

// TestExistingPythonDatabaseIsNotMigratedAgain is item 3 of 0008-L 6.3 in its
// strongest form.
//
// The other migration tests use a fixture captured from the Python
// implementation. This one runs it: the database is built by the code that
// built the deployed one, and the Go migration runner is then pointed at the
// result. It is the only check that covers the whole compatibility chain at
// once — the history table's name, the bare filenames in it, and the set
// comparison that reads them (0004-NR R1, R3, R5).
//
// If this fails, the rewrite would re-apply the migrations to the production
// database on its first start.
func TestExistingPythonDatabaseIsNotMigratedAgain(t *testing.T) {
	requireLive(t)

	path := filepath.Join(t.TempDir(), "python_built.db")
	buildWithPython(t, path)

	s := openAt(t, path)
	already, applied, err := s.Migrate()
	if err != nil {
		t.Fatalf("Migrate over a Python-built database: %v", err)
	}
	if already != 3 {
		t.Errorf("read %d rows of existing history, want 3 — the history was not found", already)
	}
	if applied != 0 {
		t.Errorf("applied %d migrations to an already-migrated database, want 0", applied)
	}

	// The history must also be untouched, not merely un-grown.
	got := dumpSchema(t, s.DB())
	want := loadReference(t)
	if !reflect.DeepEqual(got.Migrations, want.Migrations) {
		t.Errorf("history rows:\n got %v\nwant %v", got.Migrations, want.Migrations)
	}
	if !reflect.DeepEqual(got.Objects, want.Objects) {
		reportSchemaDifference(t, got.Objects, want.Objects)
	}
}

// TestReferenceFixtureIsCurrent catches the fixture drifting away from the
// implementation it was captured from — after a migration script is edited, for
// example. Without it, every other test in this file would keep passing against
// a stale reference.
func TestReferenceFixtureIsCurrent(t *testing.T) {
	requireLive(t)

	path := filepath.Join(t.TempDir(), "fresh.db")
	buildWithPython(t, path)

	fresh := openAt(t, path)
	got := dumpSchema(t, fresh.DB())
	want := loadReference(t)

	if !reflect.DeepEqual(got.Objects, want.Objects) {
		t.Error("internal/store/testdata/python_schema.json is stale;" +
			" regenerate with: python tools/pyref/dump_schema.py > internal/store/testdata/python_schema.json")
		reportSchemaDifference(t, got.Objects, want.Objects)
	}
	if !reflect.DeepEqual(got.Migrations, want.Migrations) {
		t.Errorf("history rows:\n got %v\nwant %v", got.Migrations, want.Migrations)
	}
}

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnv) == "" {
		t.Skipf("set %s=1 to run the current Python implementation", liveEnv)
	}
}

// buildWithPython creates a database at path by opening the current
// implementation's Registry against it, which applies the migrations exactly as
// a deployed server would.
func buildWithPython(t *testing.T, path string) {
	t.Helper()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	// The migrator announces each file on stdout; redirecting it keeps the
	// command's output to whatever actually went wrong.
	script := strings.Join([]string{
		"import contextlib, sys",
		"sys.path.insert(0, sys.argv[1])",
		"from spawnpoint.storage import Registry",
		"with contextlib.redirect_stdout(sys.stderr):",
		"    registry = Registry.open(sys.argv[2])",
		"registry.close()",
	}, "\n")

	cmd := exec.Command("python", "-c", script, repoRoot, path)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the database with the Python implementation: %v\n%s", err, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the Python implementation did not create %s: %v", path, err)
	}
}
