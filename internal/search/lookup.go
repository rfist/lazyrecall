package search

import (
	"fmt"
	"strconv"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// minPrefixLength is the shortest argument ItemForIdentifier will try as a
// unique-id prefix (task: "resume shortcuts and unique id prefixes"). Below
// this length a typo-sized fragment would routinely match many sessions in
// a large index, so a short arg is simply reported as not resolving -
// exactly the outcome a handle-shaped-but-unassigned arg already gets -
// rather than surfacing a candidate list nobody meant to ask for.
const minPrefixLength = 4

// AmbiguousError is ItemForIdentifier's report that arg, tried as a
// unique-id prefix, matched more than one session: resolving to any single
// one of them would risk acting on the wrong session, which is exactly what
// requiring uniqueness exists to prevent. Candidates holds every matching
// Item, most recently active first, so a caller can show the user enough to
// type a longer, disambiguating prefix - cli.RenderAmbiguous is the one
// place every verb that resolves an identifier (comment, tag, archive,
// unarchive, group, resume) renders that list, so they can never drift out
// of sync with each other.
type AmbiguousError struct {
	Identifier string
	Candidates []Item
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("search: %q matches %d sessions; type more of the id to narrow it down", e.Identifier, len(e.Candidates))
}

// ItemForIdentifier resolves a single session identifier typed by the user
// - any of a short handle, a fully-qualified composite id, or a unique
// prefix of one (spec session-search, "Short session handle" / design.md
// decision 4; task: resume shortcuts and unique id prefixes) - to the Item
// it names. Resolution order: an all-digit arg is resolved as a handle
// against the lineages table, picking that lineage's most recently active
// session, exactly as before prefixes existed - handle lookup never falls
// through to prefix matching, so a handle that resolves to nothing is
// reported as such rather than being retried as a (numeric-looking, and
// never a real) id prefix. Anything else is tried first as an exact match
// against the fully-qualified composite id, then, when that finds nothing,
// as a prefix - of at least minPrefixLength characters - of either the
// composite id or the source's own native SourceSessionID. Handle lookup is
// global, not scoped to a profile (change group-sessions-in-one-index: one
// index now holds every install, and schema v8 makes a handle unique across
// all of them, not just within one).
//
// ok is false, with no error, when the identifier is well-formed but
// resolves to nothing (spec, "Handle of a session that is gone": "reports
// that it does not resolve" - a normal, expected outcome, not a failure).
// A prefix matching more than one session is instead reported as a non-nil
// *AmbiguousError, never as ok=false - the two failure shapes read
// differently to a caller for good reason: one prefix could name several
// specific sessions, the other could name none. groups is the configured
// groups (config.Config.Groups), needed to populate the returned Item's
// Group field exactly as every other query does.
func ItemForIdentifier(db *sqlitex.Runner, arg string, groups []config.Group) (Item, bool, error) {
	if session.IsHandle(arg) {
		return itemForHandle(db, arg, groups)
	}
	if item, ok, err := ItemBySessionID(db, arg, groups); err != nil || ok {
		return item, ok, err
	}
	return itemByPrefix(db, arg, groups)
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

// itemByPrefix resolves arg as a unique prefix of either a session's
// composite id or its native SourceSessionID, once ItemForIdentifier has
// already confirmed arg is not a handle and matches no session's composite
// id exactly. Shorter than minPrefixLength is reported as not resolving
// without ever querying - see minPrefixLength's own doc comment.
//
// The prefix test is substr(...) = ..., never LIKE/GLOB: arg is text a user
// typed, and '%'/'_' are meaningful pattern characters to both of those -
// exactly the reasoning effectiveGroupExpr already applies to a configured
// group path (see its own doc comment), applied here to a typed identifier
// instead. Matching against both columns in one query, rather than trying
// the composite id first and only then the native id, is what lets one
// query decide uniqueness across both forms at once - a prefix that matches
// one session's composite id and a different session's native id is exactly
// as ambiguous as one matching two sessions the same way.
//
// Archived and hidden sessions resolve here exactly as they do by exact id
// (ItemBySessionID): this query applies no hide rule and no archive
// exclusion, matching the existing exact-match behaviour a caller already
// depends on (e.g. `archive list`'s own identifiers must keep resolving).
func itemByPrefix(db *sqlitex.Runner, arg string, groups []config.Group) (Item, bool, error) {
	if len(arg) < minPrefixLength {
		return Item{}, false, nil
	}
	params := map[string]any{"prefix": arg}
	branches := effectiveGroupParams(params, groups)
	pf, err := db.WriteParams(params)
	if err != nil {
		return Item{}, false, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	prefixRef := pf.Ref("prefix")
	q := fmt.Sprintf(`
SELECT %s, %s AS effective_group, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE substr(s.id, 1, length(%s)) = %s OR substr(s.source_session_id, 1, length(%s)) = %s
ORDER BY s.last_activity_at DESC;`, itemColumns, groupExpr, itemFrom, prefixRef, prefixRef, prefixRef, prefixRef)

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return Item{}, false, fmt.Errorf("search: resolving prefix %q: %w", arg, err)
	}
	switch len(rows) {
	case 0:
		return Item{}, false, nil
	case 1:
		return rows[0].toItem(), true, nil
	default:
		items := make([]Item, len(rows))
		for i, row := range rows {
			items[i] = row.toItem()
		}
		return Item{}, false, &AmbiguousError{Identifier: arg, Candidates: items}
	}
}
