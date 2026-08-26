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
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lazyrecall/internal/adapter"
	"lazyrecall/internal/profile"
	"lazyrecall/internal/session"
	"lazyrecall/internal/sqlitex"
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
