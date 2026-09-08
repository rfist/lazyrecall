package antigravity

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

func strp(s string) *string { return &s }

func TestClassifyEndState(t *testing.T) {
	cases := []struct {
		name string
		row  conversationRow
		want session.EndState
	}{
		{"no signal at all", conversationRow{}, session.EndStateUnknown},
		{"killed", conversationRow{Killed: 1}, session.EndStateInterrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEndState(tc.row)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTopicFor(t *testing.T) {
	if got := topicFor(conversationRow{Title: strp("Fix the flaky test")}); got == nil || *got != "Fix the flaky test" {
		t.Errorf("a non-empty title should win, got %v", got)
	}
	if got := topicFor(conversationRow{Title: strp(""), Preview: strp("investigating the retry loop")}); got == nil || *got != "investigating the retry loop" {
		t.Errorf("an empty title should fall back to preview, got %v", got)
	}
	if got := topicFor(conversationRow{}); got != nil {
		t.Errorf("no title and no preview should be nil, got %v", *got)
	}
}

func TestFirstWorkspaceDir(t *testing.T) {
	if got := firstWorkspaceDir(strp(`["file:///Users/x/personal/discordo"]`)); got == nil || *got != "/Users/x/personal/discordo" {
		t.Errorf("got %v, want the stripped first URI", got)
	}
	if got := firstWorkspaceDir(strp(`[]`)); got != nil {
		t.Errorf("an empty array should be nil, got %v", *got)
	}
	if got := firstWorkspaceDir(strp(`not json`)); got != nil {
		t.Errorf("unparseable input should be nil, got %v", *got)
	}
	if got := firstWorkspaceDir(nil); got != nil {
		t.Errorf("nil input should be nil, got %v", *got)
	}
}

// TestValidTimeRejectsTheGoZeroValue is a regression test for a real row
// found on this machine while verifying this adapter against actual data:
// one conversation's last_modified_time was literally "0001-01-01
// 00:00:00+00:00", which without this guard renders as a session
// "106751 days ago" - not a hypothetical edge case.
func TestValidTimeRejectsTheGoZeroValue(t *testing.T) {
	var zero time.Time
	if got := validTime(&zero); got != nil {
		t.Errorf("the Go zero time should be treated as no timestamp recorded, got %v", *got)
	}
	real := time.Date(2026, 7, 10, 15, 16, 32, 0, time.UTC)
	if got := validTime(&real); got == nil || !got.Equal(real) {
		t.Errorf("a real timestamp should pass through unchanged, got %v", got)
	}
	if got := validTime(nil); got != nil {
		t.Errorf("nil should stay nil, got %v", *got)
	}
}

func testDB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "conversation_summaries.db")
	r := &sqlitex.Runner{DBPath: dbPath}
	ddl := "CREATE TABLE conversation_summaries (" +
		"conversation_id text, title text NOT NULL DEFAULT '', preview text NOT NULL DEFAULT '', " +
		"step_count integer NOT NULL DEFAULT 0, last_modified_time datetime NOT NULL, " +
		"workspace_uris text NOT NULL, status text NOT NULL DEFAULT '', " +
		"parent_conversation_id text NOT NULL DEFAULT '', not_fully_idle numeric NOT NULL DEFAULT 0, " +
		"killed numeric NOT NULL DEFAULT 0, last_user_input_time datetime NOT NULL, " +
		"PRIMARY KEY (conversation_id));"
	if err := r.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDiscover(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "conversation_summaries.db")}

	inserts := `
INSERT INTO conversation_summaries
  (conversation_id, title, preview, step_count, last_modified_time, workspace_uris, parent_conversation_id, killed, last_user_input_time)
VALUES
  ('c_titled', 'Fix the flaky test', '', 6, '2026-07-10 15:16:32.035698+00:00', '["file:///Users/x/personal/discordo"]', '', 0, '2026-07-10 15:16:32.035698+00:00'),
  ('c_preview_only', '', 'investigating the retry loop', 8, '2026-07-03 18:47:55.893941+00:00', '["file:///Users/x/other"]', '', 0, '2026-07-03 18:47:55.893941+00:00'),
  ('c_killed', 'A killed run', '', 3, '2026-07-03 18:45:45.889283+00:00', '["file:///Users/x/killed"]', '', 1, '2026-07-03 18:45:45.889283+00:00'),
  ('c_child', 'A follow-up', '', 2, '2026-07-11 10:00:00+00:00', '["file:///Users/x/personal/discordo"]', 'c_titled', 0, '2026-07-11 10:00:00+00:00'),
  ('c_no_workspace', 'Untitled', '', 0, '2026-07-01 00:00:00+00:00', '[]', '', 0, '2026-07-01 00:00:00+00:00');`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	a := New("")
	discovered, err := a.Discover(profile.Profile{Name: "default", Roots: map[string]string{"antigravity": root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 5 {
		t.Fatalf("got %d sessions, want 5", len(discovered))
	}

	byID := map[string]session.Session{}
	for _, d := range discovered {
		byID[d.Session.SourceSessionID] = d.Session
	}

	titled := byID["c_titled"]
	if titled.Topic == nil || *titled.Topic != "Fix the flaky test" {
		t.Errorf("c_titled Topic = %v, want the title", titled.Topic)
	}
	if titled.CWD == nil || *titled.CWD != "/Users/x/personal/discordo" {
		t.Errorf("c_titled CWD = %v", titled.CWD)
	}
	if titled.MessageCount == nil || *titled.MessageCount != 6 {
		t.Errorf("c_titled MessageCount = %v, want 6", titled.MessageCount)
	}
	if titled.EndState != session.EndStateUnknown {
		t.Errorf("c_titled EndState = %v, want unknown (no signal)", titled.EndState)
	}
	if titled.StartedAt != nil {
		t.Error("StartedAt must stay nil - this schema has no creation-time column")
	}
	if titled.LastActivityAt == nil {
		t.Error("LastActivityAt should be set from last_modified_time")
	}
	if titled.TranscriptPath != nil {
		t.Error("antigravity sessions must never carry a TranscriptPath - no transcript exists")
	}
	if !titled.Resumable {
		t.Error("c_titled has a workspace and should be resumable")
	}

	previewOnly := byID["c_preview_only"]
	if previewOnly.Topic == nil || *previewOnly.Topic != "investigating the retry loop" {
		t.Errorf("c_preview_only Topic = %v, want the preview (empty title)", previewOnly.Topic)
	}

	killed := byID["c_killed"]
	if killed.EndState != session.EndStateInterrupted {
		t.Errorf("c_killed EndState = %v, want interrupted", killed.EndState)
	}

	child := byID["c_child"]
	if child.ContinuesFrom == nil || *child.ContinuesFrom != "c_titled" {
		t.Errorf("c_child ContinuesFrom = %v, want c_titled", child.ContinuesFrom)
	}

	noWorkspace := byID["c_no_workspace"]
	if noWorkspace.CWD != nil {
		t.Errorf("c_no_workspace CWD = %v, want nil (empty workspace_uris array)", *noWorkspace.CWD)
	}
	if noWorkspace.Resumable {
		t.Error("c_no_workspace has no CWD and must not be resumable")
	}
}

func TestDiscoverUnavailableWhenNoRoot(t *testing.T) {
	a := New("")
	if _, err := a.Discover(profile.Profile{Name: "x"}); err == nil {
		t.Fatal("expected error when antigravity root is not configured")
	}
}

func TestDiscoverUnavailableWhenDatabaseMissing(t *testing.T) {
	a := New("")
	root := t.TempDir()
	if _, err := a.Discover(profile.Profile{Name: "x", Roots: map[string]string{"antigravity": root}}); err == nil {
		t.Fatal("expected error when conversation_summaries.db does not exist at the configured root")
	}
}
