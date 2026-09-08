package goose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

func strp(s string) *string { return &s }

func TestClassifyEndState(t *testing.T) {
	cases := []struct {
		name string
		row  sessionRow
		want session.EndState
	}{
		{"no messages at all", sessionRow{}, session.EndStateUnknown},
		{"last message a dangling user prompt", sessionRow{LastMsgRole: strp("user"), LastMsgContent: strp(`[{"type":"text","text":"hi"}]`)}, session.EndStateDangling},
		{"last message a tool result awaiting the agent", sessionRow{LastMsgRole: strp("user"), LastMsgContent: strp(`[{"type":"toolResponse"}]`)}, session.EndStateUnknown},
		{"assistant finished with text", sessionRow{LastMsgRole: strp("assistant"), LastMsgContent: strp(`[{"type":"text","text":"done"}]`)}, session.EndStateCompleted},
		{"assistant mid tool call", sessionRow{LastMsgRole: strp("assistant"), LastMsgContent: strp(`[{"type":"toolRequest"}]`)}, session.EndStateInterrupted},
		{"assistant text then a trailing tool request", sessionRow{LastMsgRole: strp("assistant"), LastMsgContent: strp(`[{"type":"text","text":"let me check"},{"type":"toolRequest"}]`)}, session.EndStateInterrupted},
		{"unparseable content", sessionRow{LastMsgRole: strp("assistant"), LastMsgContent: strp(`not json`)}, session.EndStateUnknown},
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
	if got := topicFor(sessionRow{Name: strp("20260805_1"), UserSetName: 0}); got != nil {
		t.Errorf("a default (not user-set) name must not become the topic, got %q", *got)
	}
	if got := topicFor(sessionRow{Name: strp("fix the retry loop"), UserSetName: 1}); got == nil || *got != "fix the retry loop" {
		t.Errorf("a user-set name should become the topic, got %v", got)
	}
}

func TestClientFor(t *testing.T) {
	if got := clientFor(sessionRow{SessionType: strp("acp")}); got == nil || *got != "acp" {
		t.Errorf("acp session_type should become Client %q, got %v", "acp", got)
	}
	if got := clientFor(sessionRow{SessionType: strp("scheduled")}); got == nil || *got != "scheduled" {
		t.Errorf("scheduled session_type should become Client, got %v", got)
	}
	if got := clientFor(sessionRow{SessionType: strp("user")}); got != nil {
		t.Errorf("an ordinary user session should report no Client, got %v", got)
	}
}

func testDB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(sessionsDir, "sessions.db")
	r := &sqlitex.Runner{DBPath: dbPath}
	ddl := `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', user_set_name BOOLEAN DEFAULT FALSE,
	session_type TEXT NOT NULL DEFAULT 'user', working_dir TEXT NOT NULL,
	created_at TIMESTAMP, updated_at TIMESTAMP, archived_at TIMESTAMP, parent_session_id TEXT
);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
	content_json TEXT NOT NULL, created_timestamp INTEGER NOT NULL
);`
	if err := r.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	// Root is the *parent* of the sessions/ directory the real DB lives
	// under (~/.local/share/goose/sessions/sessions.db, root configured as
	// ~/.local/share/goose/sessions) - so the profile root passed to
	// Discover is sessionsDir, matching the real config.Roots entry.
	return sessionsDir
}

