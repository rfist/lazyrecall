// Effective-group resolution (change group-sessions-in-one-index): the
// query-time computation of which group a session belongs to, and the
// counts and validation built on top of it. Kept separate from group.go,
// which predates this change and groups sessions by repository/worktree - a
// different, unrelated notion of "group" that happens to share the word.
package search

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// groupBranch is one longest-prefix-first WHEN of the effective-group CASE
// expression built by effectiveGroupExpr: pathKey/nameKey are the ParamFile
// fields the path and the group name it names are registered under
// (effectiveGroupParams registers them), and path is kept alongside so the
// "/" special case below can be recognised without a second round-trip
// through the param file.
type groupBranch struct {
	pathKey, nameKey string
	path             string
}

// effectiveGroupParams registers, into params, every value the effective-
// group expression needs - one (path, name) pair per configured group path
// - and returns them ordered longest-path-first, so the CASE expression's
// first matching WHEN is always the longest (most specific) prefix. Ties -
// two paths of equal length - keep the order they were declared in the
// config file, via a stable sort over entries already built in that order
// (config.Config.Groups is itself in declaration order; see config.Load).
//
// This must run, and its result be used to build the effective-group SELECT
// expression, on every query that returns Items - not only the ones that
// filter by group - because Item.Group is populated from it regardless of
// whether the caller asked to narrow by group at all.
func effectiveGroupParams(params map[string]any, groups []config.Group) []groupBranch {
	type entry struct {
		path, name string
	}
	var entries []entry
	for _, g := range groups {
		for _, p := range g.Paths {
			entries = append(entries, entry{path: p, name: g.Name})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].path) > len(entries[j].path)
	})

	branches := make([]groupBranch, len(entries))
	for i, e := range entries {
		pathKey := fmt.Sprintf("group_path_%d", i)
		nameKey := fmt.Sprintf("group_name_%d", i)
		params[pathKey] = e.path
		params[nameKey] = e.name
		branches[i] = groupBranch{pathKey: pathKey, nameKey: nameKey, path: e.path}
	}
	return branches
}

// effectiveGroupExpr builds the SQL expression for a session's effective
// group (change group-sessions-in-one-index, "Model"): the manual override
// on its lineage, else the configured group whose path is the longest
// prefix of the session's cwd, else NULL (Unknown). It is built in this one
// place and used by every query that returns Items, the same way the hide
// rules are (buildPredicate).
//
// The prefix test deliberately avoids GLOB/LIKE - a configured path can
// contain '[', '*', '?', '%' or '_', every one of which those operators
// treat as a pattern character - in favor of a literal comparison: a cwd
// equals the root exactly, or starts with the root followed by a path
// separator. "/" is handled as its own branch because that general rule
// would otherwise require a cwd starting with "//", which no absolute path
// is: a root group path of "/" must match every absolute cwd instead.
func effectiveGroupExpr(pf *sqlitex.ParamFile, branches []groupBranch) string {
	if len(branches) == 0 {
		return "l.group_name"
	}
	cases := make([]string, len(branches))
	for i, b := range branches {
		pathRef := pf.Ref(b.pathKey)
		nameRef := pf.Ref(b.nameKey)
		var cond string
		if b.path == "/" {
			cond = "s.cwd IS NOT NULL"
		} else {
			cond = "(s.cwd = " + pathRef + " OR substr(s.cwd, 1, length(" + pathRef + ") + 1) = " + pathRef + " || '/')"
		}
		cases[i] = "WHEN " + cond + " THEN " + nameRef
	}
	return "COALESCE(l.group_name, CASE " + strings.Join(cases, " ") + " END)"
}

// pathGroupMatch is effectiveGroupExpr's Go-side twin: the same longest-
// prefix, order-of-ties, and "/" special case, computed directly over a cwd
// string instead of compiled into SQL. It exists for the browser's `p`
// popup (Stage E), which needs to show what clearing a manual override
// would produce - "Automatic (work)" - without a round trip through the
// database, and must never silently drift from what the database would
// actually compute. path is returned alongside name so a caller (the
// Detail tab) can say *which* configured path matched, not only which group
// it belongs to.
func pathGroupMatch(groups []config.Group, cwd string) (name, path string) {
	if cwd == "" {
		return "", ""
	}
	type entry struct{ path, name string }
	var entries []entry
	for _, g := range groups {
		for _, p := range g.Paths {
			entries = append(entries, entry{path: p, name: g.Name})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].path) > len(entries[j].path)
	})
	for _, e := range entries {
		if e.path == "/" {
			return e.name, e.path
		}
		if cwd == e.path || strings.HasPrefix(cwd, e.path+"/") {
			return e.name, e.path
		}
	}
	return "", ""
}

// PathGroup computes a session's group from path rules alone, ignoring any
// manual override - the "if the popup chose Automatic" answer effectiveGroupExpr
// gives when l.group_name is NULL. Empty means Unknown, never a placeholder.
func PathGroup(groups []config.Group, cwd string) string {
	name, _ := pathGroupMatch(groups, cwd)
	return name
}

// PathGroupPath is PathGroup's sibling: the specific configured path that
// won the longest-prefix match, alongside the group name it belongs to -
// what the Detail tab shows as "group: work (path ~/code)" so the reason a
// session landed in that group is visible, not just the fact that it did.
func PathGroupPath(groups []config.Group, cwd string) (name, path string) {
	return pathGroupMatch(groups, cwd)
}

