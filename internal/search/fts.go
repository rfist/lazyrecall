package search

import (
	"fmt"

	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// Search finds sessions whose prompts or topic match query (spec
// session-search, "Search over the user's own prompts and session
// topics"). Each word of query becomes an ANDed FTS5 prefix term
// (sqlitex.FTS5PrefixTerms), so "sketch" matches an indexed token like
// "sketchybar" and a multi-word query matches its words in any order;
// punctuation and FTS5 query-syntax characters in what the user typed are
// never parsed as query operators.
func Search(db *sqlitex.Runner, query string, f Filter) ([]Item, error) {
	params := map[string]any{"query": sqlitex.FTS5PrefixTerms(query)}
	branches := effectiveGroupParams(params, f.Groups)
	_, clauses := whereClauseFromFilter(f, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	predicate := buildPredicate(pf, clauses, groupExpr)
	q := fmt.Sprintf(`
SELECT %s, %s AS effective_group,
	(SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags,
	snippet(prompt_fts, 2, '>>>', '<<<', ' ... ', 10) AS snippet,
	bm25(prompt_fts) AS rank
FROM prompt_fts
JOIN sessions s ON s.id = prompt_fts.session_id
LEFT JOIN lineages l ON l.id = s.lineage_id
WHERE prompt_fts MATCH %s AND %s
ORDER BY s.last_activity_at DESC, rank;`, itemColumns, groupExpr, pf.Ref("query"), predicate)

	var rows []struct {
		itemRow
		Snippet string `json:"snippet"`
	}
	if err := db.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("search: querying prompt index: %w", err)
	}

	seen := map[string]bool{}
	var items []Item
	for _, row := range rows {
		if seen[row.ID] {
			continue // a session can have more than one matching prompt; keep its best (first-ordered) match only
		}
		seen[row.ID] = true
		it := row.itemRow.toItem()
		it.MatchSnippet = row.Snippet
		items = append(items, it)
	}
	return items, nil
}

// SearchForFacets is Search's counterpart to ListForFacets: every prompt/
// topic match for query, regardless of the selected group and regardless of
// archive state, for a Groups panel narrowed by an active search phrase to
// compute its counts from client-side (change group-sessions-in-one-index,
// P1 fix #4). See ListForFacets' doc comment for why f.Group is ignored and
// archived sessions are always included.
func SearchForFacets(db *sqlitex.Runner, query string, f Filter) ([]Item, error) {
	params := map[string]any{"query": sqlitex.FTS5PrefixTerms(query)}
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
SELECT %s, %s AS effective_group,
	(SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM prompt_fts
JOIN sessions s ON s.id = prompt_fts.session_id
LEFT JOIN lineages l ON l.id = s.lineage_id
WHERE prompt_fts MATCH %s AND %s;`, itemColumns, groupExpr, pf.Ref("query"), predicate)

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("search: querying prompt index for facet counts: %w", err)
	}

	seen := map[string]bool{}
	var items []Item
	for _, row := range rows {
		if seen[row.ID] {
			continue // a session can have more than one matching prompt; count it once
		}
		seen[row.ID] = true
		items = append(items, row.toItem())
	}
	return items, nil
}

// SearchWithHidden returns the search hits and how many the hide rules and
// the archive flag suppressed: one COUNT(DISTINCT) over the same predicate
// without the hide clauses, minus the visible hits - a session with several
// matching prompts still counts once, exactly as Search shows it once.
func SearchWithHidden(db *sqlitex.Runner, query string, f Filter) (items []Item, hidden int, err error) {
	items, err = Search(db, query, f)
	if err != nil {
		return nil, 0, err
	}
	total, err := countSearchHits(db, query, f)
	if err != nil {
		return nil, 0, err
	}
	return items, total - len(items), nil
}

// countSearchHits counts the sessions matching query and f's non-hide
// predicate. COUNT(DISTINCT s.id) mirrors Search's de-duplication, so a
// session with several matching prompts contributes one candidate.
func countSearchHits(db *sqlitex.Runner, query string, f Filter) (int, error) {
	params := map[string]any{"query": sqlitex.FTS5PrefixTerms(query)}
	branches := effectiveGroupParams(params, f.Groups)
	clauses := baseClausesFromFilter(f, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return 0, err
	}
	defer pf.Close()

	groupExpr := effectiveGroupExpr(pf, branches)
	predicate := buildPredicate(pf, clauses, groupExpr)
	q := fmt.Sprintf(`
SELECT COUNT(DISTINCT s.id) AS n
FROM prompt_fts
JOIN sessions s ON s.id = prompt_fts.session_id
LEFT JOIN lineages l ON l.id = s.lineage_id
WHERE prompt_fts MATCH %s AND %s;`, pf.Ref("query"), predicate)

	var rows []struct {
		N int `json:"n"`
	}
	if err := db.Query(q, &rows); err != nil {
		return 0, fmt.Errorf("search: counting search hits: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].N, nil
}

// PromptsForSession returns the user's own prompts for one session, oldest
// first, as they were indexed for full-text search. The browser's Prompts
// tab shows them so a session can be recognised by what was actually asked
// of the agent, which is often the only thing the user remembers about it.
//
// It reads prompt_fts directly rather than re-parsing the transcript: the
// rows are already there, already scoped to the session, and reading them
// costs nothing next to opening a source file the browser has no reason to
// touch. limit bounds the read - a long session can hold hundreds of
// prompts and the pane can show a handful.
func PromptsForSession(db *sqlitex.Runner, sessionID string, limit int) ([]string, error) {
	params := map[string]any{"sid": sessionID}
	pf, err := db.WriteParams(params)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	q := fmt.Sprintf(`
SELECT text AS text FROM prompt_fts
WHERE session_id = %s AND kind = 'prompt'
LIMIT %d;`, pf.Ref("sid"), limit)

	var rows []struct {
		Text string `json:"text"`
	}
	if err := db.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("search: reading prompts for %s: %w", sessionID, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Text != "" {
			out = append(out, r.Text)
		}
	}
	return out, nil
}
