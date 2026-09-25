package schema

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/sqlitex"
)

func testRunner(t *testing.T) *sqlitex.Runner {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	return &sqlitex.Runner{BinPath: bin, DBPath: filepath.Join(dir, "lazyrecall.db"), TmpDir: dir}
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

// TestFreshDatabaseHasHandleColumnWithUniquenessConstraint covers task 1.1,
// updated for change group-sessions-in-one-index: a fresh install at the
// current schema version must have the handle column and its uniqueness
// constraint, which is now unique across the whole index
// (idx_lineages_handle) rather than per profile (design.md decision 2 no
// longer applies - one index now serves every profile, so two profiles'
// lineages sharing a handle number would collide in the same listing).
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
		t.Fatal("expected a uniqueness violation for a duplicate handle in the same profile")
	}
	// The same handle number in a different profile is now also a
	// conflict: idx_lineages_handle is unique on handle alone, not on
	// (profile, handle) the way idx_lineages_handle_profile was.
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_c', 'q', 0, 1);`); err == nil {
		t.Fatal("expected a uniqueness violation for the same handle in a different profile too - handles are global now")
	}
	// A distinct handle number in a different profile is unaffected.
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_d', 'q', 0, 2);`); err != nil {
		t.Fatalf("expected a distinct handle number in a different profile to be allowed: %v", err)
	}
}

