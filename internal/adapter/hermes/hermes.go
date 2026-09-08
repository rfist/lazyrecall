// Package hermes adapts hermes's on-disk session data: everything lives in
// SQLite at ~/.hermes/state.db, no transcript files exist at all (design.md
// context; task 5.5). hermes's sessions table is the reference shape for
// LazyRecall's own session model - it is the only source that records both a
// topic and an explicit signal for how a session ended, so this adapter
// builds complete session.Session records directly, with no separate
// transcript-scanning step. User prompts for tier 2 come from the messages
// table filtered to role = 'user'. Every read goes through sqlitex in
// read-only mode - hermes has an active writer and this adapter must never
// disturb it.
package hermes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rfist/lazyrecall/internal/adapter"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
	"github.com/rfist/lazyrecall/internal/transcript"
)

type Adapter struct {
	SQLite3Path string
}

func New(sqlite3Path string) *Adapter { return &Adapter{SQLite3Path: sqlite3Path} }

func (*Adapter) Name() string { return "hermes" }

type sessionRow struct {
	ID                  string   `json:"id"`
	Source              string   `json:"source"`
	Title               *string  `json:"title"`
	ParentSessionID     *string  `json:"parent_session_id"`
	StartedAt           float64  `json:"started_at"`
	EndedAt             *float64 `json:"ended_at"`
	EndReason           *string  `json:"end_reason"`
	MessageCount        *int64   `json:"message_count"`
	CWD                 *string  `json:"cwd"`
	GitBranch           *string  `json:"git_branch"`
	GitRepoRoot         *string  `json:"git_repo_root"`
	LastActivityAt      *float64 `json:"last_activity_at"`
	LastMsgRole         *string  `json:"last_msg_role"`
	LastMsgFinish       *string  `json:"last_msg_finish_reason"`
	LastMsgHasToolCalls *int64   `json:"last_msg_has_tool_calls"`
}

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no hermes root for this profile"}
	}
	dbPath := filepath.Join(root, "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, &adapter.Unavailable{Reason: "no hermes database on this machine"}
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	// The last message per session (role, finish_reason, whether it carried
	// tool calls) is joined in so end-state classification can use hermes's
	// own recorded message-level completion signal rather than any
	// time-based inference (task 5.5: "end state taken from the source
	// rather than inferred").
	const q = `
WITH last_msg AS (
  SELECT session_id, role, finish_reason, (tool_calls IS NOT NULL) AS has_tool_calls,
         ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY id DESC) AS rn
  FROM messages
)
SELECT
  s.id, s.source, s.title, s.parent_session_id, s.started_at, s.ended_at, s.end_reason,
  s.message_count, s.cwd, s.git_branch, s.git_repo_root, s.last_activity_at,
  lm.role AS last_msg_role, lm.finish_reason AS last_msg_finish_reason, lm.has_tool_calls AS last_msg_has_tool_calls
FROM sessions s
LEFT JOIN last_msg lm ON lm.session_id = s.id AND lm.rn = 1;`

	var rows []sessionRow
	if err := r.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("hermes: querying state.db: %w", err)
	}

	out := make([]adapter.Discovered, 0, len(rows))
	for _, row := range rows {
		s := session.Session{
			ID:              "hermes:" + p.Name + ":" + row.ID,
			Source:          "hermes",
			Profile:         p.Name,
			SourceSessionID: row.ID,
			Topic:           row.Title,
			ContinuesFrom:   row.ParentSessionID,
			CWD:             row.CWD,
			GitBranch:       row.GitBranch,
			GitRepoRoot:     row.GitRepoRoot,
			MessageCount:    row.MessageCount,
			EndState:        classifyEndState(row),
			// hermes has no concept of compaction in its schema.
			Compaction: nil,
			// A session with no working directory did not originate in a
			// place LazyRecall can return the user to (spec session-index /
			// session-resume): chat-platform-origin sessions land here.
			Resumable: row.CWD != nil && *row.CWD != "",
		}
		if row.StartedAt > 0 {
			t := time.Unix(int64(row.StartedAt), 0)
			s.StartedAt = &t
		}
		if row.LastActivityAt != nil {
			t := time.Unix(int64(*row.LastActivityAt), 0)
			s.LastActivityAt = &t
		} else if row.EndedAt != nil {
			t := time.Unix(int64(*row.EndedAt), 0)
			s.LastActivityAt = &t
		} else if s.StartedAt != nil {
			s.LastActivityAt = s.StartedAt
		}

		out = append(out, adapter.Discovered{Session: s}) // TranscriptPath left empty: no transcripts exist
	}
	return out, nil
}

// classifyEndState maps hermes's own last-message signal onto LazyRecall's
// closed end-state set. hermes's end_reason column describes why the host
// process exited (cli_close, tui_shutdown, ...), which does not reliably
// indicate whether the conversation itself was left mid-turn - the same
// end_reason value is observed after both clean and dangling final
// exchanges. The last message's role/finish_reason is the more direct,
// source-recorded signal of conversational completion, so it is used
// instead - still "taken from the source," not inferred from timestamps.
func classifyEndState(row sessionRow) session.EndState {
	if row.LastMsgRole == nil {
		return session.EndStateUnknown
	}
	switch *row.LastMsgRole {
	case "user":
		return session.EndStateDangling
	case "assistant":
		if row.LastMsgHasToolCalls != nil && *row.LastMsgHasToolCalls != 0 {
			return session.EndStateInterrupted
		}
		if row.LastMsgFinish == nil {
			return session.EndStateUnknown
		}
		switch *row.LastMsgFinish {
		case "stop":
			return session.EndStateCompleted
		case "tool_calls":
			return session.EndStateInterrupted
		case "verification_required":
			return session.EndStateInterrupted
		default:
			return session.EndStateUnknown
		}
	case "tool":
		// A tool result was recorded last with no assistant turn to follow
		// it up yet - not enough on its own to say more.
		return session.EndStateUnknown
	default:
		return session.EndStateUnknown
	}
}

