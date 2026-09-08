package hermes

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
func i64p(i int64) *int64   { return &i }

func TestClassifyEndState(t *testing.T) {
	cases := []struct {
		name string
		row  sessionRow
		want session.EndState
	}{
		{"no messages at all", sessionRow{}, session.EndStateUnknown},
		{"last message from user", sessionRow{LastMsgRole: strp("user")}, session.EndStateDangling},
		{"assistant stopped cleanly", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("stop")}, session.EndStateCompleted},
		{"assistant mid tool call", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("tool_calls"), LastMsgHasToolCalls: i64p(1)}, session.EndStateInterrupted},
		{"assistant awaiting verification", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("verification_required")}, session.EndStateInterrupted},
		{"assistant with no finish reason recorded", sessionRow{LastMsgRole: strp("assistant")}, session.EndStateUnknown},
		{"last message a bare tool result", sessionRow{LastMsgRole: strp("tool")}, session.EndStateUnknown},
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

// All content below is synthetic, hand-written test data - never real
// session content.

func conversationTestDB(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "state.db")}
	ddl := `
CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, created_at REAL, updated_at REAL);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
	content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
	timestamp REAL NOT NULL, finish_reason TEXT
);`
	if err := r.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestConversation(t *testing.T) {
	root := conversationTestDB(t)
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "state.db")}
	inserts := `
INSERT INTO sessions (id, title) VALUES ('s_a', 'a session');
INSERT INTO messages (session_id, role, content, tool_calls, tool_name, timestamp) VALUES
  ('s_a', 'user', 'what broke the deploy', NULL, NULL, 1000),
  ('s_a', 'assistant', 'Let me look.', '[{"function":{"name":"read_file"}}]', NULL, 1001),
  ('s_a', 'tool', 'the file contents', NULL, 'read_file', 1002),
  ('s_a', 'assistant', 'The image tag was stale.', NULL, NULL, 1003);`
	if err := r.Exec(inserts); err != nil {
		t.Fatal(err)
	}

	p := profile.Profile{Name: "default", Roots: map[string]string{"hermes": root}}
	turns, _, err := New("").Conversation(p, "s_a", transcript.DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}

	// An assistant row carrying both a sentence and a tool call becomes two
	// turns; the "tool" role is the result coming back and is skipped.
	want := []transcript.Kind{
		transcript.KindUserPrompt,
		transcript.KindAssistantText,
		transcript.KindToolUse,
		transcript.KindAssistantText,
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i, k := range want {
		if turns[i].Kind != k {
			t.Errorf("turn %d kind = %v, want %v", i, turns[i].Kind, k)
		}
	}
	if got := strings.Join(turns[2].Tool, ","); got != "read_file" {
		t.Errorf("tool name = %q, want read_file (from the OpenAI-shaped function.name)", got)
	}
	if turns[3].Text != "The image tag was stale." {
		t.Errorf("last turn = %q", turns[3].Text)
	}
}

func TestHermesToolNamesReadsBothShapes(t *testing.T) {
	if got := hermesToolNames(`[{"name":"bash"}]`); len(got) != 1 || got[0] != "bash" {
		t.Errorf("bare name shape gave %v", got)
	}
	if got := hermesToolNames(`[{"function":{"name":"bash"}}]`); len(got) != 1 || got[0] != "bash" {
		t.Errorf("function.name shape gave %v", got)
	}
	if got := hermesToolNames(`[{"function":{"name":"bash"}},{"name":"bash"}]`); len(got) != 1 {
		t.Errorf("the same tool twice should be listed once, got %v", got)
	}
	if got := hermesToolNames(`not json`); got != nil {
		t.Errorf("unparseable tool_calls should name nothing, got %v", got)
	}
}