// TestMigrationBackfillsHandlesOldestFirst covers tasks 1.3/1.4: an
// existing database at schema version 1 (created before the handle column
// existed) must, on the next Open, gain the handle column and have it
// backfilled for every existing lineage, ordered so the lineage whose
// earliest session is chronologically first gets the lowest handle.
//
// Every lineage here shares one profile, "p" - not because the backfill
// migration (migrations[2], which partitions by profile) stopped doing
// that, but because that partitioning is only ever exercised by a database
// that predates group-sessions-in-one-index, and design.md decision 1 was
// "one database per profile" for every version this migration could apply
// to: a real pre-existing v1 database never held two profiles' lineages
// side by side, so testing that scenario here would exercise a shape no
// real database has, and one that migrations[8]'s now-global handle
// uniqueness (idx_lineages_handle) cannot support - two profiles'
// lineages independently numbering from 1 would collide the moment both
// migrate into the same index.
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
INSERT INTO lineages (id, profile, orphaned) VALUES ('lin_middle', 'p', 0);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, started_at) VALUES ('s1', 'claude', 's1', 'lin_newest', 'completed', 3000);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, started_at) VALUES ('s2', 'claude', 's2', 'lin_oldest', 'completed', 1000);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, started_at) VALUES ('s3', 'claude', 's3', 'lin_middle', 'completed', 2000);
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
	if byID["lin_oldest"] == 0 || byID["lin_middle"] == 0 || byID["lin_newest"] == 0 {
		t.Fatalf("expected every lineage to receive a nonzero handle, got %+v", byID)
	}
	if byID["lin_oldest"] >= byID["lin_middle"] || byID["lin_middle"] >= byID["lin_newest"] {
		t.Errorf("expected handles to order oldest < middle < newest by session started_at, got %+v", byID)
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

// TestV3ToV4MigrationAddsArchiveColumnKeepsData covers change
// add-archive-facility: an existing database at schema version 3 (with
// lineages, comments, and tags already populated) must, on the next Open,
// gain the lineages.archived_at column with all of that annotation data
// intact. Archive state is user data, so it must be migrated forward
// exactly like every other annotation - never rebuilt.
func TestV3ToV4MigrationAddsArchiveColumnKeepsData(t *testing.T) {
	r := testRunner(t)

	// Hand-build a v3-shaped database: schema_meta at version 3, lineages
	// with handles (the v2 shape), comments, tags, and a v3 index table -
	// without ever going through the current archive-aware annotationDDL.
	v3DDL := `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO schema_meta (key, value) VALUES ('schema_version', '3');
CREATE TABLE lineages (id TEXT PRIMARY KEY, profile TEXT NOT NULL, orphaned INTEGER NOT NULL DEFAULT 0, handle INTEGER);
CREATE UNIQUE INDEX idx_lineages_handle_profile ON lineages(profile, handle);
CREATE TABLE comments (id INTEGER PRIMARY KEY AUTOINCREMENT, lineage_id TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_comments_lineage ON comments(lineage_id);
CREATE TABLE tags (lineage_id TEXT NOT NULL, tag TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (lineage_id, tag));
CREATE INDEX idx_tags_tag ON tags(tag);
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, source_session_id TEXT NOT NULL, lineage_id TEXT NOT NULL, end_state TEXT NOT NULL, name TEXT);
INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_1', 'p', 0, 1);
INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('lin_1', 'keep me', 1, 1);
INSERT INTO tags (lineage_id, tag, created_at) VALUES ('lin_1', 'urgent', 1);
`
	if err := r.Exec(v3DDL); err != nil {
		t.Fatalf("seeding v3 database: %v", err)
	}

	v, err := Open(r)
	if err != nil {
		t.Fatalf("Open (migrating v3 -> current): %v", err)
	}
	if v != CurrentVersion {
		t.Fatalf("got version %d, want %d", v, CurrentVersion)
	}

	var cols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(lineages);`, &cols); err != nil {
		t.Fatal(err)
	}
	hasArchivedAt := false
	for _, c := range cols {
		if c.Name == "archived_at" {
			hasArchivedAt = true
		}
	}
	if !hasArchivedAt {
		t.Errorf("expected lineages.archived_at to exist after migration, columns: %+v", cols)
	}

	// The annotation data seeded at v3 must be intact, with archived_at
	// untouched (NULL - nothing was ever archived).
	var comments []struct {
		LineageID string `json:"lineage_id"`
		Body      string `json:"body"`
	}
	if err := r.Query(`SELECT lineage_id, body FROM comments;`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Errorf("expected the comment to survive migration, got %+v", comments)
	}

	var tags []struct {
		LineageID string `json:"lineage_id"`
		Tag       string `json:"tag"`
	}
	if err := r.Query(`SELECT lineage_id, tag FROM tags;`, &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Tag != "urgent" {
		t.Errorf("expected the tag to survive migration, got %+v", tags)
	}

	var archived []struct {
		ArchivedAt *int64 `json:"archived_at"`
	}
	if err := r.Query(`SELECT archived_at FROM lineages WHERE id = 'lin_1';`, &archived); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ArchivedAt != nil {
		t.Errorf("expected a fresh lineage to be unarchived (archived_at NULL), got %+v", archived)
	}
}

// TestV4ToV5MigrationAddsOriginColumnKeepsData covers change
// add-session-origin: an existing database at schema version 4 (with
// lineages, comments, and tags already populated, including handle and
// archived_at) must, on the next Open, gain the sessions.origin column with
// all of that annotation data intact. sessions.origin is index data read
// back out of the transcripts, so the bump discards and rebuilds the index
// (the seeded session row is gone) - exactly like sessions.name did at v3 -
// while the annotations are migrated forward and never rebuilt.
func TestV4ToV5MigrationAddsOriginColumnKeepsData(t *testing.T) {
	r := testRunner(t)

	// Hand-build a v4-shaped database: schema_meta at version 4, lineages
	// with handle and archived_at (the v4 shape), comments, tags, and a v4
	// index table without origin - without ever going through the current
	// origin-aware indexDDL.
	v4DDL := `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO schema_meta (key, value) VALUES ('schema_version', '4');
CREATE TABLE lineages (id TEXT PRIMARY KEY, profile TEXT NOT NULL, orphaned INTEGER NOT NULL DEFAULT 0, handle INTEGER, archived_at INTEGER);
CREATE UNIQUE INDEX idx_lineages_handle_profile ON lineages(profile, handle);
CREATE TABLE comments (id INTEGER PRIMARY KEY AUTOINCREMENT, lineage_id TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_comments_lineage ON comments(lineage_id);
CREATE TABLE tags (lineage_id TEXT NOT NULL, tag TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (lineage_id, tag));
CREATE INDEX idx_tags_tag ON tags(tag);
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, source_session_id TEXT NOT NULL, lineage_id TEXT NOT NULL, end_state TEXT NOT NULL, name TEXT);
INSERT INTO lineages (id, profile, orphaned, handle, archived_at) VALUES ('lin_1', 'p', 0, 3, 1234);
INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('lin_1', 'keep me', 1, 1);
INSERT INTO tags (lineage_id, tag, created_at) VALUES ('lin_1', 'urgent', 1);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state) VALUES ('s1', 'claude', 's1', 'lin_1', 'completed');
`
	if err := r.Exec(v4DDL); err != nil {
		t.Fatalf("seeding v4 database: %v", err)
	}

	v, err := Open(r)
	if err != nil {
		t.Fatalf("Open (migrating v4 -> current): %v", err)
	}
	if v != CurrentVersion {
		t.Fatalf("got version %d, want %d", v, CurrentVersion)
	}

	var cols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(sessions);`, &cols); err != nil {
		t.Fatal(err)
	}
	hasOrigin := false
	for _, c := range cols {
		if c.Name == "origin" {
			hasOrigin = true
		}
	}
	if !hasOrigin {
		t.Errorf("expected sessions.origin to exist after migration, columns: %+v", cols)
	}

	// The index table was discarded and rebuilt (the seeded row is gone)...
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM sessions;`, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected the disposable index to still be discarded after migrating, got %d rows", len(sessions))
	}

	// ...while every annotation seeded at v4 is intact: the lineage row
	// with its handle and archive timestamp, the comment, and the tag.
	var lineage []struct {
		Handle     int   `json:"handle"`
		ArchivedAt int64 `json:"archived_at"`
	}
	if err := r.Query(`SELECT handle, archived_at FROM lineages WHERE id = 'lin_1';`, &lineage); err != nil {
		t.Fatal(err)
	}
	if len(lineage) != 1 || lineage[0].Handle != 3 || lineage[0].ArchivedAt != 1234 {
		t.Errorf("expected the lineage (handle, archived_at) to survive migration, got %+v", lineage)
	}

	var comments []struct {
		LineageID string `json:"lineage_id"`
		Body      string `json:"body"`
	}
	if err := r.Query(`SELECT lineage_id, body FROM comments;`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Errorf("expected the comment to survive migration, got %+v", comments)
	}

	var tags []struct {
		LineageID string `json:"lineage_id"`
		Tag       string `json:"tag"`
	}
	if err := r.Query(`SELECT lineage_id, tag FROM tags;`, &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Tag != "urgent" {
		t.Errorf("expected the tag to survive migration, got %+v", tags)
	}
}

// TestFreshDatabaseHasGroupNameAndInstallColumns covers change
// group-sessions-in-one-index: a fresh install at the current schema
// version must have lineages.group_name and sessions.install without going
// through the migration path at all, and the old per-profile handle index
// must not exist - only the new global one.
func TestFreshDatabaseHasGroupNameAndInstallColumns(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}

	var lineageCols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(lineages);`, &lineageCols); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(lineageCols, "group_name") {
		t.Errorf("expected lineages.group_name to exist on a fresh database, columns: %+v", lineageCols)
	}

	var sessionCols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(sessions);`, &sessionCols); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(sessionCols, "install") {
		t.Errorf("expected sessions.install to exist on a fresh database, columns: %+v", sessionCols)
	}

	var indexes []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'lineages';`, &indexes); err != nil {
		t.Fatal(err)
	}
	hasGlobal, hasOldComposite := false, false
	for _, idx := range indexes {
		switch idx.Name {
		case "idx_lineages_handle":
			hasGlobal = true
		case "idx_lineages_handle_profile":
			hasOldComposite = true
		}
	}
	if !hasGlobal {
		t.Errorf("expected idx_lineages_handle to exist on a fresh database, indexes: %+v", indexes)
	}
	if hasOldComposite {
		t.Errorf("expected idx_lineages_handle_profile to be gone on a fresh database, indexes: %+v", indexes)
	}
}

