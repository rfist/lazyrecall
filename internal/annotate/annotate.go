// Package annotate is the user's own layer on top of indexed sessions:
// comments and tags, held separately from every source and never written
// back to one (spec session-annotations). Everything here operates on a
// lineage id, not a source session id, so an annotation survives
// compaction, continuation under a new identifier, and re-indexing
// (design.md decision 8) - internal/refresh is what keeps lineage ids
// stable and marks a lineage orphaned when its sessions vanish from their
// source (task 9.3/9.4); this package just reads and writes comments/tags
// against whatever lineage id a session currently resolves to.
package annotate

import (
	"fmt"
	"time"

	"lazyrecall/internal/sqlitex"
)

// Comment is one free-text comment attached to a lineage (spec
// session-annotations, "Comments on sessions").
type Comment struct {
	ID        int64
	LineageID string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// LineageForSession resolves a session id to the lineage id annotations
// should attach to.
func LineageForSession(db *sqlitex.Runner, sessionID string) (string, error) {
	pf, err := db.WriteParams(map[string]any{"id": sessionID})
	if err != nil {
		return "", err
	}
	defer pf.Close()

	var rows []struct {
		LineageID string `json:"lineage_id"`
	}
	q := "SELECT lineage_id FROM sessions WHERE id = " + pf.Ref("id") + ";"
	if err := db.Query(q, &rows); err != nil {
		return "", fmt.Errorf("annotate: resolving session %s: %w", sessionID, err)
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("annotate: no such session %q", sessionID)
	}
	return rows[0].LineageID, nil
}

// AddComment attaches a new comment to lineageID (spec session-annotations,
// "Adding a comment"). The comment is never written into any session
// source - it exists only in LazyRecall's own database.
func AddComment(db *sqlitex.Runner, lineageID, body string) error {
	now := time.Now().Unix()
	b := db.NewBatch()
	if err := b.BulkInsert("comments", []string{"lineage_id", "body", "created_at", "updated_at"}, []map[string]any{
		{"lineage_id": lineageID, "body": body, "created_at": now, "updated_at": now},
	}); err != nil {
		return err
	}
	return b.Run()
}

// EditComment updates an existing comment's body (spec session-annotations
// implies edit alongside add/view/remove).
func EditComment(db *sqlitex.Runner, commentID int64, body string) error {
	now := time.Now().Unix()
	b := db.NewBatch()
	if err := b.BulkUpdate("comments", "id", []string{"id", "body", "updated_at"}, []map[string]any{
		{"id": commentID, "body": body, "updated_at": now},
	}); err != nil {
		return err
	}
	return b.Run()
}

// RemoveComment deletes a comment (spec session-annotations, "Removing a
// comment": "the session itself is unaffected" - this only ever touches
// LazyRecall's own comments table).
func RemoveComment(db *sqlitex.Runner, commentID int64) error {
	pf, err := db.WriteParams(map[string]any{"id": commentID})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("DELETE FROM comments WHERE id = " + pf.Ref("id") + ";")
}

// CommentsForLineage lists every comment on a lineage, oldest first.
func CommentsForLineage(db *sqlitex.Runner, lineageID string) ([]Comment, error) {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	var rows []struct {
		ID        int64  `json:"id"`
		LineageID string `json:"lineage_id"`
		Body      string `json:"body"`
		CreatedAt int64  `json:"created_at"`
		UpdatedAt int64  `json:"updated_at"`
	}
	q := "SELECT id, lineage_id, body, created_at, updated_at FROM comments WHERE lineage_id = " + pf.Ref("lineage_id") + " ORDER BY created_at;"
	if err := db.Query(q, &rows); err != nil {
		return nil, err
	}
	out := make([]Comment, len(rows))
	for i, r := range rows {
		out[i] = Comment{
			ID: r.ID, LineageID: r.LineageID, Body: r.Body,
			CreatedAt: time.Unix(r.CreatedAt, 0), UpdatedAt: time.Unix(r.UpdatedAt, 0),
		}
	}
	return out, nil
}
