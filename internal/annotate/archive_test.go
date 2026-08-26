package annotate

import (
	"testing"

	"lazyrecall/internal/schema"
	"lazyrecall/internal/sqlitex"
)

// storedArchivedAt reads the raw archived_at value for a lineage, so a
// test can assert the exact timestamp rather than just whether it is set.
func storedArchivedAt(t *testing.T, db *sqlitex.Runner, lineageID string) *int64 {
	t.Helper()
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var rows []struct {
		ArchivedAt *int64 `json:"archived_at"`
	}
	q := "SELECT archived_at FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one lineage row for %q, got %d", lineageID, len(rows))
	}
	return rows[0].ArchivedAt
}

func TestArchiveRoundTrip(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if archived, err := IsArchived(db, "lin1"); err != nil || archived {
		t.Fatalf("expected fresh lineage unarchived, got archived=%v err=%v", archived, err)
	}

	if err := Archive(db, "lin1"); err != nil {
		t.Fatal(err)
	}
	if archived, err := IsArchived(db, "lin1"); err != nil || !archived {
		t.Fatalf("expected lineage archived, got archived=%v err=%v", archived, err)
	}

	if err := Unarchive(db, "lin1"); err != nil {
		t.Fatal(err)
	}
	if archived, err := IsArchived(db, "lin1"); err != nil || archived {
		t.Fatalf("expected lineage unarchived again, got archived=%v err=%v", archived, err)
	}
}

// TestArchiveTwiceKeepsTimestamp covers the idempotence requirement:
// archiving an already-archived lineage must not move the stored
// timestamp, so "when did I archive this" stays answerable.
func TestArchiveTwiceKeepsTimestamp(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := Archive(db, "lin1"); err != nil {
		t.Fatal(err)
	}
	first := storedArchivedAt(t, db, "lin1")
	if first == nil {
		t.Fatal("expected a timestamp after the first archive")
	}

	if err := Archive(db, "lin1"); err != nil {
		t.Fatal(err)
	}
	second := storedArchivedAt(t, db, "lin1")
	if second == nil || *first != *second {
		t.Fatalf("expected the second archive to keep the original timestamp: first=%v second=%v", *first, *second)
	}
}

func TestAllArchivedReturnsOnlyArchived(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	seedLineageAndSession(t, db, "lin2", "claude:p:s2")

	if err := Archive(db, "lin1"); err != nil {
		t.Fatal(err)
	}

	ids, err := AllArchived(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "lin1" {
		t.Fatalf("expected only lin1 to be listed as archived, got %v", ids)
	}
}

// TestArchiveSurvivesIndexRebuild is the test that matters most for this
// change: the whole reason archive state lives on the durable lineages
// table rather than the disposable sessions table is that a full index
// rebuild (a schema-version bump, as `refresh --full` triggers) must not
// destroy the user's decision. It simulates that rebuild by bumping
// schema.CurrentVersion and reopening, exactly as the schema tests do.
func TestArchiveSurvivesIndexRebuild(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := Archive(db, "lin1"); err != nil {
		t.Fatal(err)
	}

	orig := schema.CurrentVersion
	schema.CurrentVersion = orig + 1
	defer func() { schema.CurrentVersion = orig }()
	if _, err := schema.Open(db); err != nil {
		t.Fatalf("Open after version bump: %v", err)
	}

	// Assert the rebuild actually happened before concluding anything from
	// what survived it. Without this the test passes just as happily if the
	// version bump turned out to be a no-op, which would make it a guard
	// against nothing.
	var remaining []struct {
		N int `json:"n"`
	}
	if err := db.Query("SELECT COUNT(*) AS n FROM sessions;", &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].N != 0 {
		t.Fatalf("the index was not rebuilt (%d session rows left), so this test proves nothing", remaining[0].N)
	}

	archived, err := IsArchived(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if !archived {
		t.Fatal("expected the session to still be archived after a full index rebuild")
	}
}
