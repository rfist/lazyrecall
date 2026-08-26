// Package omp adapts omp's on-disk session data: a SQLite index at
// ~/.omp/agent/history.db (table history + FTS5 history_fts) and one
// transcript per session under sessions/<encoded-cwd>/<ts>_<uuid>.jsonl
// (design.md context; task 5.3). Enumeration walks the transcript
// directory the same way the claude and pi adapters do; history.db is used
// only for tier 2 (its own prompt index), read read-only through sqlitex so
// omp's active writer is never disturbed.
package omp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"recall/internal/adapter"
	"recall/internal/profile"
	"recall/internal/session"
	"recall/internal/sqlitex"
)

type Adapter struct {
	SQLite3Path string
}

func New(sqlite3Path string) *Adapter { return &Adapter{SQLite3Path: sqlite3Path} }

func (*Adapter) Name() string { return "omp" }

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.OmpRoot
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no omp root for this profile"}
	}
	sessionsDir := filepath.Join(root, "agent", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &adapter.Unavailable{Reason: "no omp session data on this machine"}
		}
		return nil, fmt.Errorf("omp: listing %s: %w", sessionsDir, err)
	}

	var out []adapter.Discovered
	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, dirEntry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			fi, err := f.Info()
			if err != nil {
				continue
			}
			s := session.Session{
				ID:              "omp:" + p.Name + ":" + id,
				Source:          "omp",
				Profile:         p.Name,
				SourceSessionID: id,
				EndState:        session.EndStateUnknown,
				Resumable:       true,
			}
			out = append(out, adapter.Discovered{
				Session:        s,
				TranscriptPath: filepath.Join(dir, f.Name()),
				FileSize:       fi.Size(),
			})
		}
	}
	return out, nil
}

// HistoryPrompt is one tier-2 prompt entry read from omp's own history
// table.
type HistoryPrompt struct {
	SessionID string
	Text      string
	At        *time.Time
}

// PromptsSince queries omp's history table for every row with id greater
// than fromID, read-only, and returns the new high-water id (task 6.1's
// "monotonic key for database sources"; task 6.4).
func (a *Adapter) PromptsSince(p profile.Profile, fromID int64) ([]HistoryPrompt, int64, error) {
	if p.OmpRoot == "" {
		return nil, fromID, nil
	}
	dbPath := filepath.Join(p.OmpRoot, "agent", "history.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fromID, nil
	}
	r := &sqlitex.Runner{BinPath: a.SQLite3Path, DBPath: dbPath, ReadOnly: true}

	var rows []struct {
		ID        int64  `json:"id"`
		Prompt    string `json:"prompt"`
		SessionID string `json:"session_id"`
		CreatedAt int64  `json:"created_at"`
	}
	q := fmt.Sprintf(`SELECT id, prompt, session_id, created_at FROM history WHERE id > %d ORDER BY id;`, fromID)
	if err := r.Query(q, &rows); err != nil {
		return nil, fromID, fmt.Errorf("omp: querying history.db: %w", err)
	}

	newCursor := fromID
	out := make([]HistoryPrompt, 0, len(rows))
	for _, row := range rows {
		if row.SessionID == "" || row.Prompt == "" {
			continue
		}
		at := time.Unix(row.CreatedAt, 0)
		out = append(out, HistoryPrompt{SessionID: row.SessionID, Text: row.Prompt, At: &at})
		if row.ID > newCursor {
			newCursor = row.ID
		}
	}
	return out, newCursor, nil
}
