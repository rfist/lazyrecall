// Package antigravity adapts the Antigravity CLI's (`agy`) on-disk session
// data - the standalone coding-agent binary, not to be confused with
// Antigravity.app, the separate VS Code-fork IDE that shares the brand.
// agy's own conversation listing lives at
// ~/.gemini/antigravity-cli/conversation_summaries.db, one clean SQLite
// table with a real topic (title/preview, plain text) and a working
// directory (workspace_uris) - the DB-only shape hermes established, and
// simpler than either: everything this adapter needs is already in one
// table, with no per-conversation JOIN required.
//
// What this adapter deliberately does NOT do: read per-conversation detail
// (~/.gemini/antigravity-cli/conversations/<id>.db). That store's actual
// turn content lives in a `steps.step_payload` BLOB column whose first
// bytes are protobuf wire-format framing (confirmed by hex-dumping a real
// row before writing this file, not assumed) - no .proto schema is
// available to decode it, so tier-2 full-text prompt search is not
// possible for this source (no PromptsSince), and end-state classification
// has weaker signal than goose or OpenCode get: conversation_summaries'
// own status/source/agent_name columns were empty on every row on this
// machine (verified directly, not assumed absent), so this adapter reports
// EndStateUnknown rather than inventing a heuristic the data does not
// support - see classifyEndState.
package antigravity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func (*Adapter) Name() string { return "antigravity" }

type conversationRow struct {
	ConversationID       string     `json:"conversation_id"`
	Title                *string    `json:"title"`
	Preview              *string    `json:"preview"`
	StepCount            *int64     `json:"step_count"`
	LastModifiedTime     *time.Time `json:"last_modified_time"`
	WorkspaceURIs        *string    `json:"workspace_uris"`
	ParentConversationID *string    `json:"parent_conversation_id"`
	Killed               int64      `json:"killed"`
}

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.Roots[a.Name()]
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no antigravity root for this profile"}
	}
	dbPath := filepath.Join(root, "conversation_summaries.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, &adapter.Unavailable{Reason: "no antigravity conversation_summaries.db on this machine"}
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	// No JOIN and no last_msg CTE, unlike goose/opencode/hermes: every
	// field this adapter needs is already a column on this one table, and
	// the per-conversation detail store that would otherwise justify a
	// join is the protobuf one described in the package doc.
	const q = `
SELECT conversation_id, title, preview, step_count, last_modified_time,
       workspace_uris, parent_conversation_id, killed
FROM conversation_summaries;`

	var rows []conversationRow
	if err := r.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("antigravity: querying conversation_summaries.db: %w", err)
	}

	out := make([]adapter.Discovered, 0, len(rows))
	for _, row := range rows {
		cwd := firstWorkspaceDir(row.WorkspaceURIs)
		s := session.Session{
			ID:              "antigravity:" + p.Name + ":" + row.ConversationID,
			Source:          "antigravity",
			Profile:         p.Name,
			SourceSessionID: row.ConversationID,
			Topic:           topicFor(row),
			ContinuesFrom:   nonEmpty(row.ParentConversationID),
			CWD:             cwd,
			MessageCount:    row.StepCount,
			EndState:        classifyEndState(row),
			// No compaction concept in this schema.
			Compaction: nil,
			// Tied to CWD, like every other adapter: session.Resumable's
			// own doc is "false for sessions that have no working
			// directory to resume into", not "true because this source
			// happens to always record one" - workspace_uris was
			// non-empty on every real conversation observed, but that is
			// an empirical fact about this machine's data, not a
			// guarantee, so it is still checked rather than hardcoded.
			Resumable: cwd != nil && *cwd != "",
			// No StartedAt: this schema has no creation-time column at
			// all (confirmed against the real .schema output) - only
			// last_modified_time and last_user_input_time, both activity
			// markers, not a start time. Left nil rather than picking
			// one of them and calling it something it is not.
			LastActivityAt: validTime(row.LastModifiedTime),
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

// validTime guards against a real defect found on this machine while
// verifying this adapter against actual data (not a hypothetical): one
// conversation's last_modified_time was the literal Go zero time,
// "0001-01-01 00:00:00+00:00", stored as text rather than left NULL -
// surfaced as a session "106751 days ago" until this guard was added. A
// year before Unix time began is not a real activity timestamp from any
// agy version; treated as absent, the same as if the column were NULL.
func validTime(t *time.Time) *time.Time {
	if t == nil || t.Year() < 1970 {
		return nil
	}
	return t
}

// topicFor prefers the conversation's title, falling back to its preview -
// both are plain text on this schema (confirmed: neither is the blob
// column the per-conversation detail store uses), so this needs no
// protobuf decoding.
func topicFor(row conversationRow) *string {
	if row.Title != nil && *row.Title != "" {
		return row.Title
	}
	if row.Preview != nil && *row.Preview != "" {
		return row.Preview
	}
	return nil
}

// firstWorkspaceDir extracts the first directory from workspace_uris, a
// JSON array of file:// URIs (confirmed sample:
// ["file:///Users/x/project"]) - takes the first element and strips the
// scheme; a session with more than one workspace URI is rare enough on a
// coding-agent CLI that the first is a reasonable single CWD, matching how
// every other adapter reports exactly one working directory.
func firstWorkspaceDir(raw *string) *string {
	if raw == nil || *raw == "" {
		return nil
	}
	var uris []string
	if err := json.Unmarshal([]byte(*raw), &uris); err != nil || len(uris) == 0 {
		return nil
	}
	dir := strings.TrimPrefix(uris[0], "file://")
	if dir == "" {
		return nil
	}
	return &dir
}

// classifyEndState is deliberately the simplest of the three new adapters'.
// conversation_summaries' status/source/agent_name columns exist but were
// empty on every row on this machine (queried directly before writing
// this, not assumed) - this agy version does not appear to populate them
// yet, so there is no per-message completion signal to read the way
// goose's content-block types or OpenCode's `finish` field provide.
// killed is the one unambiguous signal present: a conversation whose
// process was force-terminated did not end on its own terms, which is
// closer to EndStateInterrupted's "did not run to completion" than to
// Completed or Unknown - unlike OpenCode's ambiguous "error"/"aborted"
// strings, "killed" carries no alternate reading worth hedging against.
// Everything else reports Unknown rather than guessing from step_count or
// recency, which spec session-review already rules out ("never guessed
// from file modification times alone").
func classifyEndState(row conversationRow) session.EndState {
	if row.Killed != 0 {
		return session.EndStateInterrupted
	}
	return session.EndStateUnknown
}