// TestGroupNameDefaultsNullAndSurvivesQueries covers change
// group-sessions-in-one-index: with no override ever recorded,
// lineages.group_name reads back NULL - "use the path rule," not an empty
// string or any other stand-in value that would be indistinguishable from a
// deliberate but empty override.
func TestGroupNameDefaultsNullAndSurvivesQueries(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Exec(`INSERT INTO lineages (id, profile, orphaned) VALUES ('lin_1', 'p', 0);`); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		GroupName *string `json:"group_name"`
	}
	if err := r.Query(`SELECT group_name FROM lineages WHERE id = 'lin_1';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GroupName != nil {
		t.Errorf("expected a fresh lineage's group_name to be NULL, got %+v", rows)
	}
}

// TestV7ToV8MigrationAddsGroupNameAndInstallKeepsDataAndSwapsIndex covers
// change group-sessions-in-one-index end to end: an existing database at
// schema version 7 (with lineages, comments, tags, handles and
// archived_at already populated under the old per-profile handle index)
// must, on the next Open, gain lineages.group_name (NULL, migrated
// forward like archived_at) and sessions.install (index data, rebuilt like
// origin and client were at v5/v6), keep every annotation intact, and end
// up with idx_lineages_handle in place of idx_lineages_handle_profile.
func TestV7ToV8MigrationAddsGroupNameAndInstallKeepsDataAndSwapsIndex(t *testing.T) {
	r := testRunner(t)

	v7DDL := `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO schema_meta (key, value) VALUES ('schema_version', '7');
CREATE TABLE lineages (id TEXT PRIMARY KEY, profile TEXT NOT NULL, orphaned INTEGER NOT NULL DEFAULT 0, handle INTEGER, archived_at INTEGER);
CREATE UNIQUE INDEX idx_lineages_handle_profile ON lineages(profile, handle);
CREATE TABLE comments (id INTEGER PRIMARY KEY AUTOINCREMENT, lineage_id TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_comments_lineage ON comments(lineage_id);
CREATE TABLE tags (lineage_id TEXT NOT NULL, tag TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (lineage_id, tag));
CREATE INDEX idx_tags_tag ON tags(tag);
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, source_session_id TEXT NOT NULL, lineage_id TEXT NOT NULL, end_state TEXT NOT NULL, name TEXT, origin TEXT, human_prompt INTEGER NOT NULL DEFAULT 0, client TEXT);
INSERT INTO lineages (id, profile, orphaned, handle, archived_at) VALUES ('lin_1', 'p', 0, 5, NULL);
INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('lin_1', 'keep me', 1, 1);
INSERT INTO tags (lineage_id, tag, created_at) VALUES ('lin_1', 'urgent', 1);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, origin, client) VALUES ('s1', 'claude', 's1', 'lin_1', 'completed', 'interactive', 'vscode');
`
	if err := r.Exec(v7DDL); err != nil {
		t.Fatalf("seeding v7 database: %v", err)
	}

	v, err := Open(r)
	if err != nil {
		t.Fatalf("Open (migrating v7 -> current): %v", err)
	}
	if v != CurrentVersion {
		t.Fatalf("got version %d, want %d", v, CurrentVersion)
	}

	var lineageCols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(lineages);`, &lineageCols); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(lineageCols, "group_name") {
		t.Errorf("expected lineages.group_name to exist after migration, columns: %+v", lineageCols)
	}

	var sessionCols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(sessions);`, &sessionCols); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(sessionCols, "install") {
		t.Errorf("expected sessions.install to exist after migration, columns: %+v", sessionCols)
	}

	// The index is disposable: the seeded session row must be gone, the
	// same as sessions.name was at v3 and sessions.origin was at v5.
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM sessions;`, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected the disposable index to still be discarded after migrating, got %d rows", len(sessions))
	}

	// Every annotation seeded at v7 must be intact, and group_name on the
	// pre-existing lineage must default to NULL rather than being backfilled
	// to anything.
	var lineage []struct {
		Handle     int     `json:"handle"`
		ArchivedAt *int64  `json:"archived_at"`
		GroupName  *string `json:"group_name"`
	}
	if err := r.Query(`SELECT handle, archived_at, group_name FROM lineages WHERE id = 'lin_1';`, &lineage); err != nil {
		t.Fatal(err)
	}
	if len(lineage) != 1 || lineage[0].Handle != 5 || lineage[0].ArchivedAt != nil || lineage[0].GroupName != nil {
		t.Errorf("expected the lineage (handle, archived_at, group_name) to survive migration, got %+v", lineage)
	}

	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.Query(`SELECT body FROM comments;`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Errorf("expected the comment to survive migration, got %+v", comments)
	}

	var tags []struct {
		Tag string `json:"tag"`
	}
	if err := r.Query(`SELECT tag FROM tags;`, &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Tag != "urgent" {
		t.Errorf("expected the tag to survive migration, got %+v", tags)
	}

	// The old composite index must be gone, replaced by the global one.
	var indexes []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'lineages';`, &indexes); err != nil {
		t.Fatal(err)
	}
	hasGlobal, hasOldComposite := false, false
	for _, idx := range indexes {
		switch idx.Name {
		case "idx_lineages_handle":
			hasGlobal = true
		case "idx_lineages_handle_profile":
			hasOldComposite = true
		}
	}
	if !hasGlobal {
		t.Errorf("expected idx_lineages_handle to exist after migration, indexes: %+v", indexes)
	}
	if hasOldComposite {
		t.Errorf("expected idx_lineages_handle_profile to be gone after migration, indexes: %+v", indexes)
	}
}

// TestFreshDatabaseHasCustomNameColumn covers the follow-up to change
// show-session-names (that change's own non-goal deferred in-lazyrecall
// naming as "a separate, annotation-shaped capability"): a fresh install
// at the current schema version must have lineages.custom_name.
func TestFreshDatabaseHasCustomNameColumn(t *testing.T) {
	r := testRunner(t)
	if _, err := Open(r); err != nil {
		t.Fatal(err)
	}
	var cols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(lineages);`, &cols); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(cols, "custom_name") {
		t.Errorf("expected lineages.custom_name to exist on a fresh database, columns: %+v", cols)
	}
}

