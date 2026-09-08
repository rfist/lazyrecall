// Package goose adapts Goose's on-disk session data: everything lives in
// SQLite at ~/.local/share/goose/sessions/sessions.db, no transcript files
// exist at all - the DB-only shape hermes established (task 5.5's design
// applied to a new source).
//
// goose.sessions.session_type distinguishes real conversations ("user")
// from ones an editor drove over ACP ("acp") from goose's own internal
// bookkeeping ("hidden") from a cron-style run ("scheduled"). On this
// machine over half of all recorded sessions are "acp" - confirmed by
// querying the real database before writing this file, not assumed - so
// filtering them out the way "hidden" is filtered would silently drop most
// of what a goose-in-an-editor user would come here to find. "acp" instead
// becomes the session's Client value, the same field and the same string
// Claude Code's own editor-driven sessions already use (internal/session's
// clientLabels - "acp" needs no new label entry, an unmapped value already
// displays as itself). "scheduled" sessions are kept too: an automated run
// is still a session worth showing, the same way lazyrecall already shows
// claude sessions run non-interactively. Only "hidden" is excluded - it is
// goose's own internal bookkeeping, not a conversation a person had.
//
// Every user turn's tool results are recorded as their own message with
// role "user" carrying a toolResponse content block, not under role
// "assistant" - confirmed by querying which role each block type actually
// appears under on this machine, not assumed from the schema alone. So
// end-state classification (see classifyEndState) keys off the last
// message's content-block type, never off role alone: a last message with
// role "user" can be a person's own dangling prompt, or it can be a tool
// result still awaiting the agent's next turn, and those are different
// situations.
package goose

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
)

type Adapter struct {
	SQLite3Path string
}

func New(sqlite3Path string) *Adapter { return &Adapter{SQLite3Path: sqlite3Path} }

func (*Adapter) Name() string { return "goose" }

type sessionRow struct {
	ID              string     `json:"id"`
	Name            *string    `json:"name"`
	UserSetName     int64      `json:"user_set_name"`
	SessionType     *string    `json:"session_type"`
	WorkingDir      *string    `json:"working_dir"`
	CreatedAt       *time.Time `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
	ParentSessionID *string    `json:"parent_session_id"`
	MessageCount    *int64     `json:"message_count"`
	LastMsgRole     *string    `json:"last_msg_role"`
	LastMsgContent  *string    `json:"last_msg_content_json"`
}

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no goose root for this profile"}
	}
	dbPath := filepath.Join(root, "sessions.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, &adapter.Unavailable{Reason: "no goose database on this machine"}
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	// hidden sessions are goose's own bookkeeping (see package doc) and are
	// excluded here, at the source, rather than filtered later - the same
	// place a source-recorded exclusion belongs everywhere else in this
	// codebase (e.g. Claude's injected-block filtering happens in the
	// vocab, not downstream of it). archived_at is unused on every real
	// session on this machine but is filtered defensively anyway: a
	// session goose itself considers archived should not resurface.
	const q = `
WITH last_msg AS (
  SELECT session_id, role, content_json,
         ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY id DESC) AS rn
  FROM messages
)
SELECT
  s.id, s.name, s.user_set_name, s.session_type, s.working_dir,
  s.created_at, s.updated_at, s.parent_session_id,
  (SELECT count(*) FROM messages m WHERE m.session_id = s.id) AS message_count,
  lm.role AS last_msg_role, lm.content_json AS last_msg_content_json
