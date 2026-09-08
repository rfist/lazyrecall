package opencode

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
	"github.com/rfist/lazyrecall/internal/transcript"
)

func strp(s string) *string { return &s }

func TestClassifyEndState(t *testing.T) {
	cases := []struct {
		name string
		row  sessionRow
		want session.EndState
	}{
		{"no messages at all", sessionRow{}, session.EndStateUnknown},
		{"last message from user", sessionRow{LastMsgRole: strp("user")}, session.EndStateDangling},
		{"assistant stopped cleanly", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("stop")}, session.EndStateCompleted},
		{"assistant mid tool call", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("tool-calls")}, session.EndStateInterrupted},
		{"assistant errored", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("error")}, session.EndStateUnknown},
		{"assistant aborted", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("aborted")}, session.EndStateUnknown},
		{"assistant with no finish recorded", sessionRow{LastMsgRole: strp("assistant")}, session.EndStateUnknown},
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

// testDB builds a throwaway opencode.db under a fresh temp root, matching
// the real legacy session/message/part schema (confirmed against the real
// database on this machine before writing this adapter - see the plan's
// verification step). Returns the root directory Discover/PromptsSince
// expect in profile.Profile.Roots["opencode"].
func testDB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	r := &sqlitex.Runner{DBPath: dbPath}
	ddl := `
CREATE TABLE session (
	id text PRIMARY KEY, parent_id text, directory text NOT NULL,
	title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL
);
CREATE TABLE message (
	id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
	time_updated integer NOT NULL, data text NOT NULL
);
CREATE TABLE part (
	id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
	time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL
);`
	if err := r.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDiscover(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "opencode.db")}

	inserts := `
INSERT INTO session (id, parent_id, directory, title, time_created, time_updated)
VALUES
  ('ses_completed', NULL, '/Users/x/project', 'Fix the flaky test', 1783455524064, 1783455607192),
  ('ses_dangling', NULL, '/Users/x/other', '', 1783455524064, 1783455524064),
  ('ses_forked', 'ses_completed', '/Users/x/project', 'Follow-up', 1783455700000, 1783455700000),
  ('ses_empty', NULL, '/Users/x/empty', 'Never started', 1783455000000, 1783455000000);
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('msg1', 'ses_completed', 1783455524064, 1783455524064, '{"role":"user","finish":null}'),
  ('msg2', 'ses_completed', 1783455607192, 1783455607192, '{"role":"assistant","finish":"stop"}'),
  ('msg3', 'ses_dangling', 1783455524064, 1783455524064, '{"role":"user"}'),
  ('msg4', 'ses_forked', 1783455700000, 1783455700000, '{"role":"assistant","finish":"tool-calls"}');`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	a := New("")
	discovered, err := a.Discover(profile.Profile{Name: "default", Roots: map[string]string{"opencode": root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 4 {
		t.Fatalf("got %d sessions, want 4", len(discovered))
	}

	byID := map[string]session.Session{}
	for _, d := range discovered {
		byID[d.Session.SourceSessionID] = d.Session
	}

	completed := byID["ses_completed"]
	if completed.EndState != session.EndStateCompleted {
		t.Errorf("ses_completed EndState = %v, want completed", completed.EndState)
	}
	if completed.Topic == nil || *completed.Topic != "Fix the flaky test" {
		t.Errorf("ses_completed Topic = %v, want the session title", completed.Topic)
	}
	if completed.CWD == nil || *completed.CWD != "/Users/x/project" {
		t.Errorf("ses_completed CWD = %v", completed.CWD)
	}
	if completed.MessageCount == nil || *completed.MessageCount != 2 {
		t.Errorf("ses_completed MessageCount = %v, want 2", completed.MessageCount)
	}
	if completed.TranscriptPath != nil {
		t.Error("opencode sessions must never carry a TranscriptPath - no transcript exists")
	}
	if !completed.Resumable {
		t.Error("ses_completed has a directory and should be resumable")
	}

	dangling := byID["ses_dangling"]
	if dangling.EndState != session.EndStateDangling {
		t.Errorf("ses_dangling EndState = %v, want dangling", dangling.EndState)
	}
	if dangling.Topic != nil {
		t.Errorf("ses_dangling Topic = %v, want nil for an empty title (never a blank string)", *dangling.Topic)
	}

	forked := byID["ses_forked"]
	if forked.ContinuesFrom == nil || *forked.ContinuesFrom != "ses_completed" {
		t.Errorf("ses_forked ContinuesFrom = %v, want ses_completed", forked.ContinuesFrom)
	}
	if forked.EndState != session.EndStateInterrupted {
		t.Errorf("ses_forked EndState = %v, want interrupted", forked.EndState)
	}

	empty := byID["ses_empty"]
	if empty.EndState != session.EndStateUnknown {
		t.Errorf("ses_empty (no messages) EndState = %v, want unknown", empty.EndState)
	}
	if empty.MessageCount == nil || *empty.MessageCount != 0 {
		t.Errorf("ses_empty MessageCount = %v, want 0", empty.MessageCount)
	}
}

func TestDiscoverUnavailableWhenNoRoot(t *testing.T) {
	a := New("")
	if _, err := a.Discover(profile.Profile{Name: "x"}); err == nil {
		t.Fatal("expected error when opencode root is not configured")
	}
}

func TestDiscoverUnavailableWhenDatabaseMissing(t *testing.T) {
	a := New("")
	root := t.TempDir()
	if _, err := a.Discover(profile.Profile{Name: "x", Roots: map[string]string{"opencode": root}}); err == nil {
		t.Fatal("expected error when opencode.db does not exist at the configured root")
	}
}

func TestPromptsSince(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "opencode.db")}

	inserts := `
INSERT INTO session (id, parent_id, directory, title, time_created, time_updated)
VALUES ('ses_a', NULL, '/Users/x/project', 'A session', 1000, 1000);
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('msg1', 'ses_a', 1000, 1000, '{"role":"user"}'),
  ('msg2', 'ses_a', 2000, 2000, '{"role":"assistant","finish":"stop"}'),
  ('msg3', 'ses_a', 3000, 3000, '{"role":"user"}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('p1', 'msg1', 'ses_a', 1000, 1000, '{"type":"text","text":"first prompt"}'),
  ('p2', 'msg2', 'ses_a', 2000, 2000, '{"type":"text","text":"the reply, not a prompt"}'),
  ('p3', 'msg2', 'ses_a', 2000, 2000, '{"type":"tool","tool":"bash"}'),
  ('p4', 'msg3', 'ses_a', 3000, 3000, '{"type":"text","text":""}'),
  ('p5', 'msg3', 'ses_a', 3000, 3000, '{"type":"text","text":"second prompt"}');`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	a := New("")
	p := profile.Profile{Name: "default", Roots: map[string]string{"opencode": root}}

	entries, cursor, err := a.PromptsSince(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the reply text, the tool part, and the empty-text part must all be excluded): %+v", len(entries), entries)
	}
	if entries[0].Text != "first prompt" || entries[1].Text != "second prompt" {
		t.Errorf("entries = %+v, want first/second prompt in order", entries)
	}
	if entries[0].SessionID != "ses_a" {
		t.Errorf("SessionID = %q, want ses_a", entries[0].SessionID)
	}

	// A second call from the returned cursor must see nothing new.
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

