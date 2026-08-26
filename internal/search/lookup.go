package search

import (
	"fmt"
	"strconv"

	"recall/internal/session"
	"recall/internal/sqlitex"
)

// ItemForIdentifier resolves a single session identifier typed by the user
// - either form (spec session-search, "Short session handle" / design.md
// decision 4) - to the Item it names. An all-digit arg is resolved as a
// handle against profileName's lineages, picking that lineage's most
// recently active session; anything else is treated as the fully-qualified
// session identifier, resolved by exact match. ok is false, with no error,
// when the identifier is well-formed but resolves to nothing (spec, "Handle
// of a session that is gone": "reports that it does not resolve" - this is
// a normal, expected outcome, not a failure).
func ItemForIdentifier(db *sqlitex.Runner, profileName, arg string) (Item, bool, error) {
	if session.IsHandle(arg) {
		return itemForHandle(db, profileName, arg)
	}
	return ItemBySessionID(db, arg)
}

// ItemBySessionID looks up one session by its fully-qualified identifier.
func ItemBySessionID(db *sqlitex.Runner, id string) (Item, bool, error) {
	pf, err := db.WriteParams(map[string]any{"id": id})
	if err != nil {
		return Item{}, false, err
	}
	defer pf.Close()

	q := fmt.Sprintf(`
SELECT %s, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE s.id = %s
LIMIT 1;`, itemColumns, itemFrom, pf.Ref("id"))

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return Item{}, false, fmt.Errorf("search: looking up session %s: %w", id, err)
	}
	if len(rows) == 0 {
		return Item{}, false, nil
	}
	return rows[0].toItem(), true, nil
}

// itemForHandle resolves a handle (already confirmed all-digit by the
// caller) to the most recently active session in the lineage it names -
// never a different lineage (design.md decision 3, "Handles are never
// reused": acting on the wrong session because a handle stopped resolving
// cleanly is exactly what must not happen).
func itemForHandle(db *sqlitex.Runner, profileName, arg string) (Item, bool, error) {
	handle, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		// session.IsHandle already confirmed arg is all-digit, so this can
		// only happen on an implausibly long number; treat it the same as
		// "does not resolve" rather than erroring.
		return Item{}, false, nil
	}
	pf, err := db.WriteParams(map[string]any{"profile": profileName, "handle": handle})
	if err != nil {
		return Item{}, false, err
	}
	defer pf.Close()

	q := fmt.Sprintf(`
SELECT %s, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE l.profile = %s AND l.handle = %s
ORDER BY s.last_activity_at DESC
LIMIT 1;`, itemColumns, itemFrom, pf.Ref("profile"), pf.Ref("handle"))

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return Item{}, false, fmt.Errorf("search: looking up handle %s: %w", arg, err)
	}
	if len(rows) == 0 {
		return Item{}, false, nil
	}
	return rows[0].toItem(), true, nil
}
