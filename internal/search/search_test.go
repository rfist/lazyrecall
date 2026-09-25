package search

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rfist/lazyrecall/internal/annotate"
	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/schema"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
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
	r := &sqlitex.Runner{BinPath: bin, DBPath: filepath.Join(dir, "lazyrecall.db"), TmpDir: dir}
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

// The following cover the switch from one literal FTS5 phrase to per-word
// ANDed prefix terms (sqlitex.FTS5PrefixTerms), fixing the reported bug
// that searching "sketch" found nothing even though a prompt contained
// "sketchybar".

func TestSearchPrefixMatchesShortWordAgainstLongerToken(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedPrompt(t, db, "claude:p:1", "let's install sketchybar for the status bar")

	hits, err := Search(db, "sketch", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1 (prefix match of \"sketch\" against \"sketchybar\")", len(hits))
	}
}

func TestSearchMultiWordMatchesAnyOrderAndRequiresAllWords(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedPrompt(t, db, "claude:p:1", "please look at the retries before fixing the timeout")
	seedSession(t, db, map[string]any{
		"id": "claude:p:2", "source": "claude", "source_session_id": "2", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})
	seedPrompt(t, db, "claude:p:2", "please just fix the timeout")

	// The query words are in the opposite order from how they appear in the
	// matching prompt, and session 2 has only one of the two words.
	hits, err := Search(db, "fix retry", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SessionID != "claude:p:1" {
		t.Fatalf("got %+v, want only session 1 (both words present, in either order)", hits)
	}
}

func TestSearchWordMatchingNothingExcludesWholeQuery(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedPrompt(t, db, "claude:p:1", "please fix the flaky retry test")

	hits, err := Search(db, "fix nonexistentword", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits when one word of the query matches nothing, got %+v", hits)
	}
}

// TestSearchCountAgreesWithHitsUnderPrefixMatching covers that
// countSearchHits (via SearchWithHidden) and Search never disagree now that
// both build their MATCH query the same way, through FTS5PrefixTerms.
func TestSearchCountAgreesWithHitsUnderPrefixMatching(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedPrompt(t, db, "claude:p:1", "install sketchybar please")
	seedSession(t, db, map[string]any{
		"id": "claude:p:2", "source": "claude", "source_session_id": "2", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})
	seedPrompt(t, db, "claude:p:2", "sketching out a design doc")

	hits, hidden, err := SearchWithHidden(db, "sketch", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 0 {
		t.Fatalf("hidden = %d, want 0 (no hide rules configured)", hidden)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 - count and hits must agree", len(hits))
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

// TestEmptyMessageNamesGroupWhenSelected covers change
// group-sessions-in-one-index's fix to a stale message: EmptyMessage used
// to say `No session in profile "all"...` regardless of what was actually
// selected, a leftover from before groups replaced the single active
// profile. With a group selected, the message names it directly from f.Group
// - not a caller-supplied placeholder, which is what let one call site
// (cmd/lazyrecall's cmdResume) pass the literal "all" without regard to its
// own filter.
func TestEmptyMessageNamesGroupWhenSelected(t *testing.T) {
	msg := EmptyMessage(Filter{Group: "work"}, "some phrase")
	if msg == "" {
		t.Fatal("expected a non-empty message")
	}
	for _, want := range []string{`group "work"`, "some phrase", "prompts and topics"} {
		if !contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// TestEmptyMessageSaysNothingAboutScopeForAll covers the other half of the
// same fix: with no group selected, the message must not name a scope at
// all - not "all", not "profile" - since All is the ordinary, unscoped
// view.
func TestEmptyMessageSaysNothingAboutScopeForAll(t *testing.T) {
	msg := EmptyMessage(Filter{}, "")
	for _, unwanted := range []string{"group", "profile", `"all"`} {
		if contains(msg, unwanted) {
			t.Errorf("message %q should not mention a scope for All (found %q)", msg, unwanted)
		}
	}
	if !contains(msg, "the active filters") {
		t.Errorf("message %q should describe the no-query scope", msg)
	}
}

// TestEmptyMessageListsOtherFilters covers "Keep any other filters the
// message lists": the group rewrite must not drop the existing
// agent/install/client/repo/tag/time-range clauses.
func TestEmptyMessageListsOtherFilters(t *testing.T) {
	msg := EmptyMessage(Filter{Agent: "claude", Repo: "/x", Tag: "wip"}, "")
	for _, want := range []string{"agent=claude", "repo=/x", "tag=wip"} {
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

// TestListIncludesCustomNameFromLineage covers the lazyrecall-assigned name
// (lineages.custom_name, distinct from sessions.name which a source tool
// records): a lineage with a custom name set must have it come back on the
// Item every query path over itemColumns returns.
func TestListIncludesCustomNameFromLineage(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 1)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	if err := annotate.SetName(db, "lin1", "the release checklist"); err != nil {
		t.Fatal(err)
	}

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].CustomName == nil || *items[0].CustomName != "the release checklist" {
		t.Fatalf("expected the custom name on the listed item, got %+v", items)
	}

	it, ok, err := ItemForIdentifier(db, "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.CustomName == nil || *it.CustomName != "the release checklist" {
		t.Fatalf("expected the custom name from ItemForIdentifier, got item=%+v ok=%v", it, ok)
	}
}

// TestListWithNoCustomNameReadsNil covers the default: a lineage with no
// custom name ever set must read back nil, not an empty string standing in
// for "none".
func TestListWithNoCustomNameReadsNil(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 1)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].CustomName != nil {
		t.Fatalf("expected a nil custom name with none ever set, got %+v", items)
	}
}

// TestListIncludesCommentCount covers the row marker's data source: a
// lineage's comment count is a correlated COUNT(*), computed at query time
// over every query path that selects itemColumns, and it is 0 - not
// missing - for a session with no comments.
func TestListIncludesCommentCount(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 1)
	seedLineageWithHandle(t, db, "lin2", "p", 2)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:2", "source": "claude", "source_session_id": "2", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	if err := annotate.AddComment(db, "lin1", "first"); err != nil {
		t.Fatal(err)
	}
	if err := annotate.AddComment(db, "lin1", "second"); err != nil {
		t.Fatal(err)
	}

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	byID := map[string]Item{}
	for _, it := range items {
		byID[it.SessionID] = it
	}
	if got := byID["claude:p:1"].CommentCount; got != 2 {
		t.Errorf("comment count for lin1 = %d, want 2", got)
	}
	if got := byID["claude:p:2"].CommentCount; got != 0 {
		t.Errorf("comment count for lin2 = %d, want 0", got)
	}
}

func TestItemForIdentifierByHandle(t *testing.T) {
	db := testDB(t)
	seedLineageWithHandle(t, db, "lin1", "p", 3)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "3", nil)
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

	it, ok, err := ItemForIdentifier(db, "claude:p:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.Handle != 3 {
		t.Fatalf("got item=%+v ok=%v", it, ok)
	}
}

func TestItemForIdentifierHandleDoesNotResolve(t *testing.T) {
	db := testDB(t)
	it, ok, err := ItemForIdentifier(db, "999", nil)
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

	it, ok, err := ItemForIdentifier(db, "3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "claude:p:2" {
		t.Fatalf("expected the most recently active session in the lineage, got %+v ok=%v", it, ok)
	}
}

// The following cover task "resume shortcuts and unique id prefixes":
// ItemForIdentifier trying an arg that is neither a handle nor an exact id
// match as a unique prefix of a session's composite id or its native
// SourceSessionID.

func TestItemForIdentifierPrefixOfNativeSessionID(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:9f2a1b3c-uuid", "source": "claude", "source_session_id": "9f2a1b3c-uuid", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "9f2a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "claude:p:9f2a1b3c-uuid" {
		t.Fatalf("got item=%+v ok=%v, want the session whose native id starts with the prefix", it, ok)
	}
}

func TestItemForIdentifierPrefixOfCompositeID(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:cc:abc123", "source": "claude", "source_session_id": "abc123", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	// "claude:c" is a prefix of the composite id but not of the native
	// SourceSessionID ("abc123") - proving the composite-id column alone is
	// enough to resolve it.
	it, ok, err := ItemForIdentifier(db, "claude:c", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "claude:cc:abc123" {
		t.Fatalf("got item=%+v ok=%v, want the session whose composite id starts with the prefix", it, ok)
	}
}

// TestItemForIdentifierExactIDWinsOverAmbiguousPrefix covers that an exact
// composite-id match is tried, and returned, before prefix matching ever
// runs - so a session whose id happens to be a literal prefix of a
// different, longer session id (e.g. "s:p:abc" is a prefix of "s:p:abcxyz")
// still resolves to itself exactly, not to an AmbiguousError.
func TestItemForIdentifierExactIDWinsOverAmbiguousPrefix(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "s:p:abc", "source": "s", "source_session_id": "abc", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "s:p:abcxyz", "source": "s", "source_session_id": "abcxyz", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	it, ok, err := ItemForIdentifier(db, "s:p:abc", nil)
	if err != nil {
		t.Fatalf("expected the exact match to win with no error, got %v", err)
	}
	if !ok || it.SessionID != "s:p:abc" {
		t.Fatalf("got item=%+v ok=%v, want the exact match, not an ambiguity", it, ok)
	}
}

// TestItemForIdentifierPrefixAmbiguousReturnsCandidates covers the several-
// matches outcome: a *AmbiguousError carrying every matching Item, most
// recently active first, rather than ok=false or a silently arbitrary pick.
func TestItemForIdentifierPrefixAmbiguousReturnsCandidates(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:abcd1111", "source": "claude", "source_session_id": "abcd1111", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:abcd2222", "source": "claude", "source_session_id": "abcd2222", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	it, ok, err := ItemForIdentifier(db, "abcd", nil)
	if ok {
		t.Fatalf("expected ok=false for an ambiguous prefix, got item=%+v", it)
	}
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("expected a *AmbiguousError, got %v", err)
	}
	if amb.Identifier != "abcd" {
		t.Errorf("Identifier = %q, want %q", amb.Identifier, "abcd")
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %+v", amb.Candidates)
	}
	if amb.Candidates[0].SessionID != "claude:p:abcd2222" || amb.Candidates[1].SessionID != "claude:p:abcd1111" {
		t.Errorf("expected candidates ordered most-recently-active first, got %+v", amb.Candidates)
	}
}

// TestItemForIdentifierPrefixBelowMinimumLengthDoesNotResolve covers the
// minimum-length floor: a 3-character prefix must not resolve even when it
// would otherwise uniquely match exactly one session, since a fragment that
// short would routinely be ambiguous in a real, larger index.
func TestItemForIdentifierPrefixBelowMinimumLengthDoesNotResolve(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:xyz9999", "source": "claude", "source_session_id": "xyz9999", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "xyz", nil)
	if err != nil {
		t.Fatalf("a too-short prefix should not resolve to nothing without ever being an error, got %v", err)
	}
	if ok {
		t.Fatalf("expected a 3-character prefix not to resolve, got %+v", it)
	}
}

// TestItemForIdentifierPrefixAtMinimumLengthResolves pins the boundary
// itself: exactly 4 characters is long enough to be tried.
func TestItemForIdentifierPrefixAtMinimumLengthResolves(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:xyz9999", "source": "claude", "source_session_id": "xyz9999", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "xyz9", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "claude:p:xyz9999" {
		t.Fatalf("got item=%+v ok=%v, want the 4-character prefix to resolve", it, ok)
	}
}