// All content below is synthetic, hand-written test data - never real
// session content.

func TestConversation(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "opencode.db")}
	inserts := `
INSERT INTO session (id, parent_id, directory, title, time_created, time_updated)
VALUES ('ses_a', NULL, '/Users/x/project', 'A session', 1783455524064, 1783455607192);
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('msg_1', 'ses_a', 1783455524064, 1783455524064, '{"role":"user"}'),
  ('msg_2', 'ses_a', 1783455525000, 1783455525000, '{"role":"assistant"}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('prt_1', 'msg_1', 'ses_a', 1783455524064, 1783455524064, '{"type":"text","text":"where did the config go"}'),
  ('prt_2', 'msg_2', 'ses_a', 1783455525000, 1783455525000, '{"type":"reasoning","text":"thinking to myself"}'),
  ('prt_3', 'msg_2', 'ses_a', 1783455525100, 1783455525100, '{"type":"tool","tool":"grep","callID":"c1"}'),
  ('prt_4', 'msg_2', 'ses_a', 1783455525200, 1783455525200, '{"type":"step-finish"}'),
  ('prt_5', 'msg_2', 'ses_a', 1783455525300, 1783455525300, '{"type":"text","text":"it moved under etc/"}');`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	p := profile.Profile{Name: "default", Roots: map[string]string{"opencode": root}}
	turns, _, err := New("").Conversation(p, "ses_a", transcript.DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}

	// reasoning is the model thinking to itself and step-finish is
	// bookkeeping; neither is content a reader wants a line for.
	want := []transcript.Kind{
		transcript.KindUserPrompt, transcript.KindToolUse, transcript.KindAssistantText,
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i, k := range want {
		if turns[i].Kind != k {
			t.Errorf("turn %d kind = %v, want %v", i, turns[i].Kind, k)
		}
	}
	if turns[0].Text != "where did the config go" {
		t.Errorf("first turn = %q", turns[0].Text)
	}
	if got := strings.Join(turns[1].Tool, ","); got != "grep" {
		t.Errorf("tool name = %q, want grep", got)
	}
	if turns[2].Text != "it moved under etc/" {
		t.Errorf("last turn = %q", turns[2].Text)
	}
}

