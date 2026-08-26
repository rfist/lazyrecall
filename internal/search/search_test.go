package search

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"recall/internal/schema"
	"recall/internal/sqlitex"
)

// All fixtures below are synthetic, hand-written data - no real session
// content.

func testDB(t *testing.T) *sqlitex.Runner {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	r := &sqlitex.Runner{BinPath: bin, DBPath: filepath.Join(dir, "recall.db"), TmpDir: dir}
	if _, err := schema.Open(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func seedSession(t *testing.T, db *sqlitex.Runner, rec map[string]any) {
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

func seedPrompt(t *testing.T, db *sqlitex.Runner, sessionID, text string) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("prompt_fts", []string{"session_id", "kind", "text"}, []map[string]any{
		{"session_id": sessionID, "kind": "prompt", "text": text},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestListOrdersByLastActivity(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "pi:p:1", "source": "pi", "source_session_id": "1", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	if items[0].SessionID != "pi:p:1" {
		t.Errorf("expected most recent first, got %+v", items)
	}
}

func TestListOmitsAbsentPreviewFields(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "unknown", "resumable": 1, "last_activity_at": 100,
		// deliberately no topic
	})
	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Topic != nil {
		t.Errorf("expected absent topic to stay nil, got %v", *items[0].Topic)
	}
	if items[0].CWD != nil {
		t.Errorf("expected absent cwd to stay nil, got %v", *items[0].CWD)
	}
}

func TestFilterByAgentAndTag(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "pi:p:1", "source": "pi", "source_session_id": "1", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})
	b := db.NewBatch()
	if err := b.BulkUpsert("lineages", []string{"id"}, []string{"id", "profile", "orphaned"}, []map[string]any{
		{"id": "lin1", "profile": "p", "orphaned": 0},
		{"id": "lin2", "profile": "p", "orphaned": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("tags", []string{"lineage_id", "tag", "created_at"}, []map[string]any{
		{"lineage_id": "lin1", "tag": "urgent", "created_at": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	items, err := List(db, Filter{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SessionID != "claude:p:1" {
		t.Fatalf("agent filter: got %+v", items)
	}

	tagged, err := List(db, Filter{Tag: "urgent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 1 || tagged[0].SessionID != "claude:p:1" {
		t.Fatalf("tag filter: got %+v", tagged)
	}

	combined, err := List(db, Filter{Agent: "pi", Tag: "urgent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != 0 {
		t.Fatalf("combined filter matching nothing should return empty, got %+v", combined)
	}
}

// TestMissingDirectoryStillListedAndSearchable covers spec session-search,
// "Working directory has been deleted": dir_exists=false must not exclude
// a session from List or from Search.
func TestMissingDirectoryStillListedAndSearchable(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
		"cwd": "/deleted/dir", "dir_exists": 0,
	})
	seedPrompt(t, db, "claude:p:1", "synthetic prompt in a now-missing directory")

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected the session to still be listed, got %+v", items)
	}
	if items[0].DirExists == nil || *items[0].DirExists {
		t.Fatalf("expected DirExists=false to be reported, got %+v", items[0].DirExists)
	}

	hits, err := Search(db, "now-missing directory", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the session to still be searchable, got %+v", hits)
	}
}

func TestFilterByTimeRange(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:2", "source": "claude", "source_session_id": "2", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 900,
	})
	since := time.Unix(500, 0)
	items, err := List(db, Filter{Since: &since})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SessionID != "claude:p:2" {
		t.Fatalf("got %+v", items)
	}
}

func TestSearchOnlyMatchesPrompts(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedPrompt(t, db, "claude:p:1", "please fix the flaky retry test")

	hits, err := Search(db, "flaky retry", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].MatchSnippet == "" {
		t.Error("expected a match snippet")
	}

	none, err := Search(db, "something never typed", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no matches, got %+v", none)
	}
}

func TestSearchQueryIsLiteralNotFTS5Syntax(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	// A prompt containing characters that are FTS5 query syntax if parsed
	// (hyphen prefix = NOT, colon = column filter). Verifies the query text
	// is matched literally rather than interpreted.
	seedPrompt(t, db, "claude:p:1", "how do I use async-await here")

	hits, err := Search(db, "async-await", Filter{})
	if err != nil {
		t.Fatalf("search must not error on FTS5-special characters in the query: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1 (literal phrase match)", len(hits))
	}
}

func TestGroupByRepoNestsWorktrees(t *testing.T) {
	main := "/repo/main"
	wt := "/repo/wt-feature"
	items := []Item{
		{SessionID: "1", GitRepoRoot: &main, GitCommonRoot: &main},
		{SessionID: "2", GitRepoRoot: &wt, GitCommonRoot: &main},
	}
	groups, unplaced := GroupByRepo(items)
	if len(unplaced) != 0 {
		t.Fatalf("expected nothing unplaced, got %+v", unplaced)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 repo group, got %d", len(groups))
	}
	g := groups[0]
	if len(g.Direct) != 1 || g.Direct[0].SessionID != "1" {
		t.Errorf("main worktree session should be Direct, got %+v", g.Direct)
	}
	if len(g.Worktrees["wt-feature"]) != 1 || g.Worktrees["wt-feature"][0].SessionID != "2" {
		t.Errorf("expected session 2 nested under worktree 'wt-feature', got %+v", g.Worktrees)
	}
}

func TestGroupByCWDWhenNotInGitRepo(t *testing.T) {
	dir := "/home/x/scratch"
	items := []Item{{SessionID: "1", CWD: &dir}}
	groups, unplaced := GroupByRepo(items)
	if len(unplaced) != 0 {
		t.Fatal("a session with a known cwd should never be unplaced")
	}
	if len(groups) != 1 || groups[0].IsRepo {
		t.Fatalf("expected a non-repo group keyed by cwd, got %+v", groups)
	}
	if groups[0].Key != dir {
		t.Errorf("got key %q, want %q", groups[0].Key, dir)
	}
}

func TestEmptyMessageNamesProfileAndScope(t *testing.T) {
	msg := EmptyMessage("claude-personal", Filter{}, "some phrase")
	if msg == "" {
		t.Fatal("expected a non-empty message")
	}
	for _, want := range []string{"claude-personal", "some phrase", "prompts and topics"} {
		if !contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// The following cover change add-readable-session-listing, spec
// session-search "Short session handle": resolving either form of a session
// identifier.

func seedLineageWithHandle(t *testing.T, db *sqlitex.Runner, id, profileName string, handle int) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "handle"}, []map[string]any{
		{"id": id, "profile": profileName, "orphaned": 0, "handle": handle},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestListIncludesHandleFromLineage(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 7)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Handle != 7 {
		t.Fatalf("expected handle 7 on the listed item, got %+v", items)
	}
}

func TestItemForIdentifierByHandle(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 3)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "p", "3")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "claude:p:1" {
		t.Fatalf("got item=%+v ok=%v", it, ok)
	}
}

func TestItemForIdentifierByFullyQualifiedID(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 3)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "p", "claude:p:1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.Handle != 3 {
		t.Fatalf("got item=%+v ok=%v", it, ok)
	}
}

func TestItemForIdentifierHandleDoesNotResolve(t *testing.T) {
	db := testDB(t)
	it, ok, err := ItemForIdentifier(db, "p", "999")
	if err != nil {
		t.Fatalf("a handle that resolves to nothing should not be an error: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for an unresolved handle, got %+v", it)
	}
}

func TestItemForIdentifierHandlePicksMostRecentSessionInLineage(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 3)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:2", "source": "claude", "source_session_id": "2", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	it, ok, err := ItemForIdentifier(db, "p", "3")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "claude:p:2" {
		t.Fatalf("expected the most recently active session in the lineage, got %+v ok=%v", it, ok)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
