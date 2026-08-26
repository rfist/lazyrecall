package search

import (
	"fmt"

	"recall/internal/sqlitex"
)

// Search finds sessions whose prompts or topic match query (spec
// session-search, "Search over the user's own prompts and session
// topics"). query is matched as a literal FTS5 phrase (sqlitex.FTS5Phrase)
// so punctuation and FTS5 query-syntax characters in what the user typed
// are never parsed as query operators.
func Search(db *sqlitex.Runner, query string, f Filter) ([]Item, error) {
	params := map[string]any{"query": sqlitex.FTS5Phrase(query)}
	_, clauses := whereClauseFromFilter(f, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	predicate := buildPredicate(pf, clauses)
	q := fmt.Sprintf(`
SELECT %s,
	(SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags,
	snippet(prompt_fts, 2, '>>>', '<<<', ' ... ', 10) AS snippet,
	bm25(prompt_fts) AS rank
FROM prompt_fts
JOIN sessions s ON s.id = prompt_fts.session_id
LEFT JOIN lineages l ON l.id = s.lineage_id
WHERE prompt_fts MATCH %s AND %s
ORDER BY s.last_activity_at DESC, rank;`, itemColumns, pf.Ref("query"), predicate)

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
