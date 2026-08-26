package annotate

import (
	"fmt"
	"time"

	"lazyrecall/internal/sqlitex"
)

// AddTag applies a named tag to a lineage (spec session-annotations,
// "Tags on sessions"). Applying the same tag twice is a no-op - tags is
// keyed (lineage_id, tag).
func AddTag(db *sqlitex.Runner, lineageID, tag string) error {
	b := db.NewBatch()
	if err := b.BulkUpsert("tags", []string{"lineage_id", "tag"}, []string{"lineage_id", "tag", "created_at"}, []map[string]any{
		{"lineage_id": lineageID, "tag": tag, "created_at": time.Now().Unix()},
	}); err != nil {
		return err
	}
	return b.Run()
}

// RemoveTag removes a tag from a lineage (spec session-annotations,
// "Removing a tag from a session": "other sessions carrying the tag are
// unaffected" - this deletes exactly one (lineage_id, tag) row).
func RemoveTag(db *sqlitex.Runner, lineageID, tag string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID, "tag": tag})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec(fmt.Sprintf("DELETE FROM tags WHERE lineage_id = %s AND tag = %s;", pf.Ref("lineage_id"), pf.Ref("tag")))
}

// TagsForLineage lists the tags on one lineage.
func TagsForLineage(db *sqlitex.Runner, lineageID string) ([]string, error) {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return nil, err
	}
	defer pf.Close()
	var rows []struct {
		Tag string `json:"tag"`
	}
	q := "SELECT tag FROM tags WHERE lineage_id = " + pf.Ref("lineage_id") + " ORDER BY tag;"
	if err := db.Query(q, &rows); err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Tag
	}
	return out, nil
}

// AllTags lists every distinct tag currently in use (spec
// session-annotations, "to list the tags in use").
func AllTags(db *sqlitex.Runner) ([]string, error) {
	var rows []struct {
		Tag string `json:"tag"`
	}
	if err := db.Query(`SELECT DISTINCT tag FROM tags ORDER BY tag;`, &rows); err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Tag
	}
	return out, nil
}