// The role on a text part comes from its message, not from the part: a
// user turn and an assistant turn are otherwise identical rows.
func TestConversationTakesTheSpeakerFromTheMessage(t *testing.T) {
	root := testDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "opencode.db")}
	inserts := `
INSERT INTO session (id, parent_id, directory, title, time_created, time_updated)
VALUES ('ses_a', NULL, '/x', 'A', 1, 2);
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('msg_1', 'ses_a', 1, 1, '{"role":"assistant"}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('prt_1', 'msg_1', 'ses_a', 1, 1, '{"type":"text","text":"said by the agent"}');`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}
	turns, _, err := New("").Conversation(
		profile.Profile{Name: "default", Roots: map[string]string{"opencode": root}},
		"ses_a", transcript.DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Kind != transcript.KindAssistantText {
		t.Errorf("turns = %+v, want one assistant turn", turns)
	}
}

// Kilo is this same adapter pointed at a different name and file, so the
// conversation reader has to follow both rather than the opencode ones.
func TestKiloVariantReadsItsOwnDatabase(t *testing.T) {
	root := t.TempDir()
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "kilo.db")}
	ddl := `
CREATE TABLE session (
	id text PRIMARY KEY, parent_id text, directory text NOT NULL,
	title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL
);
CREATE TABLE message (
	id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
	time_updated integer NOT NULL, data text NOT NULL
);
CREATE TABLE part (
	id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
	time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL
);
INSERT INTO session (id, parent_id, directory, title, time_created, time_updated)
VALUES ('ses_k', NULL, '/x', 'K', 1, 2);
INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
  ('msg_1', 'ses_k', 1, 1, '{"role":"user"}');
INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES
  ('prt_1', 'msg_1', 'ses_k', 1, 1, '{"type":"text","text":"from the kilo database"}');`
	if err := r.Exec(ddl); err != nil {
		t.Fatal(err)
	}

	a := NewFor("kilo", "kilo.db", "")
	if a.Name() != "kilo" {
		t.Errorf("Name() = %q, want kilo", a.Name())
	}
	turns, _, err := a.Conversation(
		profile.Profile{Name: "default", Roots: map[string]string{"kilo": root}},
		"ses_k", transcript.DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Text != "from the kilo database" {
		t.Errorf("turns = %+v, want the kilo database's own message", turns)
	}
}