func TestDiscover(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "sessions.db")}

	inserts := `
INSERT INTO sessions (id, name, user_set_name, session_type, working_dir, created_at, updated_at, parent_session_id) VALUES
  ('s_completed', '20260805_1', 0, 'user', '/Users/x/project', '2026-08-05 07:38:36', '2026-08-05 07:41:54', NULL),
  ('s_acp', '20260805_2', 0, 'acp', '/Users/x/editor-project', '2026-08-05 08:00:00', '2026-08-05 08:05:00', NULL),
  ('s_hidden', '20260805_3', 0, 'hidden', '/Users/x/internal', '2026-08-05 08:10:00', '2026-08-05 08:10:00', NULL),
  ('s_named', 'fix the retry loop', 1, 'user', '/Users/x/project', '2026-08-05 09:00:00', '2026-08-05 09:05:00', 's_completed'),
  ('s_empty', '20260805_4', 0, 'user', '/Users/x/other', '2026-08-05 10:00:00', '2026-08-05 10:00:00', NULL);
INSERT INTO messages (session_id, role, content_json, created_timestamp) VALUES
  ('s_completed', 'user', '[{"type":"text","text":"hi"}]', 1785915542),
  ('s_completed', 'assistant', '[{"type":"text","text":"hello"}]', 1785915547),
  ('s_acp', 'assistant', '[{"type":"toolRequest"}]', 1785920000),
  ('s_named', 'user', '[{"type":"text","text":"still going"}]', 1785930000);`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	a := New("")
	discovered, err := a.Discover(profile.Profile{Name: "default", Roots: map[string]string{"goose": root}})
	if err != nil {
		t.Fatal(err)
	}
	// s_hidden must be excluded entirely.
	if len(discovered) != 4 {
		t.Fatalf("got %d sessions, want 4 (hidden excluded)", len(discovered))
	}

	byID := map[string]session.Session{}
	for _, d := range discovered {
		byID[d.Session.SourceSessionID] = d.Session
		if d.Session.SourceSessionID == "s_hidden" {
			t.Error("s_hidden (session_type=hidden) must not be discovered")
		}
	}

	completed := byID["s_completed"]
	if completed.EndState != session.EndStateCompleted {
		t.Errorf("s_completed EndState = %v, want completed", completed.EndState)
	}
	if completed.Topic != nil {
		t.Errorf("s_completed Topic = %v, want nil (name was not user-set)", *completed.Topic)
	}
	if completed.Client != nil {
		t.Errorf("s_completed Client = %v, want nil (plain user session)", *completed.Client)
	}
	if completed.CWD == nil || *completed.CWD != "/Users/x/project" {
		t.Errorf("s_completed CWD = %v", completed.CWD)
	}
	if completed.StartedAt == nil {
		t.Fatal("s_completed StartedAt not parsed")
	}
	if got := completed.StartedAt.Format("2006-01-02 15:04:05"); got != "2026-08-05 07:38:36" {
		t.Errorf("s_completed StartedAt = %s, want the parsed created_at", got)
	}
	if completed.TranscriptPath != nil {
		t.Error("goose sessions must never carry a TranscriptPath - no transcript exists")
	}

	acp := byID["s_acp"]
	if acp.Client == nil || *acp.Client != "acp" {
		t.Errorf("s_acp Client = %v, want acp", acp.Client)
	}
	if acp.EndState != session.EndStateInterrupted {
		t.Errorf("s_acp EndState = %v, want interrupted (last message is a bare toolRequest)", acp.EndState)
	}

	named := byID["s_named"]
	if named.Topic == nil || *named.Topic != "fix the retry loop" {
		t.Errorf("s_named Topic = %v, want the user-set name", named.Topic)
	}
	if named.ContinuesFrom == nil || *named.ContinuesFrom != "s_completed" {
		t.Errorf("s_named ContinuesFrom = %v, want s_completed", named.ContinuesFrom)
	}
	if named.EndState != session.EndStateDangling {
		t.Errorf("s_named EndState = %v, want dangling", named.EndState)
	}

	empty := byID["s_empty"]
	if empty.EndState != session.EndStateUnknown {
		t.Errorf("s_empty (no messages) EndState = %v, want unknown", empty.EndState)
	}
}

func TestDiscoverUnavailableWhenNoRoot(t *testing.T) {
	a := New("")
	if _, err := a.Discover(profile.Profile{Name: "x"}); err == nil {
		t.Fatal("expected error when goose root is not configured")
	}
}

func TestDiscoverUnavailableWhenDatabaseMissing(t *testing.T) {
	a := New("")
	root := t.TempDir()
	if _, err := a.Discover(profile.Profile{Name: "x", Roots: map[string]string{"goose": root}}); err == nil {
		t.Fatal("expected error when sessions.db does not exist at the configured root")
	}
}

func TestPromptsSince(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "sessions.db")}

	inserts := `
INSERT INTO sessions (id, working_dir) VALUES ('s_a', '/Users/x/project');
INSERT INTO messages (session_id, role, content_json, created_timestamp) VALUES
  ('s_a', 'user', '[{"type":"text","text":"first prompt"}]', 1000),
  ('s_a', 'assistant', '[{"type":"toolRequest"}]', 1001),
  ('s_a', 'user', '[{"type":"toolResponse"}]', 1002),
  ('s_a', 'assistant', '[{"type":"text","text":"the reply"}]', 1003),
  ('s_a', 'user', '[{"type":"text","text":"second prompt"}]', 1004);`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	a := New("")
	p := profile.Profile{Name: "default", Roots: map[string]string{"goose": root}}

	entries, cursor, err := a.PromptsSince(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the toolResponse-only message must be excluded): %+v", len(entries), entries)
	}
	if entries[0].Text != "first prompt" || entries[1].Text != "second prompt" {
		t.Errorf("entries = %+v, want first/second prompt in order", entries)
	}

	entries2, cursor2, err := a.PromptsSince(p, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 0 {
		t.Errorf("second call from the cursor returned %d entries, want 0", len(entries2))
	}
	if cursor2 != cursor {
		t.Errorf("cursor moved from %d to %d with nothing new to advance it", cursor, cursor2)
	}
}

func TestPromptsSinceNoRootIsNotAnError(t *testing.T) {
	a := New("")
	entries, cursor, err := a.PromptsSince(profile.Profile{Name: "x"}, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil || cursor != 5 {
		t.Errorf("got entries=%v cursor=%d, want nil/5 unchanged", entries, cursor)
	}
}