// PromptEntry is one tier-2 prompt entry read from hermes's messages table.
type PromptEntry struct {
	SessionID string
	Text      string
	At        *time.Time
}

// PromptsSince queries hermes's messages table for user-role rows with id
// greater than fromID, read-only (task 5.5, "prompts from messages with
// role user"; task 6.1's monotonic-key cursor for database sources).
func (a *Adapter) PromptsSince(p profile.Profile, fromID int64) ([]PromptEntry, int64, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, fromID, nil
	}
	dbPath := filepath.Join(root, "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fromID, nil
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	var rows []struct {
		ID        int64   `json:"id"`
		SessionID string  `json:"session_id"`
		Content   *string `json:"content"`
		Timestamp float64 `json:"timestamp"`
	}
	q := fmt.Sprintf(`SELECT id, session_id, content, timestamp FROM messages WHERE role = 'user' AND id > %d ORDER BY id;`, fromID)
	if err := r.Query(q, &rows); err != nil {
		return nil, fromID, fmt.Errorf("hermes: querying messages: %w", err)
	}

	newCursor := fromID
	out := make([]PromptEntry, 0, len(rows))
	for _, row := range rows {
		if row.Content == nil || *row.Content == "" {
			continue
		}
		at := time.Unix(int64(row.Timestamp), 0)
		out = append(out, PromptEntry{SessionID: row.SessionID, Text: *row.Content, At: &at})
		if row.ID > newCursor {
			newCursor = row.ID
		}
	}
	return out, newCursor, nil
}

// Conversation reads one session's exchange back out of hermes's messages
// table (adapter.ConversationReader). hermes writes no transcript file, so
// these rows are the conversation.
//
// Its shape is flatter than the block-structured sources: content is plain
// text in a column, and a tool call is an assistant row carrying tool_calls
// JSON. The "tool" role is the result coming back and is skipped, the same
// way a tool_result record is skipped in a Claude transcript - it carries
// no line a reader wants.
func (a *Adapter) Conversation(p profile.Profile, sourceSessionID string, limits transcript.ConversationLimits) ([]transcript.Turn, int, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, 0, nil
	}
	dbPath := filepath.Join(root, "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, 0, nil
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	pf, err := r.WriteParams(map[string]any{"sid": sourceSessionID})
	if err != nil {
		return nil, 0, fmt.Errorf("hermes: binding session id: %w", err)
	}
	defer pf.Close()

	var rows []struct {
		Role      string  `json:"role"`
		Content   *string `json:"content"`
		ToolName  *string `json:"tool_name"`
		ToolCalls *string `json:"tool_calls"`
		Timestamp float64 `json:"timestamp"`
	}
	q := `SELECT role, content, tool_name, tool_calls, timestamp FROM messages WHERE session_id = ` +
		pf.Ref("sid") + ` ORDER BY timestamp, id;`
	if err := r.Query(q, &rows); err != nil {
		return nil, 0, fmt.Errorf("hermes: querying messages: %w", err)
	}

	var turns []transcript.Turn
	for _, row := range rows {
		at := time.Unix(int64(row.Timestamp), 0)
		text := ""
		if row.Content != nil {
			text = *row.Content
		}
		switch row.Role {
		case "user":
			turns = append(turns, transcript.Turn{Kind: transcript.KindUserPrompt, Text: text, At: &at})
		case "assistant":
			// An assistant row can carry both a sentence and the tool calls
			// that followed it, so it can produce two turns: what was said,
			// then what was done. Dropping either would misreport the turn.
			if text != "" {
				turns = append(turns, transcript.Turn{Kind: transcript.KindAssistantText, Text: text, At: &at})
			}
			if row.ToolCalls != nil && *row.ToolCalls != "" {
				turn := transcript.Turn{Kind: transcript.KindToolUse, At: &at}
				turn.Tool = hermesToolNames(*row.ToolCalls)
				turns = append(turns, turn)
			}
		}
	}
	return transcript.LimitTurns(turns, limits)
}

// hermesToolNames pulls the called tools' names out of an assistant row's
// tool_calls JSON. The column holds the provider's own tool-call array, so
// the name sits at either "name" or, in the OpenAI-shaped form hermes
// stores for most providers, "function.name"; both are read because a row
// written by either shape has to render with a name rather than without.
func hermesToolNames(toolCalls string) []string {
	var calls []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(toolCalls), &calls); err != nil {
		return nil
	}
	var out []string
	for _, c := range calls {
		name := c.Name
		if name == "" {
			name = c.Function.Name
		}
		if name == "" {
			continue
		}
		seen := false
		for _, e := range out {
			if e == name {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, name)
		}
	}
	return out
}
