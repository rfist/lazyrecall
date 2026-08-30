package annotate

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/schema"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

func testDB(t *testing.T) *sqlitex.Runner {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	r := &sqlitex.Runner{BinPath: bin, DBPath: filepath.Join(dir, "lazyrecall.db"), TmpDir: dir}
	if _, err := schema.Open(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func seedLineageAndSession(t *testing.T, db *sqlitex.Runner, lineageID, sessionID string) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned"}, []map[string]any{
		{"id": lineageID, "profile": "p", "orphaned": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable"}, []map[string]any{
		{"id": sessionID, "source": "claude", "source_session_id": "s1", "lineage_id": lineageID, "end_state": "completed", "resumable": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestCommentLifecycle(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := AddComment(db, "lin1", "worth revisiting"); err != nil {
		t.Fatal(err)
	}
	comments, err := CommentsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "worth revisiting" {
		t.Fatalf("got %+v", comments)
	}

	if err := EditComment(db, comments[0].ID, "actually done"); err != nil {
		t.Fatal(err)
	}
	comments2, err := CommentsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if comments2[0].Body != "actually done" {
		t.Fatalf("edit did not apply: %+v", comments2)
	}

	if err := RemoveComment(db, comments[0].ID); err != nil {
		t.Fatal(err)
	}
	comments3, err := CommentsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments3) != 0 {
		t.Fatalf("expected comment removed, got %+v", comments3)
	}
}

func TestCommentAdversarialContentNeverInterpolated(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	body := `'; DROP TABLE comments; --" unicode 日本語`
	if err := AddComment(db, "lin1", body); err != nil {
		t.Fatal(err)
	}
	comments, err := CommentsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != body {
		t.Fatalf("got %+v, want body preserved exactly", comments)
	}
}

func TestTagLifecycleAndFiltering(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	seedLineageAndSession(t, db, "lin2", "claude:p:s2")

	if err := AddTag(db, "lin1", "billing"); err != nil {
		t.Fatal(err)
	}
	if err := AddTag(db, "lin2", "billing"); err != nil {
		t.Fatal(err)
	}
	if err := AddTag(db, "lin1", "billing"); err != nil { // duplicate: must be a no-op, not an error
		t.Fatal(err)
	}

	all, err := AllTags(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0] != "billing" {
		t.Fatalf("got %+v", all)
	}

	if err := RemoveTag(db, "lin1", "billing"); err != nil {
		t.Fatal(err)
	}
	lin1Tags, err := TagsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(lin1Tags) != 0 {
		t.Fatalf("expected lin1's tag removed, got %+v", lin1Tags)
	}
	lin2Tags, err := TagsForLineage(db, "lin2")
	if err != nil {
		t.Fatal(err)
	}
	if len(lin2Tags) != 1 || lin2Tags[0] != "billing" {
		t.Fatalf("expected lin2 unaffected by lin1's tag removal, got %+v", lin2Tags)
	}
}

func TestLineageForSession(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	id, err := LineageForSession(db, "claude:p:s1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "lin1" {
		t.Fatalf("got %q", id)
	}
	if _, err := LineageForSession(db, "no-such-session"); err == nil {
		t.Fatal("expected an error for an unknown session id")
	}
}

// The following cover change add-readable-session-listing, spec
// session-annotations "Annotation commands accept the short handle".

func seedLineageWithHandle(t *testing.T, db *sqlitex.Runner, lineageID, sessionID, profileName string, handle int) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "handle"}, []map[string]any{
		{"id": lineageID, "profile": profileName, "orphaned": 0, "handle": handle},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable"}, []map[string]any{
		{"id": sessionID, "source": "claude", "source_session_id": "s1", "lineage_id": lineageID, "end_state": "completed", "resumable": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestLineageForIdentifierByHandle(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "claude:p:s1", "p", 5)

	id, err := LineageForIdentifier(db, "p", "5")
	if err != nil {
		t.Fatal(err)
	}
	if id != "lin1" {
		t.Fatalf("got %q", id)
	}
}

func TestLineageForIdentifierFullyQualifiedStillWorks(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "claude:p:s1", "p", 5)

	id, err := LineageForIdentifier(db, "p", "claude:p:s1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "lin1" {
		t.Fatalf("got %q", id)
	}
}

func TestLineageForIdentifierHandleDoesNotResolve(t *testing.T) {
	db := testDB(t)
	if _, err := LineageForIdentifier(db, "p", "999"); err == nil {
		t.Fatal("expected an error for a handle that resolves to no session")
	}
}

// TestTagAndCommentByHandle covers the spec scenarios "Tagging by handle"
// and "Commenting by handle" end to end through the public annotate API.
func TestTagAndCommentByHandle(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "claude:p:s1", "p", 9)

	lineage, err := LineageForIdentifier(db, "p", "9")
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTag(db, lineage, "billing"); err != nil {
		t.Fatal(err)
	}
	tags, err := TagsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != "billing" {
		t.Fatalf("got %+v", tags)
	}

	if err := AddComment(db, lineage, "attached by handle"); err != nil {
		t.Fatal(err)
	}
	comments, err := CommentsForLineage(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "attached by handle" {
		t.Fatalf("got %+v", comments)
	}
}
