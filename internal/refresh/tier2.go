package refresh

import (
	"strconv"
	"time"

	"github.com/rfist/lazyrecall/internal/adapter"
	claudeadapter "github.com/rfist/lazyrecall/internal/adapter/claude"
	hermesadapter "github.com/rfist/lazyrecall/internal/adapter/hermes"
	ompadapter "github.com/rfist/lazyrecall/internal/adapter/omp"
	"github.com/rfist/lazyrecall/internal/transcript"
)

// tier2ForSource ingests one source's prompts for the search index (task
// 6.4). It prefers the source's own prompt index (claude's history.jsonl,
// omp's history table, hermes's messages table) and falls back to
// transcript-extracted prompts, already collected during this pass's tier-1
// scan, for any session that index doesn't cover - most notably pi, which
// has no prompt index at all.
func (r *Refresher) tier2ForSource(
	sourceName string,
	cursors map[string]cursorRow,
	discovered []adapter.Discovered,
	fallback map[string][]transcript.PromptText,
) ([]map[string]any, []map[string]any, error) {
	var promptRecords []map[string]any
	var cursorRecords []map[string]any
	covered := map[string]bool{} // lazyrecall session id -> got at least one prompt from the primary index

	switch sourceName {
	case "claude":
		if root := r.Profile.Roots["claude"]; root != "" {
			from := int64(0)
			if c, ok := cursors[cursorKey("claude", "*")]; ok && c.ByteOffset != nil {
				from = *c.ByteOffset
			}
			prompts, newOffset, err := claudeadapter.PromptsSince(root, from)
			if err == nil {
				for _, p := range prompts {
					sid := "claude:" + r.Profile.Name + ":" + p.SessionID
					promptRecords = append(promptRecords, promptRecord(sid, p.Text))
					covered[sid] = true
				}
				cursorRecords = append(cursorRecords, sourceCursorRecord("claude", newOffset))
			}
		}

	case "omp":
		if r.Profile.Roots["omp"] != "" {
			from := int64(0)
			if c, ok := cursors[cursorKey("omp", "*")]; ok && c.DBCursorKey != nil {
				from = parseInt64(*c.DBCursorKey)
			}
			a := ompadapter.New(r.SQLite3Path)
			prompts, newCursor, err := a.PromptsSince(r.Profile, from)
			if err == nil {
				for _, p := range prompts {
					sid := "omp:" + r.Profile.Name + ":" + p.SessionID
					promptRecords = append(promptRecords, promptRecord(sid, p.Text))
					covered[sid] = true
				}
				cursorRecords = append(cursorRecords, sourceCursorRecordDBKey("omp", newCursor))
			}
		}

	case "hermes":
		if r.Profile.Roots["hermes"] != "" {
			from := int64(0)
			if c, ok := cursors[cursorKey("hermes", "*")]; ok && c.DBCursorKey != nil {
				from = parseInt64(*c.DBCursorKey)
			}
			a := hermesadapter.New(r.SQLite3Path)
			prompts, newCursor, err := a.PromptsSince(r.Profile, from)
			if err == nil {
				for _, p := range prompts {
					sid := "hermes:" + r.Profile.Name + ":" + p.SessionID
					promptRecords = append(promptRecords, promptRecord(sid, p.Text))
					covered[sid] = true
				}
				cursorRecords = append(cursorRecords, sourceCursorRecordDBKey("hermes", newCursor))
			}
		}

	case "pi":
		// No source index at all: every prompt comes from the fallback
		// (task 5.4).
	}

	// Fallback: any discovered session from this source with no prompt
	// this pass, but with prompts already extracted from its transcript
	// during the tier-1 scan, uses those (task 6.4's incomplete-index
	// scenario; the only path for pi).
	existingCoverage, _ := r.sessionsWithPrompts()
	for _, d := range discovered {
		sid := d.Session.ID
		if covered[sid] || existingCoverage[sid] {
			continue
		}
		for _, p := range fallback[sid] {
			promptRecords = append(promptRecords, promptRecord(sid, p.Text))
		}
	}

	return promptRecords, cursorRecords, nil
}

func promptRecord(sessionID, text string) map[string]any {
	return map[string]any{"session_id": sessionID, "kind": "prompt", "text": text}
}

func sourceCursorRecord(source string, byteOffset int64) map[string]any {
	return map[string]any{
		"source": source, "source_id": "*", "kind": "transcript_offset",
		"byte_offset": byteOffset, "updated_at": time.Now().Unix(),
	}
}

func sourceCursorRecordDBKey(source string, key int64) map[string]any {
	return map[string]any{
		"source": source, "source_id": "*", "kind": "db_key",
		"db_cursor_key": strconv.FormatInt(key, 10), "updated_at": time.Now().Unix(),
	}
}

func (r *Refresher) sessionsWithPrompts() (map[string]bool, error) {
	var rows []struct {
		SessionID string `json:"session_id"`
	}
	if err := r.DB.Query(`SELECT DISTINCT session_id FROM prompt_fts WHERE kind = 'prompt';`, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.SessionID] = true
	}
	return out, nil
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
