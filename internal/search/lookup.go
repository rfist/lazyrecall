package search

import (
	"fmt"
	"strconv"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// ItemForIdentifier resolves a single session identifier typed by the user
// - either form (spec session-search, "Short session handle" / design.md
// decision 4) - to the Item it names. An all-digit arg is resolved as a
// handle against the lineages table, picking that lineage's most recently
// active session; anything else is treated as the fully-qualified session
// identifier, resolved by exact match. Handle lookup is global, not scoped
// to a profile (change group-sessions-in-one-index: one index now holds
// every install, and schema v8 makes a handle unique across all of them,
// not just within one). ok is false, with no error, when the identifier is
// well-formed but resolves to nothing (spec, "Handle of a session that is
// gone": "reports that it does not resolve" - this is a normal, expected
// outcome, not a failure). groups is the configured groups
// (config.Config.Groups), needed to populate the returned Item's Group
// field exactly as every other query does.
func ItemForIdentifier(db *sqlitex.Runner, arg string, groups []config.Group) (Item, bool, error) {
	if session.IsHandle(arg) {
		return itemForHandle(db, arg, groups)
	}
	return ItemBySessionID(db, arg, groups)
}

// ItemBySessionID looks up one session by its fully-qualified identifier.
func ItemBySessionID(db *sqlitex.Runner, id string, groups []config.Group) (Item, bool, error) {
	params := map[string]any{"id": id}
	branches := effectiveGroupParams(params, groups)
	pf, err := db.WriteParams(params)
	if err != nil {
		return Item{}, false, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	q := fmt.Sprintf(`
SELECT %s, %s AS effective_group, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE s.id = %s
LIMIT 1;`, itemColumns, groupExpr, itemFrom, pf.Ref("id"))

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
func itemForHandle(db *sqlitex.Runner, arg string, groups []config.Group) (Item, bool, error) {
	handle, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		// session.IsHandle already confirmed arg is all-digit, so this can
		// only happen on an implausibly long number; treat it the same as
		// "does not resolve" rather than erroring.
		return Item{}, false, nil
	}
	params := map[string]any{"handle": handle}
	branches := effectiveGroupParams(params, groups)
	pf, err := db.WriteParams(params)
	if err != nil {
		return Item{}, false, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	q := fmt.Sprintf(`
SELECT %s, %s AS effective_group, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE l.handle = %s
ORDER BY s.last_activity_at DESC
LIMIT 1;`, itemColumns, groupExpr, itemFrom, pf.Ref("handle"))

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return Item{}, false, fmt.Errorf("search: looking up handle %s: %w", arg, err)
	}
	if len(rows) == 0 {
		return Item{}, false, nil
	}
	return rows[0].toItem(), true, nil
}
