// Package search answers "find a past session" (spec session-search): the
// unified cross-agent listing, full-text search over what the user
// actually typed, repository/worktree grouping, and combinable filters. It
// only reads LazyRecall's own database - never a source directly.
package search

import (
	"fmt"
	"strings"
	"time"

	"lazyrecall/internal/session"
	"lazyrecall/internal/sqlitex"
)

// Filter narrows a listing or search. Zero values mean "no constraint on
// this dimension." Filters combine with AND (spec session-search,
// "Filtering", "Combining filters").
type Filter struct {
	Agent string // "claude" | "pi" | "omp" | "hermes"
	Since *time.Time
	Until *time.Time
	Repo  string // a git_common_root or a bare cwd, per GroupKey
	Tag   string
}

// Item is one session as shown in a listing or search result (spec
// session-search, "Unified session listing with preview"). Pointer fields
// are nil when the source never recorded that value - never a placeholder.
type Item struct {
	SessionID string
	Source    string
	LineageID string
	// Handle is the session's short, typable handle (spec session-search,
	// "Short session handle") - a small integer, unique within its profile,
	// never reused. Zero means no handle could be resolved for this row
	// (e.g. its lineage row does not exist yet, which should not happen
	// once a refresh has run to completion).
	Handle         int
	CWD            *string
	GitBranch      *string
	GitRepoRoot    *string
	GitCommonRoot  *string
	StartedAt      *time.Time
	LastActivityAt *time.Time
	Topic          *string
	// Name is the session name the user set inside the source tool, when
	// there is one. Display prefers it over Topic (see cli.RenderRow).
	Name       *string
	LastPrompt *string
	EndState   session.EndState
	// Origin is who drove the session, always a member of the closed set
	// session.Origin - a stored value outside the set (older build, or a
	// hand-edited row) reads back as OriginUnknown, never as-is.
	Origin       session.Origin
	DirExists    *bool // nil = never checked (no cwd known); false = missing
	MessageCount *int64
	Resumable    bool
	Tags         []string
	// Archived is true when the user archived the session (change
	// add-archive-facility). The flag lives on lineages, so it survives a
	// full index rebuild; it is shown by `archive list` and tagged on the
	// machine-readable output.
	Archived bool

	// MatchSnippet is set only by Search: the surrounding text of the
	// match, for the user to recognize the session (spec session-search,
	// "Match context is shown").
	MatchSnippet string
}

// GroupKey is what a session is grouped by for repository/worktree display
// (spec session-search, "Grouping by repository and worktree"): the
// canonical shared repo root when known, otherwise the bare working
// directory (never attributed to an unrelated repository).
func (it Item) GroupKey() (key string, isRepo bool) {
	if it.GitCommonRoot != nil && *it.GitCommonRoot != "" {
		return *it.GitCommonRoot, true
	}
	if it.CWD != nil && *it.CWD != "" {
		return *it.CWD, false
	}
	return "", false
}

// itemFrom is the shared FROM+JOIN clause behind itemColumns: a LEFT JOIN,
// deliberately, not an INNER JOIN - a session row must still be listable
// even in the (should-not-happen-post-refresh) case its lineage row is
// missing, rather than silently disappearing from every listing.
const itemFrom = `sessions s LEFT JOIN lineages l ON l.id = s.lineage_id`

var itemColumns = `s.id, s.source, s.lineage_id, l.handle, s.cwd, s.git_branch, s.git_repo_root, s.git_common_root,
	s.started_at, s.last_activity_at, s.topic, s.name, s.last_prompt, s.end_state, s.origin, s.dir_exists, s.message_count, s.resumable,
	l.archived_at IS NOT NULL AS archived`

type itemRow struct {
	ID             string  `json:"id"`
	Source         string  `json:"source"`
	LineageID      string  `json:"lineage_id"`
	Handle         *int    `json:"handle"`
	CWD            *string `json:"cwd"`
	GitBranch      *string `json:"git_branch"`
	GitRepoRoot    *string `json:"git_repo_root"`
	GitCommonRoot  *string `json:"git_common_root"`
	StartedAt      *int64  `json:"started_at"`
	LastActivityAt *int64  `json:"last_activity_at"`
	Topic          *string `json:"topic"`
	Name           *string `json:"name"`
	LastPrompt     *string `json:"last_prompt"`
	EndState       string  `json:"end_state"`
	Origin         string  `json:"origin"`
	DirExists      *int64  `json:"dir_exists"`
	MessageCount   *int64  `json:"message_count"`
	Resumable      int64   `json:"resumable"`
	Archived       int64   `json:"archived"`
	Tags           *string `json:"tags"`
}

func (row itemRow) toItem() Item {
	// Normalise on the way out: an empty or unrecognised stored value (a
	// database written by an older build, or a hand-edited row) must never
	// escape the closed set - it reads back as unknown.
	origin := session.Origin(row.Origin)
	if !origin.Valid() {
		origin = session.OriginUnknown
	}
	it := Item{
		SessionID:     row.ID,
		Source:        row.Source,
		LineageID:     row.LineageID,
		CWD:           row.CWD,
		GitBranch:     row.GitBranch,
		GitRepoRoot:   row.GitRepoRoot,
		GitCommonRoot: row.GitCommonRoot,
		Topic:         row.Topic,
		Name:          row.Name,
		LastPrompt:    row.LastPrompt,
		EndState:      session.EndState(row.EndState),
		Origin:        origin,
		MessageCount:  row.MessageCount,
		Resumable:     row.Resumable != 0,
		Archived:      row.Archived != 0,
	}
	if row.Handle != nil {
		it.Handle = *row.Handle
	}
	if row.StartedAt != nil {
		t := time.Unix(*row.StartedAt, 0)
		it.StartedAt = &t
	}
	if row.LastActivityAt != nil {
		t := time.Unix(*row.LastActivityAt, 0)
		it.LastActivityAt = &t
	}
	if row.DirExists != nil {
		v := *row.DirExists != 0
		it.DirExists = &v
	}
	if row.Tags != nil && *row.Tags != "" {
		it.Tags = strings.Split(*row.Tags, "\x1f")
	}
	return it
}