// ValidateGroup checks a --group value (or the browser popup's choice, from
// Stage E) against the configured groups: "" (All), "archive", "unknown",
// and any configured group's name are valid; anything else is an error
// listing every valid value, so a typo is reported instead of silently
// matching nothing. It is one helper so the CLI and the browser agree on
// exactly the same set of valid values.
func ValidateGroup(cfg config.Config, name string) error {
	switch name {
	case "", "archive", "unknown":
		return nil
	}
	for _, g := range cfg.Groups {
		if g.Name == name {
			return nil
		}
	}
	valid := make([]string, 0, len(cfg.Groups)+3)
	valid = append(valid, "all", "archive", "unknown")
	for _, g := range cfg.Groups {
		valid = append(valid, g.Name)
	}
	return fmt.Errorf("search: unknown group %q; valid groups are: %s", name, strings.Join(valid, ", "))
}

// GroupCounts is the per-group, archive, and unknown counts the `groups`
// command and (Stage E) the Groups panel both show.
type GroupCounts struct {
	// ByGroup counts non-archived sessions per configured group name, with
	// the standing hide rules applied exactly as List applies them - a
	// count that disagrees with what --group=NAME would actually list is
	// worse than no count at all.
	ByGroup map[string]int
	// Archive counts every archived session, regardless of which group it
	// is filed under - archiving is a state a session can be in within any
	// group, not a group of its own.
	Archive int
	// Unknown counts non-archived sessions whose effective group is NULL:
	// no manual override and no configured path claims them.
	Unknown int
}

// facetSupersetClauses builds the clause set ListForFacets and
// SearchForFacets share: f's ordinary predicate (agent, install, client,
// repo, tag, since, until - f.Group is forced off, see ListForFacets' doc
// comment) plus the standing hide rules unless f.ShowAll, but never the
// archive exclusion - archived sessions always pass, so a panel counting
// them (the Groups panel's Archive row) can see them regardless of which
// group, if any, is currently selected.
func facetSupersetClauses(f Filter, params map[string]any) []string {
	f.Group = ""
	clauses := baseClausesFromFilter(f, params)
	if !f.ShowAll {
		clauses = append(clauses, standingHideClauses(f.Hide, params)...)
	}
	return clauses
}

// ListForFacets returns every session that could count toward some facet
// panel's row, regardless of which group (if any) is currently selected and
// regardless of archive state - the superset the Groups panel computes its
// counts from client-side, the same way the Agents/Repos/Tags panels
// already do over their own loaded corpus (internal/cli/facet.go:
// countBy/narrow/matchesFacets). Change group-sessions-in-one-index, P1 fix:
// the Groups panel used to source its counts from Counts, a query over the
// whole index that ignored the Agents/Repos/Tags/text selections already
// narrowing every other panel on screen - so a group with sessions only
// outside the current agent filter was still advertised as if selecting it
// would show something.
//
// f.Group is ignored (facetSupersetClauses forces it off): the caller
// narrows by group afterward, client-side, exactly like every other facet.
// Archived sessions are always included for the same reason - Archive is
// one of the Groups panel's own rows, not a state this query should ever
// exclude on the caller's behalf.
func ListForFacets(db *sqlitex.Runner, f Filter) ([]Item, error) {
	params := map[string]any{}
	branches := effectiveGroupParams(params, f.Groups)
	clauses := facetSupersetClauses(f, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	predicate := buildPredicate(pf, clauses, groupExpr)
	q := fmt.Sprintf(`
SELECT %s, %s AS effective_group, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE %s
ORDER BY s.last_activity_at DESC;`, itemColumns, groupExpr, itemFrom, predicate)

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("search: listing sessions for facet counts: %w", err)
	}
	items := make([]Item, len(rows))
	for i, row := range rows {
		items[i] = row.toItem()
	}
	return items, nil
}

// Counts computes GroupCounts in one query: one COUNT(*) grouped by a
// session's effective group and its archive state, since effectiveGroupExpr
// is the same expression that classifies every row everywhere else. hide
// carries the standing hide rules (not ShowAll - a count is meaningless
// without knowing which rules it honors, so Counts always applies them,
// matching what a plain `list`/`browse` with no --all would show).
func Counts(db *sqlitex.Runner, hide config.Hide, groups []config.Group) (GroupCounts, error) {
	params := map[string]any{}
	branches := effectiveGroupParams(params, groups)
	clauses := standingHideClauses(hide, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return GroupCounts{}, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	predicate := buildPredicate(pf, clauses, groupExpr)
	q := fmt.Sprintf(`
SELECT %s AS grp, l.archived_at IS NOT NULL AS is_archived, COUNT(*) AS n
FROM %s
WHERE %s
GROUP BY grp, is_archived;`, groupExpr, itemFrom, predicate)

	var rows []struct {
		Grp        *string `json:"grp"`
		IsArchived int64   `json:"is_archived"`
		N          int     `json:"n"`
	}
	if err := db.Query(q, &rows); err != nil {
		return GroupCounts{}, fmt.Errorf("search: counting groups: %w", err)
	}

	out := GroupCounts{ByGroup: map[string]int{}}
	for _, r := range rows {
		switch {
		case r.IsArchived != 0:
			out.Archive += r.N
		case r.Grp == nil:
			out.Unknown += r.N
		default:
			out.ByGroup[*r.Grp] += r.N
		}
	}
	return out, nil
}
