package review

import (
	"os/exec"
	"path/filepath"
	"testing"

	"lazyrecall/internal/config"
	"lazyrecall/internal/schema"
	"lazyrecall/internal/search"
	"lazyrecall/internal/sqlitex"
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

func seed(t *testing.T, db *sqlitex.Runner, rec map[string]any) {
	t.Helper()
	b := db.NewBatch()
	cols := make([]string, 0, len(rec))
	for k := range rec {
		cols = append(cols, k)
	}
	if err := b.BulkInsert("sessions", cols, []map[string]any{rec}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestReportExcludesCompleted(t *testing.T) {
	db := testDB(t)
	seed(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 300,
	})
	seed(t, db, map[string]any{
		"id": "claude:p:2", "source": "claude", "source_session_id": "2", "lineage_id": "lin2",
		"end_state": "dangling", "resumable": 1, "last_activity_at": 200,
	})
	seed(t, db, map[string]any{
		"id": "pi:p:1", "source": "pi", "source_session_id": "1", "lineage_id": "lin3",
		"end_state": "interrupted", "resumable": 1, "last_activity_at": 400,
	})

	entries, err := Report(db, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (completed excluded)", len(entries))
	}
	if entries[0].SessionID != "pi:p:1" {
		t.Errorf("expected most recent first, got %+v", entries)
	}
	for _, e := range entries {
		if e.EndState == "completed" {
			t.Errorf("completed session leaked into report: %+v", e)
		}
	}
}

// TestReportEntryIdentifiesAgentDirectoryAndActivity covers spec
// session-review, "Report identifies where to resume": each entry must
// carry its agent, working directory, and last activity.
func TestReportEntryIdentifiesAgentDirectoryAndActivity(t *testing.T) {
	db := testDB(t)
	seed(t, db, map[string]any{
		"id": "pi:p:1", "source": "pi", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "dangling", "resumable": 1, "last_activity_at": 100, "cwd": "/work/repo",
	})
	entries, err := Report(db, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	e := entries[0]
	if e.Source != "pi" {
		t.Errorf("missing agent: %+v", e)
	}
	if e.CWD == nil || *e.CWD != "/work/repo" {
		t.Errorf("missing working directory: %+v", e)
	}
	if e.LastActivityAt == nil {
		t.Errorf("missing last activity: %+v", e)
	}
}

func TestReportEmptyWhenNothingUnfinished(t *testing.T) {
	db := testDB(t)
	seed(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	entries, err := Report(db, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
	msg := EmptyMessage("claude-personal")
	if msg == "" {
		t.Fatal("expected a non-empty explanatory message")
	}
}

// TestReportRespectsHideRules covers change apply-config-hide-rules: the
// report is a listing and the hide rules apply to it exactly as they do to
// any other listing - an automated or archived dangling session must not
// appear, while ReportWithHidden reports how many such sessions were
// suppressed.
func TestReportRespectsHideRules(t *testing.T) {
	db := testDB(t)
	seed(t, db, map[string]any{
		"id": "claude:p:auto", "source": "claude", "source_session_id": "auto", "lineage_id": "lin-auto",
		"end_state": "dangling", "resumable": 1, "last_activity_at": 100, "origin": "automated",
	})
	seed(t, db, map[string]any{
		"id": "claude:p:inter", "source": "claude", "source_session_id": "inter", "lineage_id": "lin-inter",
		"end_state": "dangling", "resumable": 1, "last_activity_at": 200, "origin": "interactive",
	})
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "archived_at"}, []map[string]any{
		{"id": "lin-archived", "profile": "p", "orphaned": 0, "archived_at": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	seed(t, db, map[string]any{
		"id": "claude:p:archived", "source": "claude", "source_session_id": "archived", "lineage_id": "lin-archived",
		"end_state": "dangling", "resumable": 1, "last_activity_at": 300, "origin": "interactive",
	})

	entries, hidden, err := ReportWithHidden(db, search.Filter{Hide: config.Hide{NonInteractive: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != "claude:p:inter" {
		t.Fatalf("got entries %+v, want only the interactive dangling session", entries)
	}
	// Three sessions would have matched the report with no hide rules; one
	// is shown, so two were suppressed (automated + archived).
	if hidden != 2 {
		t.Fatalf("hidden = %d, want 2", hidden)
	}

	all, hiddenAll, err := ReportWithHidden(db, search.Filter{Hide: config.Hide{NonInteractive: true}, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if hiddenAll != 0 || len(all) != 3 {
		t.Fatalf("with --all: entries=%d hidden=%d, want 3 and 0", len(all), hiddenAll)
	}
}