// TestItemForIdentifierHandleNeverFallsThroughToPrefix covers that an
// all-digit arg is only ever tried as a handle (spec session-search, "Short
// session handle"; design.md decision 4) - never retried as an id prefix
// even when it would otherwise uniquely match one, so an unassigned handle
// is reported as not resolving rather than surprising the caller by
// matching an unrelated session's native id.
func TestItemForIdentifierHandleNeverFallsThroughToPrefix(t *testing.T) {
	db := testDB(t)
	// No lineage is given the handle 1234, but a session's own native id
	// starts with the digits "1234" - if prefix matching were ever tried
	// for an all-digit arg, this would resolve it.
	seedSession(t, db, map[string]any{
		"id": "claude:p:1234567", "source": "claude", "source_session_id": "1234567", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("expected the all-digit arg to be tried only as a handle, not as a prefix, got %+v", it)
	}
}

// TestItemForIdentifierPrefixTreatsPercentLiterally covers that the prefix
// test never uses LIKE/GLOB semantics: a literal '%' in the typed prefix
// must match only a literal '%' in the stored id, never "any sequence of
// characters" the way a LIKE pattern would read it.
func TestItemForIdentifierPrefixTreatsPercentLiterally(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "s:p:ab%1234", "source": "s", "source_session_id": "ab%1234", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	// Under LIKE semantics, the pattern "ab%1%" (prefix "ab%1" plus a
	// trailing wildcard) would also match this session, since '%' reads as
	// "anything" - it does not literally start with "ab%1".
	seedSession(t, db, map[string]any{
		"id": "s:p:abY1999", "source": "s", "source_session_id": "abY1999", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	it, ok, err := ItemForIdentifier(db, "ab%1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "s:p:ab%1234" {
		t.Fatalf("got item=%+v ok=%v, want only the session whose id literally starts with \"ab%%1\"", it, ok)
	}
}

// TestItemForIdentifierPrefixTreatsUnderscoreLiterally is
// TestItemForIdentifierPrefixTreatsPercentLiterally's sibling for '_', which
// GLOB (and, as "any one character", LIKE) also read as a pattern
// character.
func TestItemForIdentifierPrefixTreatsUnderscoreLiterally(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "s:p:ab_1234", "source": "s", "source_session_id": "ab_1234", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	// Under LIKE semantics, "_" matches any single character, so "abY1999"
	// would also match a pattern built from the prefix "ab_1" - it does not
	// literally start with "ab_1".
	seedSession(t, db, map[string]any{
		"id": "s:p:abY1999", "source": "s", "source_session_id": "abY1999", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	it, ok, err := ItemForIdentifier(db, "ab_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || it.SessionID != "s:p:ab_1234" {
		t.Fatalf("got item=%+v ok=%v, want only the session whose id literally starts with \"ab_1\"", it, ok)
	}
}

// TestItemForIdentifierPrefixResolvesArchivedSession covers that a prefix
// resolves an archived session exactly the way an exact id already does
// (ItemBySessionID applies no hide/archive predicate) - archiving a session
// must not make it unreachable by its own id.
func TestItemForIdentifierPrefixResolvesArchivedSession(t *testing.T) {
	db := testDB(t)
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "archived_at"}, []map[string]any{
		{"id": "lin1", "profile": "p", "orphaned": 0, "archived_at": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	seedSession(t, db, map[string]any{
		"id": "claude:p:archived123", "source": "claude", "source_session_id": "archived123", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})

	it, ok, err := ItemForIdentifier(db, "arch", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !it.Archived {
		t.Fatalf("got item=%+v ok=%v, want the archived session to resolve by prefix", it, ok)
	}
}

// The following cover change group-sessions-in-one-index, effective-group
// resolution (see effective_group.go): the query-time CASE expression that
// classifies every session into a group, and its interaction with the
// archive flag, manual overrides, and --agent.

func TestEffectiveGroupByPathPrefix(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:work", "source": "claude", "source_session_id": "work", "lineage_id": "lin-work",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code/proj",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:personal", "source": "claude", "source_session_id": "personal", "lineage_id": "lin-personal",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200, "cwd": "/home/me/personal/x",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:unknown", "source": "claude", "source_session_id": "unknown", "lineage_id": "lin-unknown",
		"end_state": "completed", "resumable": 1, "last_activity_at": 300, "cwd": "/home/me",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:nocwd", "source": "claude", "source_session_id": "nocwd", "lineage_id": "lin-nocwd",
		"end_state": "completed", "resumable": 1, "last_activity_at": 400,
		// deliberately no cwd
	})

	groups := []config.Group{
		{Name: "work", Paths: []string{"/home/me/code"}},
		{Name: "personal", Paths: []string{"/home/me/personal"}},
	}
	items, err := List(db, Filter{Groups: groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, it := range items {
		byID[it.SessionID] = it.Group
	}
	if byID["claude:p:work"] != "work" {
		t.Errorf("~/code session group = %q, want work", byID["claude:p:work"])
	}
	if byID["claude:p:personal"] != "personal" {
		t.Errorf("~/personal/x session group = %q, want personal", byID["claude:p:personal"])
	}
	if byID["claude:p:unknown"] != "" {
		t.Errorf("~ session group = %q, want empty (Unknown)", byID["claude:p:unknown"])
	}
	if byID["claude:p:nocwd"] != "" {
		t.Errorf("nil-cwd session group = %q, want empty (Unknown)", byID["claude:p:nocwd"])
	}
}

// TestEffectiveGroupPrefixIsPathAware covers the requirement that
// "/code-other" must not match a group path of "/code" - the prefix test
// must respect the path separator, not just do a byte-prefix comparison.
func TestEffectiveGroupPrefixIsPathAware(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/code-other/proj",
	})
	groups := []config.Group{{Name: "work", Paths: []string{"/code"}}}
	items, err := List(db, Filter{Groups: groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Group != "" {
		t.Fatalf("expected /code-other to stay Unknown (not match /code), got %+v", items)
	}
}

// TestEffectiveGroupLongestPrefixWins covers a nested pair of group paths:
// the more specific (longer) one must win regardless of config order.
func TestEffectiveGroupLongestPrefixWins(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code/sub/deep",
	})
	// "code" declared first, "code/sub" second - config order must not
	// matter, only path length.
	groups := []config.Group{
		{Name: "outer", Paths: []string{"/home/me/code"}},
		{Name: "inner", Paths: []string{"/home/me/code/sub"}},
	}
	items, err := List(db, Filter{Groups: groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Group != "inner" {
		t.Fatalf("expected the longer prefix (inner) to win, got %+v", items)
	}
}

// TestEffectiveGroupManualOverrideBeatsPath covers a lineage's manual
// group_name taking priority over the path rule, even when the path rule
// would have claimed the session for a different group.
func TestEffectiveGroupManualOverrideBeatsPath(t *testing.T) {
	db := testDB(t)
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "group_name"}, []map[string]any{
		{"id": "lin1", "profile": "p", "orphaned": 0, "group_name": "personal"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code/proj",
	})
	groups := []config.Group{{Name: "work", Paths: []string{"/home/me/code"}}}
	items, err := List(db, Filter{Groups: groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Group != "personal" || !items[0].GroupManual {
		t.Fatalf("expected the manual override to win over the path rule, got %+v", items)
	}
}

// TestEffectiveGroupPathWithGlobSpecialCharsMatchesLiterally covers the
// requirement that the prefix test never use GLOB/LIKE: a configured path
// containing '[' or '%' - both pattern metacharacters to those operators -
// must still match by plain substring comparison.
func TestEffectiveGroupPathWithGlobSpecialCharsMatchesLiterally(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/[proj]/100%done",
	})
	groups := []config.Group{{Name: "work", Paths: []string{"/home/me/[proj]"}}}
	items, err := List(db, Filter{Groups: groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Group != "work" {
		t.Fatalf("expected a literal match against a path containing '[' and '%%', got %+v", items)
	}
}

// TestFilterByGroupExcludesArchivedFromNamedGroupToo covers that
// Filter.Group=<name> is non-archived, exactly like All - an archived
// session filed under "work" must not appear under --group=work, only under
// --group=archive.
func TestFilterByGroupExcludesArchivedFromNamedGroupToo(t *testing.T) {
	db := testDB(t)
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "group_name", "archived_at"}, []map[string]any{
		{"id": "lin-active", "profile": "p", "orphaned": 0, "group_name": "work", "archived_at": nil},
		{"id": "lin-archived", "profile": "p", "orphaned": 0, "group_name": "work", "archived_at": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	seedSession(t, db, map[string]any{
		"id": "claude:p:active", "source": "claude", "source_session_id": "active", "lineage_id": "lin-active",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:archived", "source": "claude", "source_session_id": "archived", "lineage_id": "lin-archived",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})
	groups := []config.Group{{Name: "work", Paths: []string{"/nope"}}}

	all, err := List(db, Filter{Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].SessionID != "claude:p:active" {
		t.Fatalf("All: expected only the active session, got %+v", all)
	}

	work, err := List(db, Filter{Groups: groups, Group: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 || work[0].SessionID != "claude:p:active" {
		t.Fatalf("--group=work: expected only the active session, got %+v", work)
	}

	archived, err := List(db, Filter{Groups: groups, Group: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].SessionID != "claude:p:archived" {
		t.Fatalf("--group=archive: expected only the archived session, got %+v", archived)
	}
}

// TestFilterByGroupUnknown covers Filter.Group="unknown": non-archived
// sessions with no effective group.
func TestFilterByGroupUnknown(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:known", "source": "claude", "source_session_id": "known", "lineage_id": "lin-known",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:unknown", "source": "claude", "source_session_id": "unknown", "lineage_id": "lin-unknown",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200, "cwd": "/tmp",
	})
	groups := []config.Group{{Name: "work", Paths: []string{"/home/me/code"}}}

	items, err := List(db, Filter{Groups: groups, Group: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SessionID != "claude:p:unknown" {
		t.Fatalf("--group=unknown: got %+v", items)
	}
}

// TestConfigGroupChangeRegroupsWithoutRefresh covers that groups are
// resolved at query time: two List calls against the very same rows, with
// different Groups configured (as would happen if the config file were
// edited between them), must classify the session differently - no refresh
// or rewrite of anything already indexed is involved.
func TestConfigGroupChangeRegroupsWithoutRefresh(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code",
	})

	before, err := List(db, Filter{Groups: []config.Group{{Name: "work", Paths: []string{"/home/me/code"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Group != "work" {
		t.Fatalf("before: got %+v, want group work", before)
	}

	after, err := List(db, Filter{Groups: []config.Group{{Name: "renamed", Paths: []string{"/home/me/code"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Group != "renamed" {
		t.Fatalf("after: got %+v, want group renamed - the group must be recomputed from the new config, not cached", after)
	}
}

// TestFilterByAgentMatchesSourceOnly covers change
// group-sessions-in-one-index: Filter.Agent matches a session's source
// alone, so a plain source name (e.g. "claude") matches every install of
// it.
func TestFilterByAgentMatchesSourceOnly(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:claude:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "install": "claude",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:claude-personal:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200, "install": "claude-personal",
	})

	both, err := List(db, Filter{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Fatalf("Agent=claude: expected both installs (matching s.source), got %+v", both)
	}
}

// TestFilterByInstallMatchesOneInstallEvenWhenNameEqualsSource covers the
// bug this follow-up fix addresses: an install's name can equal its own
// source's name (one claude install is literally named "claude"), so
// Filter.Install must be its own clause (s.install = v), never OR-ed with
// Filter.Agent's source match (s.source = v) - the OR'd form made
// Filter.Install narrow to nothing more than Filter.Agent already did,
// because every claude session (any install) also satisfies s.source =
// 'claude'.
func TestFilterByInstallMatchesOneInstallEvenWhenNameEqualsSource(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:claude:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "install": "claude",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:claude-personal:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200, "install": "claude-personal",
	})

	work, err := List(db, Filter{Install: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 || work[0].SessionID != "claude:claude:1" {
		t.Fatalf("Install=claude: expected only the install named \"claude\" (not every claude session), got %+v", work)
	}

	personal, err := List(db, Filter{Install: "claude-personal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(personal) != 1 || personal[0].SessionID != "claude:claude-personal:1" {
		t.Fatalf("Install=claude-personal: got %+v", personal)
	}
}

// TestListWithNoGroupsConfiguredMatchesPreChangeBehavior covers the
// zero-config requirement: with Filter.Groups empty (no [groups.*] tables),
// List's results - ids, order, and count - are unaffected by this change.
// Every Item still gets an (empty) Group, but nothing about which rows come
// back or in what order changes.
func TestListWithNoGroupsConfiguredMatchesPreChangeBehavior(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100, "cwd": "/home/me/code",
	})
	seedSession(t, db, map[string]any{
		"id": "pi:p:1", "source": "pi", "source_session_id": "1", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
	})

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].SessionID != "pi:p:1" || items[1].SessionID != "claude:p:1" {
		t.Fatalf("got %+v, want the same ids and order as before groups existed", items)
	}
	for _, it := range items {
		if it.Group != "" {
			t.Errorf("session %s: Group = %q, want empty with no groups configured", it.SessionID, it.Group)
		}
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

// The following cover the hide rules (change apply-config-hide-rules):
// List/Search must exclude what the config's hide rules and the archive
// flag say to exclude, must never hide 'unknown' or a NULL message_count,
// and ListWithHidden/SearchWithHidden must report how much was suppressed.

// hideSeed seeds the six-session fixture the hide-rule tests share: one
// automated, one interactive, one unknown, one archived, one with a NULL
// message_count, and one whose cwd matches a hide pattern. All synthetic.
func hideSeed(t *testing.T, db *sqlitex.Runner) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "archived_at"}, []map[string]any{
		{"id": "lin-archived", "profile": "p", "orphaned": 0, "archived_at": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []map[string]any{
		{"id": "s:auto", "source": "claude", "source_session_id": "auto", "lineage_id": "lin-auto",
			"end_state": "completed", "resumable": 1, "last_activity_at": 100, "origin": "automated", "message_count": 10},
		{"id": "s:inter", "source": "claude", "source_session_id": "inter", "lineage_id": "lin-inter",
			"end_state": "completed", "resumable": 1, "last_activity_at": 200, "origin": "interactive", "message_count": 10, "cwd": "/home/me/work"},
		{"id": "s:unknown", "source": "claude", "source_session_id": "unknown", "lineage_id": "lin-unknown",
			"end_state": "completed", "resumable": 1, "last_activity_at": 300, "origin": "unknown", "message_count": 10},
		{"id": "s:archived", "source": "claude", "source_session_id": "archived", "lineage_id": "lin-archived",
			"end_state": "completed", "resumable": 1, "last_activity_at": 400, "origin": "interactive", "message_count": 10},
		// deliberately no message_count -> the column stores NULL
		{"id": "s:nullmsg", "source": "claude", "source_session_id": "nullmsg", "lineage_id": "lin-nullmsg",
			"end_state": "completed", "resumable": 1, "last_activity_at": 500, "origin": "interactive"},
		{"id": "s:scratch", "source": "claude", "source_session_id": "scratch", "lineage_id": "lin-scratch",
			"end_state": "completed", "resumable": 1, "last_activity_at": 600, "origin": "interactive", "message_count": 10, "cwd": "/home/me/scratch/proj"},
	} {
		seedSession(t, db, rec)
	}
}

func TestListAppliesDefaultHideRules(t *testing.T) {
	db := testDB(t)
	hideSeed(t, db)

	items, hidden, err := ListWithHidden(db, Filter{Hide: config.Hide{NonInteractive: true}})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 2 {
		t.Fatalf("hidden = %d, want 2 (automated + archived)", hidden)
	}
	byID := map[string]bool{}
	for _, it := range items {
		byID[it.SessionID] = true
	}
	if byID["s:auto"] {
		t.Error("the automated session must be hidden by the non_interactive rule")
	}
	if byID["s:archived"] {
		t.Error("the archived session must be hidden by the archive rule")
	}
	// unknown is never hidden - pi, omp and hermes report it for every
	// session, and hiding on absence of evidence would make three sources
	// vanish.
	if !byID["s:unknown"] {
		t.Error("the unknown-origin session must never be hidden")
	}
	if !byID["s:inter"] || !byID["s:nullmsg"] || !byID["s:scratch"] {
		t.Errorf("the interactive sessions must remain visible, got %v", byID)
	}
}

func TestListAllHideRules(t *testing.T) {
	db := testDB(t)
	hideSeed(t, db)
	hide := config.Hide{NonInteractive: true, MinMessages: 5, Paths: []string{"/home/me/scratch/*"}}

	items, hidden, err := ListWithHidden(db, Filter{Hide: hide})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 3 {
		t.Fatalf("hidden = %d, want 3 (automated + archived + scratch)", hidden)
	}
	byID := map[string]bool{}
	for _, it := range items {
		byID[it.SessionID] = true
	}
	if byID["s:scratch"] {
		t.Error("a session under a matching hide path must be hidden")
	}
	// A NULL message_count is unknown, not small: the threshold must not
	// hide it.
	if !byID["s:nullmsg"] {
		t.Error("a NULL message_count must survive MinMessages")
	}
	if !byID["s:unknown"] {
		t.Error("the unknown-origin session must never be hidden")
	}
}

func TestShowAllDefeatsHideRules(t *testing.T) {
	db := testDB(t)
	hideSeed(t, db)
	hide := config.Hide{NonInteractive: true, MinMessages: 5, Paths: []string{"/home/me/scratch/*"}}

	items, hidden, err := ListWithHidden(db, Filter{Hide: hide, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 0 {
		t.Fatalf("hidden = %d, want 0 under --all", hidden)
	}
	if len(items) != 6 {
		t.Fatalf("got %d items, want all 6", len(items))
	}
}

func TestSearchAppliesHideRules(t *testing.T) {
	db := testDB(t)
	hideSeed(t, db)
	for _, id := range []string{"s:auto", "s:inter", "s:unknown", "s:archived", "s:nullmsg", "s:scratch"} {
		seedPrompt(t, db, id, "hide-me token")
	}
	hide := config.Hide{NonInteractive: true, MinMessages: 5, Paths: []string{"/home/me/scratch/*"}}

	hits, hidden, err := SearchWithHidden(db, "hide-me", Filter{Hide: hide})
	if err != nil {
		t.Fatal(err)
	}
	if hidden != 3 {
		t.Fatalf("hidden = %d, want 3", hidden)
	}
	byID := map[string]bool{}
	for _, it := range hits {
		byID[it.SessionID] = true
	}
	if byID["s:auto"] || byID["s:archived"] || byID["s:scratch"] {
		t.Errorf("hidden sessions leaked into the search hits: %v", byID)
	}
	if !byID["s:unknown"] || !byID["s:nullmsg"] {
		t.Errorf("unknown and NULL-message_count sessions must stay searchable: %v", byID)
	}
}

// GLOB's '*' already crosses '/', so '**' hides exactly what '*' hides -
// the config collapses the former to the latter at load, and either spelling
// behaves identically against the data. The fixture's archived session is
// hidden by the archive flag under both spellings, so the expected count is
// two either way.
func TestDoubleGlobPathHidesLikeSingleGlob(t *testing.T) {
	db := testDB(t)
	hideSeed(t, db)

	_, hiddenStar, err := ListWithHidden(db, Filter{Hide: config.Hide{Paths: []string{"/home/me/scratch/*"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, hiddenDouble, err := ListWithHidden(db, Filter{Hide: config.Hide{Paths: []string{"/home/me/scratch/**"}}})
	if err != nil {
		t.Fatal(err)
	}
	if hiddenStar != 2 || hiddenDouble != 2 {
		t.Fatalf("'**' must hide identically to '*', got %d and %d (want 2 each)", hiddenStar, hiddenDouble)
	}
}

// TestOriginNormalisedOnReadOut covers the origin closed-set invariant on
// the way out: a sessions row whose origin was never written (NULL, as an
// older build or a hand-edit would leave it) or holds a garbage value
// reads back as session.OriginUnknown - never as the raw stored value -
// while a legitimate value comes through unchanged.
func TestOriginNormalisedOnReadOut(t *testing.T) {
	db := testDB(t)
	seedSession(t, db, map[string]any{
		"id": "claude:p:null", "source": "claude", "source_session_id": "null", "lineage_id": "lin1",
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
		// deliberately no origin -> the column stores NULL
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:garbage", "source": "claude", "source_session_id": "garbage", "lineage_id": "lin2",
		"end_state": "completed", "resumable": 1, "last_activity_at": 200,
		"origin": "nonsense",
	})
	seedSession(t, db, map[string]any{
		"id": "claude:p:interactive", "source": "claude", "source_session_id": "interactive", "lineage_id": "lin3",
		"end_state": "completed", "resumable": 1, "last_activity_at": 300,
		"origin": "interactive",
	})

	items, err := List(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]session.Origin{}
	for _, it := range items {
		byID[it.SessionID] = it.Origin
	}
	if byID["claude:p:null"] != session.OriginUnknown {
		t.Errorf("NULL origin read back as %q, want unknown", byID["claude:p:null"])
	}
	if byID["claude:p:garbage"] != session.OriginUnknown {
		t.Errorf("garbage origin read back as %q, want unknown", byID["claude:p:garbage"])
	}
	if byID["claude:p:interactive"] != session.OriginInteractive {
		t.Errorf("valid origin read back as %q, want interactive", byID["claude:p:interactive"])
	}
}
