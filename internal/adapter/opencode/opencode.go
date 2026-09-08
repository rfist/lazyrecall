// Package opencode adapts OpenCode's on-disk session data: everything lives
// in SQLite at ~/.local/share/opencode/opencode.db, no transcript files
// exist at all - the same DB-only shape hermes established (task 5.5's
// design applied to a new source). A newer session_message/event schema
// exists in the same database from a Drizzle migration but is unpopulated
// on real installs; this adapter reads the legacy session/message/part
// tables, which is what OpenCode is actually writing to. Message and part
// payloads are JSON blob columns (`data`); this adapter pulls fields out of
// them with SQLite's json_extract rather than decoding blobs in Go, so the
// shaping stays in SQL the way every other adapter's queries do.
//
// OpenCode records an explicit per-turn completion signal (message.data
// $.finish: "stop"/"tool-calls"/"error"/"aborted"), unlike hermes's
// finish_reason-or-nothing and unlike goose, which records no completion
// signal at all - classifyEndState here is the most direct of the three new
// adapters', "taken from the source" rather than inferred.
package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rfist/lazyrecall/internal/adapter"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

type Adapter struct {
	SQLite3Path string
}

func New(sqlite3Path string) *Adapter { return &Adapter{SQLite3Path: sqlite3Path} }

func (*Adapter) Name() string { return "opencode" }

type sessionRow struct {
	ID            string  `json:"id"`
	ParentID      *string `json:"parent_id"`
	Directory     *string `json:"directory"`
	Title         *string `json:"title"`
	TimeCreated   int64   `json:"time_created"`
	TimeUpdated   *int64  `json:"time_updated"`
	MessageCount  *int64  `json:"message_count"`
	LastMsgRole   *string `json:"last_msg_role"`
	LastMsgFinish *string `json:"last_msg_finish"`
}

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no opencode root for this profile"}
	}
	dbPath := filepath.Join(root, "opencode.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, &adapter.Unavailable{Reason: "no opencode database on this machine"}
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	// The last message per session (role, finish) is joined in via SQLite's
	// json1 extension so end-state classification can use OpenCode's own
	// recorded per-turn completion signal, the same pattern hermes's
	// last_msg CTE uses for its own role/finish_reason columns.
	const q = `
WITH last_msg AS (
  SELECT session_id,
         json_extract(data, '$.role') AS role,
         json_extract(data, '$.finish') AS finish,
         ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY time_created DESC) AS rn
  FROM message
)
SELECT
  s.id, s.parent_id, s.directory, s.title, s.time_created, s.time_updated,
  (SELECT count(*) FROM message m WHERE m.session_id = s.id) AS message_count,
  lm.role AS last_msg_role, lm.finish AS last_msg_finish
FROM session s
LEFT JOIN last_msg lm ON lm.session_id = s.id AND lm.rn = 1;`

	var rows []sessionRow
	if err := r.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("opencode: querying opencode.db: %w", err)
	}

	out := make([]adapter.Discovered, 0, len(rows))
	for _, row := range rows {
		s := session.Session{
			ID:              "opencode:" + p.Name + ":" + row.ID,
			Source:          "opencode",
			Profile:         p.Name,
			SourceSessionID: row.ID,
			Topic:           nonEmpty(row.Title),
			ContinuesFrom:   row.ParentID,
			CWD:             row.Directory,
			MessageCount:    row.MessageCount,
			EndState:        classifyEndState(row),
			// OpenCode has a time_compacting timestamp, but it is a single
			// nullable marker, not the richer per-event log
			// session.Compaction expects (count plus automatic/dropped-token
			// detail claude and omp's transcripts provide) - mapping one
			// onto the other would invent detail the source does not
			// record, so this is left nil rather than guessed.
			Compaction: nil,
			Resumable:  row.Directory != nil && *row.Directory != "",
		}
		s.StartedAt = epochMillis(row.TimeCreated)
		if row.TimeUpdated != nil {
			s.LastActivityAt = epochMillis(*row.TimeUpdated)
		} else {
			s.LastActivityAt = s.StartedAt
		}
		out = append(out, adapter.Discovered{Session: s}) // TranscriptPath left empty: no transcripts exist
	}
	return out, nil
}

func nonEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

func epochMillis(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms)
	return &t
}

// classifyEndState maps OpenCode's own last-message role/finish signal onto
// LazyRecall's closed end-state set. "error" and "aborted" both describe a
// turn that ran to completion badly rather than one still waiting on a tool
// result - EndStateInterrupted's own doc comment scopes it to "initiated a
// tool operation for which no result was ever recorded", which neither
// value is - so both map to EndStateUnknown rather than being folded into
// Interrupted on the strength of the name alone.
func classifyEndState(row sessionRow) session.EndState {
	if row.LastMsgRole == nil {
		return session.EndStateUnknown
	}
	switch *row.LastMsgRole {
	case "user":
		return session.EndStateDangling
	case "assistant":
		if row.LastMsgFinish == nil {
			return session.EndStateUnknown
		}
		switch *row.LastMsgFinish {
		case "stop":
			return session.EndStateCompleted
		case "tool-calls":
			return session.EndStateInterrupted
		default: // "error", "aborted", or any value this build has not seen
			return session.EndStateUnknown
		}
	default:
		return session.EndStateUnknown
	}
}

// PromptEntry is one tier-2 prompt entry read from OpenCode's part table.
type PromptEntry struct {
	SessionID string
	Text      string
	At        *time.Time
}

// PromptsSince queries OpenCode's part table for text-typed parts of
// user-role messages with rowid greater than fromID, read-only. part.id is
// a text primary key (OpenCode's own id scheme, not necessarily numeric or
// sortable), so the cursor uses SQLite's always-present integer rowid
// instead - part carries no WITHOUT ROWID clause, confirmed by reading its
// schema, so this is safe.
func (a *Adapter) PromptsSince(p profile.Profile, fromID int64) ([]PromptEntry, int64, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, fromID, nil
	}
	dbPath := filepath.Join(root, "opencode.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fromID, nil
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	var rows []struct {
		RowID     int64   `json:"rowid"`
		SessionID string  `json:"session_id"`
		Text      *string `json:"text"`
		Timestamp *int64  `json:"time_created"`
	}
	q := fmt.Sprintf(`
SELECT p.rowid AS rowid, p.session_id AS session_id,
       json_extract(p.data, '$.text') AS text, p.time_created AS time_created
FROM part p
JOIN message m ON m.id = p.message_id
WHERE json_extract(m.data, '$.role') = 'user'
  AND json_extract(p.data, '$.type') = 'text'
  AND p.rowid > %d
ORDER BY p.rowid;`, fromID)
	if err := r.Query(q, &rows); err != nil {
		return nil, fromID, fmt.Errorf("opencode: querying part: %w", err)
	}

	newCursor := fromID
	out := make([]PromptEntry, 0, len(rows))
	for _, row := range rows {
		if row.Text == nil || *row.Text == "" {
			continue
		}
		var at *time.Time
		if row.Timestamp != nil {
			at = epochMillis(*row.Timestamp)
		}
		out = append(out, PromptEntry{SessionID: row.SessionID, Text: *row.Text, At: at})
		if row.RowID > newCursor {
			newCursor = row.RowID
		}
	}
	return out, newCursor, nil
}
