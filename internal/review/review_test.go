package review

import (
	"os/exec"
	"path/filepath"
	"testing"

	"lazyrecall/internal/schema"
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

	entries, err := Report(db)
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
	entries, err := Report(db)
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
	entries, err := Report(db)
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