// TestV9ToV10MigrationAddsCustomNameKeepsData covers an existing database
// at schema version 9 (with lineages, comments, tags, handle, archived_at
// and group_name already populated): on the next Open it must gain
// lineages.custom_name (NULL, migrated forward like group_name was at v8)
// while every existing annotation stays intact and the disposable index is
// discarded and rebuilt as usual.
// TestV9ToV10MigrationAddsCustomNameKeepsData covers the fix that made v10
// annotation-only (annotationOnly[10]): unlike every migration test above
// it, which built a v-shaped `sessions` table by hand and expected it gone
// after Open (the index used to be unconditionally dropped and rebuilt on
// any version bump), this one seeds the REAL index tables via indexDDL -
// the v9 and v10 index shapes are identical, since v10 touches only
// lineages - with rows in sessions, prompt_fts, and cursors, and asserts
// they are all still there after migrating. A session's row is the only
// record of it once its transcript is gone (Claude Code deletes transcripts
// after 30 days), so dropping the index here would permanently erase such
// sessions for no reason connected to what this migration actually
// changes - the exact regression a coordinator review caught before this
// change shipped.
func TestV9ToV10MigrationAddsCustomNameKeepsData(t *testing.T) {
	r := testRunner(t)

	v9DDL := `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO schema_meta (key, value) VALUES ('schema_version', '9');
CREATE TABLE lineages (id TEXT PRIMARY KEY, profile TEXT NOT NULL, orphaned INTEGER NOT NULL DEFAULT 0, handle INTEGER, archived_at INTEGER, group_name TEXT);
CREATE UNIQUE INDEX idx_lineages_handle ON lineages(handle);
CREATE TABLE comments (id INTEGER PRIMARY KEY AUTOINCREMENT, lineage_id TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_comments_lineage ON comments(lineage_id);
CREATE TABLE tags (lineage_id TEXT NOT NULL, tag TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (lineage_id, tag));
CREATE INDEX idx_tags_tag ON tags(tag);
` + indexDDL + `
INSERT INTO lineages (id, profile, orphaned, handle, archived_at, group_name) VALUES ('lin_1', 'p', 0, 5, NULL, 'work');
INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('lin_1', 'keep me', 1, 1);
INSERT INTO tags (lineage_id, tag, created_at) VALUES ('lin_1', 'urgent', 1);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, last_activity_at) VALUES ('s1', 'claude', 's1', 'lin_1', 'completed', 12345);
INSERT INTO prompt_fts (session_id, kind, text) VALUES ('s1', 'prompt', 'do the thing');
INSERT INTO cursors (source, source_id, kind, updated_at) VALUES ('claude', 's1', 'transcript_offset', 999);
`
	if err := r.Exec(v9DDL); err != nil {
		t.Fatalf("seeding v9 database: %v", err)
	}

	v, err := Open(r)
	if err != nil {
		t.Fatalf("Open (migrating v9 -> current): %v", err)
	}
	if v != CurrentVersion {
		t.Fatalf("got version %d, want %d", v, CurrentVersion)
	}

	var lineageCols []struct {
		Name string `json:"name"`
	}
	if err := r.Query(`PRAGMA table_info(lineages);`, &lineageCols); err != nil {
		t.Fatal(err)
	}
	if !hasColumn(lineageCols, "custom_name") {
		t.Errorf("expected lineages.custom_name to exist after migration, columns: %+v", lineageCols)
	}

	// The whole point of making v10 annotation-only: the seeded index rows
	// (sessions, prompt_fts, cursors) must survive a v9 -> v10 migration
	// untouched, since nothing about this migration needs them rebuilt.
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM sessions;`, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "s1" {
		t.Errorf("expected the seeded session to survive an annotation-only migration, got %+v", sessions)
	}

	var prompts []struct {
		Text string `json:"text"`
	}
	if err := r.Query(`SELECT text FROM prompt_fts WHERE session_id = 's1';`, &prompts); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0].Text != "do the thing" {
		t.Errorf("expected the seeded prompt to survive an annotation-only migration, got %+v", prompts)
	}

	var cursors []struct {
		SourceID string `json:"source_id"`
	}
	if err := r.Query(`SELECT source_id FROM cursors;`, &cursors); err != nil {
		t.Fatal(err)
	}
	if len(cursors) != 1 || cursors[0].SourceID != "s1" {
		t.Errorf("expected the seeded cursor to survive an annotation-only migration, got %+v", cursors)
	}

	// Every annotation seeded at v9 must be intact, and custom_name on the
	// pre-existing lineage must default to NULL rather than being
	// backfilled to anything.
	var lineage []struct {
		Handle     int     `json:"handle"`
		GroupName  *string `json:"group_name"`
		CustomName *string `json:"custom_name"`
	}
	if err := r.Query(`SELECT handle, group_name, custom_name FROM lineages WHERE id = 'lin_1';`, &lineage); err != nil {
		t.Fatal(err)
	}
	if len(lineage) != 1 || lineage[0].Handle != 5 || lineage[0].GroupName == nil || *lineage[0].GroupName != "work" || lineage[0].CustomName != nil {
		t.Errorf("expected the lineage (handle, group_name, custom_name) to survive migration, got %+v", lineage)
	}

	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.Query(`SELECT body FROM comments;`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Errorf("expected the comment to survive migration, got %+v", comments)
	}

	var tags []struct {
		Tag string `json:"tag"`
	}
	if err := r.Query(`SELECT tag FROM tags;`, &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Tag != "urgent" {
		t.Errorf("expected the tag to survive migration, got %+v", tags)
	}
}

// TestCrossingANonAnnotationOnlyVersionStillRebuildsTheIndex covers the
// other half of the annotationOnly rule: it is "skip the rebuild only when
// EVERY version being crossed is annotation-only", not "skip it if any one
// of them is". Simulated here with the test hook every other version-bump
// test in this file uses (temporarily raising CurrentVersion) to stand in
// for a hypothetical next version that isn't marked annotationOnly: crossing
// it alongside v10 must still discard and rebuild the index.
func TestCrossingANonAnnotationOnlyVersionStillRebuildsTheIndex(t *testing.T) {
	r := testRunner(t)

	v9DDL := `
CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO schema_meta (key, value) VALUES ('schema_version', '9');
CREATE TABLE lineages (id TEXT PRIMARY KEY, profile TEXT NOT NULL, orphaned INTEGER NOT NULL DEFAULT 0, handle INTEGER, archived_at INTEGER, group_name TEXT);
CREATE UNIQUE INDEX idx_lineages_handle ON lineages(handle);
CREATE TABLE comments (id INTEGER PRIMARY KEY AUTOINCREMENT, lineage_id TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_comments_lineage ON comments(lineage_id);
CREATE TABLE tags (lineage_id TEXT NOT NULL, tag TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (lineage_id, tag));
CREATE INDEX idx_tags_tag ON tags(tag);
` + indexDDL + `
INSERT INTO lineages (id, profile, orphaned, handle) VALUES ('lin_1', 'p', 0, 1);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state) VALUES ('s1', 'claude', 's1', 'lin_1', 'completed');
`
	if err := r.Exec(v9DDL); err != nil {
		t.Fatalf("seeding v9 database: %v", err)
	}

	// CurrentVersion is 10 in this build (annotationOnly); bumping it one
	// further simulates crossing a hypothetical v11 that is NOT
	// annotation-only - annotationOnly has no entry for it, so it defaults
	// to "needs a rebuild" exactly like every pre-v10 version did.
	orig := CurrentVersion
	CurrentVersion = orig + 1
	defer func() { CurrentVersion = orig }()

	if _, err := Open(r); err != nil {
		t.Fatalf("Open (migrating v9 -> v%d): %v", CurrentVersion, err)
	}

	var sessions []struct {
		ID string `json:"id"`
	}
	if err := r.Query(`SELECT id FROM sessions;`, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected the index to be rebuilt when the jump also crosses a non-annotation-only version, got %d rows", len(sessions))
	}
}

func hasColumn(cols []struct {
	Name string `json:"name"`
}, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}
