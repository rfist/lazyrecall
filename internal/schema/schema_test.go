package schema

import (
	"os/exec"
	"path/filepath"
	"testing"

	"recall/internal/sqlitex"
)

func testRunner(t *testing.T) *sqlitex.Runner {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	return &sqlitex.Runner{BinPath: bin, DBPath: filepath.Join(dir, "recall.db"), TmpDir: dir}
}

func TestOpenFreshDatabase(t *testing.T) {
	r := testRunner(t)
	v, err := Open(r)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if v != CurrentVersion {
		t.Fatalf("got version %d, want %d", v, CurrentVersion)
	}

	// Both annotation and index tables must exist.
	for _, table := range []string{"sessions", "lineages", "comments", "tags", "cursors", "prompt_fts"} {
		var rows []struct {
			Name string `json:"name"`
		}
		if err := r.Query(`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name = '`+table+`';`, &rows); err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if len(rows) == 0 {
			t.Errorf("expected table %s to exist after Open", table)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r); err != nil {
		t.Fatalf("second Open should be a no-op, got: %v", err)
	}
}

func TestSchemaBumpRebuildsIndexButKeepsAnnotations(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}

	// Seed a lineage + comment (annotation data) and a session (index data).
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned) VALUES ('lin_1', 'personal', 0);`); err != nil {
		t.Fatal(err)
	}
	if err := r.Exec(`INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('lin_1', 'keep me', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	if err := r.Exec(`INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state) VALUES ('s1','claude','s1','lin_1','completed');`); err != nil {
		t.Fatal(err)
	}

	// Simulate a schema bump by raising CurrentVersion, forcing the
	// "stored < CurrentVersion" migrate-and-rebuild path on next Open.
	orig := CurrentVersion
	CurrentVersion = orig + 1
	defer func() { CurrentVersion = orig }()

	if _, err := Open(r); err != nil {
		t.Fatalf("Open after version bump: %v", err)
	}

	var sessions []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM sessions;`, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected index (sessions) to be discarded on rebuild, found %d rows", len(sessions))
	}

	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.Query(`SELECT body FROM comments;`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Errorf("expected annotation (comment) to survive rebuild, got %v", comments)
	}

	var lineages []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM lineages;`, &lineages); err != nil {
		t.Fatal(err)
	}
	if len(lineages) != 1 {
		t.Errorf("expected lineage to survive rebuild, got %v", lineages)
	}
}

// TestFreshDatabaseHasHandleColumnWithUniquenessConstraint covers task 1.1:
// a fresh install at the current schema version must have the handle column
// and its uniqueness constraint (unique per profile - design.md decision 2)
// without going through the migration path at all.
func TestFreshDatabaseHasHandleColumnWithUniquenessConstraint(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_a', 'p', 0, 1);`); err != nil {
		t.Fatal(err)
	}
	// A second lineage in the same profile claiming the same handle must
	// violate the uniqueness constraint.
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_b', 'p', 0, 1);`); err == nil {
		t.Fatal("expected a uniqueness violation for a duplicate (profile, handle) pair")
	}
	// The same handle number in a different profile is not a conflict -
	// handles are per profile (design.md decision 2), not global.
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_c', 'q', 0, 1);`); err != nil {
		t.Fatalf("expected the same handle number in a different profile to be allowed: %v", err)
	}
}

// TestMigrationBackfillsHandlesOldestFirst covers tasks 1.3/1.4: an
// existing database at schema version 1 (created before the handle column
// existed) must, on the next Open, gain the handle column and have it
// backfilled for every existing lineage, ordered so the lineage whose
// earliest session is chronologically first gets the lowest handle - and
// this must happen per profile, never mixing two profiles' numbering.
func TestMigrationBackfillsHandlesOldestFirst(t *testing.T) {
	r := testRunner(t)

	// Hand-build a v1-shaped database: schema_meta at version 1, and
	// lineages/sessions without ever going through the current handle-aware
	// annotationDDL. This simulates a real pre-existing install.
	v1DDL := `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO schema_meta (key, value) VALUES ('schema_version', '1');
CREATE TABLE lineages (id TEXT PRIMARY KEY, profile TEXT NOT NULL, orphaned INTEGER NOT NULL DEFAULT 0);
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, source_session_id TEXT, lineage_id TEXT, end_state TEXT NOT NULL, started_at INTEGER);
INSERT INTO lineages (id, profile, orphaned) VALUES ('lin_newest', 'p', 0);
INSERT INTO lineages (id, profile, orphaned) VALUES ('lin_oldest', 'p', 0);
INSERT INTO lineages (id, profile, orphaned) VALUES ('lin_other_profile', 'q', 0);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, started_at) VALUES ('s1', 'claude', 's1', 'lin_newest', 'completed', 2000);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, started_at) VALUES ('s2', 'claude', 's2', 'lin_oldest', 'completed', 1000);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, started_at) VALUES ('s3', 'claude', 's3', 'lin_other_profile', 'completed', 500);
`
	if err := r.Exec(v1DDL); err != nil {
		t.Fatalf("seeding v1 database: %v", err)
	}

	v, err := Open(r)
	if err != nil {
		t.Fatalf("Open (migrating v1 -> current): %v", err)
	}
	if v != CurrentVersion {
		t.Fatalf("got version %d, want %d", v, CurrentVersion)
	}

	var rows []struct {
		ID      string `json:"id"`
		Profile string `json:"profile"`
		Handle  int    `json:"handle"`
	}
	if err := r.Query(`SELECT id, profile, handle FROM lineages ORDER BY id;`, &rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for _, row := range rows {
		byID[row.ID] = row.Handle
	}
	if byID["lin_oldest"] == 0 || byID["lin_newest"] == 0 || byID["lin_other_profile"] == 0 {
		t.Fatalf("expected every lineage to receive a nonzero handle, got %+v", byID)
	}
	if byID["lin_oldest"] >= byID["lin_newest"] {
		t.Errorf("expected the oldest lineage (earliest session started_at) to take the lower handle: oldest=%d newest=%d", byID["lin_oldest"], byID["lin_newest"])
	}
	// A different profile's lineage must not compete for the same handle
	// sequence - it should independently start at 1, not continue profile
	// "p"'s numbering.
	if byID["lin_other_profile"] != 1 {
		t.Errorf("expected profile q's lone lineage to get handle 1 (its own counter), got %d", byID["lin_other_profile"])
	}

	// The v1 index table (sessions) must still have been discarded and
	// rebuilt empty by the same Open call, per the existing rebuild rule -
	// the migration must not interfere with that.
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM sessions;`, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected the disposable index to still be discarded after migrating, got %d rows", len(sessions))
	}
}

// TestHandleSurvivesIndexRebuild covers task 1.4 via the normal (already
// current-version) rebuild path: bumping CurrentVersion and reopening must
// not disturb a handle already assigned.
func TestHandleSurvivesIndexRebuild(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_1', 'p', 0, 7);`); err != nil {
		t.Fatal(err)
	}

	orig := CurrentVersion
	CurrentVersion = orig + 1
	defer func() { CurrentVersion = orig }()
	if _, err := Open(r); err != nil {
		t.Fatalf("Open after version bump: %v", err)
	}

	var rows []struct {
		Handle int `json:"handle"`
	}
	if err := r.Query(`SELECT handle FROM lineages WHERE id = 'lin_1';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Handle != 7 {
		t.Fatalf("expected handle 7 to survive a rebuild, got %+v", rows)
	}
}