// whereClauseFromFilter builds the shared filter predicate (task 7.6). All
// values travel through the ParamFile mechanism - never string-interpolated
// (the same rule that governs writes applies to reads: nothing a user typed
// into a filter flag is trusted as SQL text).
func whereClauseFromFilter(f Filter, params map[string]any) (string, []string) {
	var clauses []string
	if f.Agent != "" {
		params["agent"] = f.Agent
		clauses = append(clauses, "agent")
	}
	if f.Since != nil {
		params["since"] = f.Since.Unix()
		clauses = append(clauses, "since")
	}
	if f.Until != nil {
		params["until"] = f.Until.Unix()
		clauses = append(clauses, "until")
	}
	if f.Repo != "" {
		params["repo"] = f.Repo
		clauses = append(clauses, "repo")
	}
	if f.Tag != "" {
		params["tag"] = f.Tag
		clauses = append(clauses, "tag")
	}
	return "", clauses
}

func buildPredicate(pf *sqlitex.ParamFile, clauses []string) string {
	var parts []string
	for _, c := range clauses {
		switch c {
		case "agent":
			parts = append(parts, "s.source = "+pf.Ref("agent"))
		case "since":
			parts = append(parts, "s.last_activity_at >= "+pf.Ref("since"))
		case "until":
			parts = append(parts, "s.last_activity_at <= "+pf.Ref("until"))
		case "repo":
			parts = append(parts, "(s.git_common_root = "+pf.Ref("repo")+" OR (s.git_common_root IS NULL AND s.cwd = "+pf.Ref("repo")+"))")
		case "tag":
			parts = append(parts, "EXISTS (SELECT 1 FROM tags tg WHERE tg.lineage_id = s.lineage_id AND tg.tag = "+pf.Ref("tag")+")")
		}
	}
	if len(parts) == 0 {
		return "1=1"
	}
	return strings.Join(parts, " AND ")
}

// List returns every session matching f, most recently active first (spec
// session-search, "Unified session listing with preview"). An empty result
// is not itself an error - callers use EmptyMessage for the "no session
// matched" text (spec, "Filter matches nothing").
func List(db *sqlitex.Runner, f Filter) ([]Item, error) {
	params := map[string]any{}
	_, clauses := whereClauseFromFilter(f, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	predicate := buildPredicate(pf, clauses)
	q := fmt.Sprintf(`
SELECT %s, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE %s
ORDER BY s.last_activity_at DESC;`, itemColumns, itemFrom, predicate)

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("search: listing sessions: %w", err)
	}
	items := make([]Item, len(rows))
	for i, row := range rows {
		items[i] = row.toItem()
	}
	return items, nil
}

// ListByLineageIDs returns the sessions belonging to any of the given
// lineages, most recently active first, rendered through the same row path
// as List - `archive list` uses it to show archived sessions via the normal
// listing rather than a bespoke format. An empty id list yields no rows.
func ListByLineageIDs(db *sqlitex.Runner, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	params := make(map[string]any, len(ids))
	keys := make([]string, len(ids))
	for i, id := range ids {
		key := fmt.Sprintf("lin_%d", i)
		params[key] = id
		keys[i] = key
	}
	pf, err := db.WriteParams(params)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	refs := make([]string, len(keys))
	for i, k := range keys {
		refs[i] = pf.Ref(k)
	}
	inClause := "l.id IN (" + strings.Join(refs, ", ") + ")"
	q := fmt.Sprintf(`
SELECT %s, (SELECT group_concat(tag, char(31)) FROM tags WHERE tags.lineage_id = s.lineage_id) AS tags
FROM %s
WHERE %s
ORDER BY s.last_activity_at DESC;`, itemColumns, itemFrom, inClause)

	var rows []itemRow
	if err := db.Query(q, &rows); err != nil {
		return nil, fmt.Errorf("search: listing sessions by lineage: %w", err)
	}
	items := make([]Item, len(rows))
	for i, row := range rows {
		items[i] = row.toItem()
	}
	return items, nil
}

// nothing, naming the profile and the scope that was searched (spec
// session-search, "No session matches", "Filter matches nothing").
func EmptyMessage(profileName string, f Filter, query string) string {
	scope := "prompts and topics"
	if query == "" {
		scope = "the active filters"
	}
	var extra []string
	if f.Agent != "" {
		extra = append(extra, "agent="+f.Agent)
	}
	if f.Repo != "" {
		extra = append(extra, "repo="+f.Repo)
	}
	if f.Tag != "" {
		extra = append(extra, "tag="+f.Tag)
	}
	if f.Since != nil || f.Until != nil {
		extra = append(extra, "time range")
	}
	suffix := ""
	if len(extra) > 0 {
		suffix = " (" + strings.Join(extra, ", ") + ")"
	}
	if query != "" {
		return fmt.Sprintf("No session in profile %q matched %q, searched over %s%s.", profileName, query, scope, suffix)
	}
	return fmt.Sprintf("No session in profile %q matched %s%s.", profileName, scope, suffix)
}
