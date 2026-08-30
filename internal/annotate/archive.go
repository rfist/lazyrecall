package annotate

import (
	"fmt"
	"time"

	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// Archive marks a lineage archived at the current time (change
// add-archive-facility). The archive flag lives on the durable lineages
// table, never on the disposable sessions table, so the user's decision
// survives the next refresh --full, which drops and rebuilds sessions. It
// is idempotent: archiving something already archived keeps the original
// timestamp - "when did I archive this" must stay answerable, and a
// repeated command must not move it.
func Archive(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID, "now": time.Now().Unix()})
	if err != nil {
		return err
	}
	defer pf.Close()
	// COALESCE(archived_at, now) preserves an existing timestamp instead of
	// overwriting it, which is what makes a double-archive a no-op.
	return db.Exec("UPDATE lineages SET archived_at = COALESCE(archived_at, " + pf.Ref("now") + ") WHERE id = " + pf.Ref("lineage_id") + ";")
}

// Unarchive clears a lineage's archive flag, returning it to normal
// listings. Clearing an already-unarchived lineage is a no-op.
func Unarchive(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET archived_at = NULL WHERE id = " + pf.Ref("lineage_id") + ";")
}

// IsArchived reports whether a lineage is currently archived.
func IsArchived(db *sqlitex.Runner, lineageID string) (bool, error) {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return false, err
	}
	defer pf.Close()

	var rows []struct {
		ArchivedAt *int64 `json:"archived_at"`
	}
	q := "SELECT archived_at FROM lineages WHERE id = " + pf.Ref("lineage_id") + ";"
	if err := db.Query(q, &rows); err != nil {
		return false, fmt.Errorf("annotate: checking archive status of %s: %w", lineageID, err)
	}
	if len(rows) == 0 {
		return false, fmt.Errorf("annotate: no such lineage %q", lineageID)
	}
	return rows[0].ArchivedAt != nil, nil
}

// AllArchived lists every archived lineage's id, most recently archived
// first, for `archive list`.
func AllArchived(db *sqlitex.Runner) ([]string, error) {
	var rows []struct {
		ID string `json:"id"`
	}
	if err := db.Query(`SELECT id FROM lineages WHERE archived_at IS NOT NULL ORDER BY archived_at DESC, id;`, &rows); err != nil {
		return nil, fmt.Errorf("annotate: listing archived lineages: %w", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out, nil
}
