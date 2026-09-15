package search

import (
	"fmt"
	"testing"

	"github.com/rfist/lazyrecall/internal/config"
)

func TestValidateGroupAcceptsReservedAndConfiguredNames(t *testing.T) {
	cfg := config.Config{Groups: []config.Group{{Name: "work"}, {Name: "personal"}}}
	for _, ok := range []string{"", "archive", "unknown", "work", "personal"} {
		if err := ValidateGroup(cfg, ok); err != nil {
			t.Errorf("ValidateGroup(%q) = %v, want nil", ok, err)
		}
	}
}

func TestValidateGroupRejectsUnknownNameAndListsValidOnes(t *testing.T) {
	cfg := config.Config{Groups: []config.Group{{Name: "work"}}}
	err := ValidateGroup(cfg, "bogus")
	if err == nil {
		t.Fatal("expected an error for an unconfigured group name")
	}
	for _, want := range []string{"all", "archive", "unknown", "work", "bogus"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestCountsSplitsByGroupArchiveAndUnknown covers the `groups` command's
// data source: one query that classifies every non-archived session into
// its configured group or Unknown, and counts archived sessions separately
// regardless of which group they are filed under.
func TestCountsSplitsByGroupArchiveAndUnknown(t *testing.T) {
	db := testDB(t)
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "group_name", "archived_at"}, []map[string]any{
		{"id": "lin-work", "profile": "p", "orphaned": 0, "group_name": nil, "archived_at": nil},
		{"id": "lin-archived-work", "profile": "p", "orphaned": 0, "group_name": "work", "archived_at": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	seedSession(t, db, map[string]any{
		"id": "claude:p:work", "source": "claude", "source_session_id": "work", "lineage_id": "lin-work",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:unknown", "source": "claude", "source_session_id": "unknown", "lineage_id": "lin-unknown",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200, "cwd": "/tmp",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:archived", "source": "claude", "source_session_id": "archived", "lineage_id": "lin-archived-work",
		"end_state": "completed", "resumable": 1, "last_activity_at": 300,
	})

	groups := []config.Group{{Name: "work", Paths: []string{"/home/me/code"}}}
	counts, err := Counts(db, config.Hide{}, groups)
	if err != nil {
		t.Fatal(err)
	}
	if counts.ByGroup["work"] != 1 {
		t.Errorf("ByGroup[work] = %d, want 1", counts.ByGroup["work"])
	}
	if counts.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", counts.Unknown)
	}
	if counts.Archive != 1 {
		t.Errorf("Archive = %d, want 1 (filed under work, but archived sessions count separately)", counts.Archive)
	}
}

// TestPathGroupAgreesWithSQLGroupExpr runs the Go-side matcher (PathGroup,
// which the browser's `p` popup uses to show what clearing a manual
// override would produce) and the SQL expression every query actually
// classifies rows with (effectiveGroupExpr, via List) over the same
// fixtures, so the two can never quietly diverge on longest-prefix,
// tie-break order, or an empty/absent cwd.
func TestPathGroupAgreesWithSQLGroupExpr(t *testing.T) {
	db := testDB(t)
	groups := []config.Group{
		{Name: "work", Paths: []string{"/home/me/code", "/home/me/work"}},
		{Name: "personal", Paths: []string{"/home/me/dotfiles"}},
		{Name: "nested", Paths: []string{"/home/me/code/deep"}},
	}
	cwds := []string{
		"/home/me/code", "/home/me/code/deep", "/home/me/code/deep/er",
		"/home/me/work/x", "/home/me/dotfiles", "/home/me", "/tmp", "",
	}
	for i, cwd := range cwds {
		rec := map[string]any{
			"id": fmt.Sprintf("claude:p:pg%d", i), "source": "claude", "source_session_id": fmt.Sprintf("pg%d", i),
			"lineage_id": fmt.Sprintf("lin-pg%d", i), "end_state": "completed", "resumable": 1, "last_activity_at": int64(1000 + i),
		}
		if cwd != "" {
			rec["cwd"] = cwd
		}
		seedSession(t, db, rec)
	}
	items, err := List(db, Filter{Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Item{}
	for _, it := range items {
		byID[it.SessionID] = it
	}
	for i, cwd := range cwds {
		it, ok := byID[fmt.Sprintf("claude:p:pg%d", i)]
		if !ok {
			t.Fatalf("missing item for cwd %q", cwd)
		}
		if want := PathGroup(groups, cwd); it.Group != want {
			t.Errorf("cwd %q: SQL gave %q, PathGroup gave %q", cwd, it.Group, want)
		}
	}
}
