package annotate

import (
	"testing"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// storedGroup reads the raw group_name and archived_at values for a
// lineage, so tests can assert the exact combination SetGroup leaves behind
// rather than just one field in isolation.
func storedGroup(t *testing.T, db *sqlitex.Runner, lineageID string) (groupName *string, archivedAt *int64) {
	t.Helper()
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var rows []struct {
		GroupName  *string `json:"group_name"`
		ArchivedAt *int64  `json:"archived_at"`
	}
	q := "SELECT group_name, archived_at FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one lineage row for %q, got %d", lineageID, len(rows))
	}
	return rows[0].GroupName, rows[0].ArchivedAt
}

var testGroupsConfig = config.Config{Groups: []config.Group{
	{Name: "work", Paths: []string{"/code"}},
	{Name: "personal", Paths: []string{"/home"}},
}}

func TestSetGroupToConfiguredNameClearsArchive(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	if err := Archive(db, "lin1"); err != nil {
		t.Fatal(err)
	}

	if err := SetGroup(db, testGroupsConfig, "lin1", "work"); err != nil {
		t.Fatal(err)
	}

	name, archivedAt := storedGroup(t, db, "lin1")
	if name == nil || *name != "work" {
		t.Errorf("group_name = %v, want work", name)
	}
	if archivedAt != nil {
		t.Errorf("archived_at = %v, want nil (setting a group clears an earlier archive)", archivedAt)
	}
}

func TestSetGroupToArchiveKeepsExistingGroupName(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	if err := SetGroup(db, testGroupsConfig, "lin1", "personal"); err != nil {
		t.Fatal(err)
	}

	if err := SetGroup(db, testGroupsConfig, "lin1", "archive"); err != nil {
		t.Fatal(err)
	}

	name, archivedAt := storedGroup(t, db, "lin1")
	if name == nil || *name != "personal" {
		t.Errorf("group_name = %v, want personal (archiving must not clear it)", name)
	}
	if archivedAt == nil {
		t.Error("archived_at = nil, want set")
	}
}

func TestSetGroupToAutomaticClearsBoth(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")
	if err := SetGroup(db, testGroupsConfig, "lin1", "work"); err != nil {
		t.Fatal(err)
	}
	if err := SetGroup(db, testGroupsConfig, "lin1", "archive"); err != nil {
		t.Fatal(err)
	}

	if err := SetGroup(db, testGroupsConfig, "lin1", ""); err != nil {
		t.Fatal(err)
	}

	name, archivedAt := storedGroup(t, db, "lin1")
	if name != nil {
		t.Errorf("group_name = %v, want nil", name)
	}
	if archivedAt != nil {
		t.Errorf("archived_at = %v, want nil", archivedAt)
	}
}

func TestSetGroupRejectsUnconfiguredName(t *testing.T) {
	db := testDB(t)
	seedLineageAndSession(t, db, "lin1", "claude:p:s1")

	err := SetGroup(db, testGroupsConfig, "lin1", "nonexistent")
	if err == nil {
		t.Fatal("expected an error for a group not in the config")
	}
}