FROM sessions s
LEFT JOIN last_msg lm ON lm.session_id = s.id AND lm.rn = 1
WHERE s.session_type != 'hidden' AND s.archived_at IS NULL;`

	var rows []sessionRow
	if err := r.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("goose: querying sessions.db: %w", err)
	}

	out := make([]adapter.Discovered, 0, len(rows))
	for _, row := range rows {
		s := session.Session{
			ID:              "goose:" + p.Name + ":" + row.ID,
			Source:          "goose",
			Profile:         p.Name,
			SourceSessionID: row.ID,
			Topic:           topicFor(row),
			Client:          clientFor(row),
			ContinuesFrom:   row.ParentSessionID,
			CWD:             row.WorkingDir,
			MessageCount:    row.MessageCount,
			EndState:        classifyEndState(row),
			Compaction:      nil, // goose's schema has no compaction concept
			Resumable:       row.WorkingDir != nil && *row.WorkingDir != "",
		}
		s.StartedAt = row.CreatedAt
		if row.UpdatedAt != nil {
			s.LastActivityAt = row.UpdatedAt
		} else {
			s.LastActivityAt = s.StartedAt
		}
		out = append(out, adapter.Discovered{Session: s}) // TranscriptPath left empty: no transcripts exist
	}
	return out, nil
}

// topicFor uses the session's name only when the user actually set it
// (user_set_name) - goose's own default names are session-id-derived
// placeholders, confirmed by querying real data before writing this: 108
// of 109 sessions on this machine have user_set_name = 0, and are not worth
// showing as a topic.
func topicFor(row sessionRow) *string {
	if row.UserSetName == 0 || row.Name == nil || *row.Name == "" {
		return nil
	}
	return row.Name
}

// clientFor reports session_type as the Client value for the two types
// that mean "driven through something other than goose's own terminal" -
// see the package doc for why "acp" needs no new label mapping. "user" (the
// ordinary case) and "hidden" (already excluded before this runs) report no
// client, matching how Claude's own terminal sessions report none.
func clientFor(row sessionRow) *string {
	if row.SessionType == nil {
		return nil
	}
	switch *row.SessionType {
	case "acp", "scheduled":
		return row.SessionType
	default:
		return nil
	}
}

// contentBlock is the shape of one entry in a goose message's content_json
// array - only the discriminant this adapter needs.
type contentBlock struct {
	Type string `json:"type"`
}

// lastBlockType returns the type of the last block in a content_json array,
// or "" if it cannot be parsed or is empty. Order matters here, not just
// presence: a message's blocks are read in the order goose recorded them,
// and only the last one describes how the message - and so, when it is
// also the session's last message, the session - actually left off.
func lastBlockType(contentJSON *string) string {
	if contentJSON == nil {
		return ""
	}
	var blocks []contentBlock
	if err := json.Unmarshal([]byte(*contentJSON), &blocks); err != nil || len(blocks) == 0 {
		return ""
	}
	return blocks[len(blocks)-1].Type
}

// classifyEndState maps goose's last message onto LazyRecall's closed
// end-state set. goose records no explicit turn-completion signal the way
// OpenCode's `finish` field or hermes's finish_reason column do, so this is
// inferred from message shape - but from the last content BLOCK's type,
// never from role alone: a toolResponse block rides a message with role
// "user" (confirmed against real data - see the package doc), so "last
// message has role user" is not the same fact as "a person's prompt is
// sitting there unanswered". Only a last block of type "text" under role
// "user" is a genuine dangling human prompt.
func classifyEndState(row sessionRow) session.EndState {
	if row.LastMsgRole == nil {
		return session.EndStateUnknown
	}
	last := lastBlockType(row.LastMsgContent)
	switch *row.LastMsgRole {
	case "user":
		switch last {
		case "text":
			return session.EndStateDangling
		case "toolResponse":
			// A tool result came back with no assistant turn to follow it
			// up yet - the same "not enough on its own to say more" case
			// hermes's own bare-tool-result branch leaves as Unknown.
			return session.EndStateUnknown
		default:
			return session.EndStateUnknown
		}
	case "assistant":
		switch last {
		case "text":
			return session.EndStateCompleted
		case "toolRequest":
			return session.EndStateInterrupted
		default:
			return session.EndStateUnknown
		}
	default:
		return session.EndStateUnknown
	}
}

// PromptEntry is one tier-2 prompt entry read from goose's messages table.
type PromptEntry struct {
	SessionID string
	Text      string
	At        *time.Time
}

// PromptsSince queries goose's messages table for user-role rows with id
// greater than fromID, read-only, extracting only "text"-typed blocks - a
// user message's content_json can also carry a toolResponse block (see the
// package doc), which is not something the user typed and must not be
// indexed as if it were.
func (a *Adapter) PromptsSince(p profile.Profile, fromID int64) ([]PromptEntry, int64, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, fromID, nil
	}
	dbPath := filepath.Join(root, "sessions.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fromID, nil
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	var rows []struct {
		ID          int64  `json:"id"`
		SessionID   string `json:"session_id"`
		ContentJSON string `json:"content_json"`
		Timestamp   int64  `json:"created_timestamp"`
	}
	q := fmt.Sprintf(`SELECT id, session_id, content_json, created_timestamp FROM messages WHERE role = 'user' AND id > %d ORDER BY id;`, fromID)
	if err := r.Query(q, &rows); err != nil {
		return nil, fromID, fmt.Errorf("goose: querying messages: %w", err)
	}

	newCursor := fromID
	out := make([]PromptEntry, 0, len(rows))
	for _, row := range rows {
		text := textBlocks(row.ContentJSON)
		if text != "" {
			at := time.Unix(row.Timestamp, 0)
			out = append(out, PromptEntry{SessionID: row.SessionID, Text: text, At: &at})
		}
		if row.ID > newCursor {
			newCursor = row.ID
		}
	}
	return out, newCursor, nil
}

// textBlocks concatenates every "text"-typed block in a content_json array,
// skipping toolResponse and any other block type - a user message can in
// principle carry more than one text block plus a non-text one, and only
// the text is a prompt.
func textBlocks(contentJSON string) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &blocks); err != nil {
		return ""
	}
	out := ""
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += b.Text
	}
	return out
}
