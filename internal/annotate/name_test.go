package annotate

import (
	"testing"

	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// storedCustomName reads the raw custom_name value for a lineage, so a
// test can assert the exact stored value rather than only SetName's error
// return.
func storedCustomName(t *testing.T, db *sqlitex.Runner, lineageID string) *string {
	t.Helper()
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var rows []struct {
		CustomName *string `json:"custom_name"`
	}
	q := "SELECT custom_name FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one lineage row for %q, got %d", lineageID, len(rows))
	}
	return rows[0].CustomName
}

func TestSetNameStoresAndReads(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := SetName(db, "lin1", "the release checklist"); err != nil {
		t.Fatal(err)
	}
	got := storedCustomName(t, db, "lin1")
	if got == nil || *got != "the release checklist" {
		t.Errorf("custom_name = %v, want %q", got, "the release checklist")
	}
}

// TestSetNameEmptyClears covers the rename prompt's "submit blank to
// clear" behaviour: an empty value must remove a previously set name.
func TestSetNameEmptyClears(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	if err := SetName(db, "lin1", "retry loop"); err != nil {
		t.Fatal(err)
	}

	if err := SetName(db, "lin1", ""); err != nil {
		t.Fatal(err)
	}
	if got := storedCustomName(t, db, "lin1"); got != nil {
		t.Errorf("custom_name = %v, want nil after clearing", got)
	}
}

// TestSetNameWhitespaceOnlyClears covers the same "clear" behaviour for a
// name that is not literally empty but carries no visible content -
// whitespace typed and then submitted must not be stored as a name.
func TestSetNameWhitespaceOnlyClears(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	if err := SetName(db, "lin1", "retry loop"); err != nil {
		t.Fatal(err)
	}

	if err := SetName(db, "lin1", "   \t  "); err != nil {
		t.Fatal(err)
	}
	if got := storedCustomName(t, db, "lin1"); got != nil {
		t.Errorf("custom_name = %v, want nil after a whitespace-only name", got)
	}
}

// TestSetNameTrimsSurroundingWhitespace covers a name that has real
// content but stray leading/trailing whitespace - only the whitespace
// itself is discarded.
func TestSetNameTrimsSurroundingWhitespace(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := SetName(db, "lin1", "  retry loop  "); err != nil {
		t.Fatal(err)
	}
	got := storedCustomName(t, db, "lin1")
	if got == nil || *got != "retry loop" {
		t.Errorf("custom_name = %v, want %q", got, "retry loop")
	}
}

// TestSetNameOverwritesExisting covers renaming a session twice: the
// second call replaces the first rather than erroring or appending.
func TestSetNameOverwritesExisting(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	if err := SetName(db, "lin1", "first name"); err != nil {
		t.Fatal(err)
	}
	if err := SetName(db, "lin1", "second name"); err != nil {
		t.Fatal(err)
	}
	got := storedCustomName(t, db, "lin1")
	if got == nil || *got != "second name" {
		t.Errorf("custom_name = %v, want %q", got, "second name")
	}
}
