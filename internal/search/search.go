// Package search answers "find a past session" (spec session-search): the
// unified cross-agent listing, full-text search over what the user
// actually typed, repository/worktree grouping, and combinable filters. It
// only reads LazyRecall's own database - never a source directly.
package search

import (
	"fmt"
	"strings"
	"time"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// Filter narrows a listing or search. Zero values mean "no constraint on
// this dimension." Filters combine with AND (spec session-search,
// "Filtering", "Combining filters"). Hide carries the standing hide rules
// from the config - its zero value applies nothing - and ShowAll disables
// them together with the archive flag: a user who asks to see everything
// sees everything.
type Filter struct {
	Agent string // "claude" | "pi" | "omp" | "hermes"
	// Client narrows to sessions driven through one program: the source's
	// own raw value ("sdk-ts") or the short label a listing shows for it
	// ("acp"), which are matched interchangeably so a user can filter by
	// what they read on screen (change show-editor-clients).
	Client string
	Since  *time.Time
	Until  *time.Time
	Repo   string // a git_common_root or a bare cwd, per GroupKey
	Tag    string

	Hide    config.Hide // the standing rules; zero value applies nothing
	ShowAll bool        // true = apply no hide rule and show archived sessions
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
	Origin session.Origin
	// Client is the program the session was driven through, verbatim as
	// the source recorded it, or nil when the source records no such
	// thing. See session.ClientLabel for the short name shown in a row.
	Client *string

	DirExists    *bool // nil = never checked (no cwd known); false = missing
	MessageCount *int64
	// TranscriptPath is the session's on-disk transcript, for the sources
	// that keep one (claude, pi, omp). It is nil for the SQLite-backed
	// sources, which record no transcript file at all - a reader must
	// treat nil as "this source keeps no transcript", not as an error.
	TranscriptPath *string
	Resumable      bool
	Tags           []string
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
	s.started_at, s.last_activity_at, s.topic, s.name, s.last_prompt, s.end_state, s.origin, s.client, s.dir_exists, s.message_count, s.transcript_path, s.resumable,
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
	Client         *string `json:"client"`
	DirExists      *int64  `json:"dir_exists"`
	MessageCount   *int64  `json:"message_count"`
	TranscriptPath *string `json:"transcript_path"`
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
		SessionID:      row.ID,
		Source:         row.Source,
		LineageID:      row.LineageID,
		CWD:            row.CWD,
		GitBranch:      row.GitBranch,
		GitRepoRoot:    row.GitRepoRoot,
		GitCommonRoot:  row.GitCommonRoot,
		Topic:          row.Topic,
		Name:           row.Name,
		LastPrompt:     row.LastPrompt,
		EndState:       session.EndState(row.EndState),
		Origin:         origin,
		Client:         row.Client,
		MessageCount:   row.MessageCount,
		TranscriptPath: row.TranscriptPath,
		Resumable:      row.Resumable != 0,
		Archived:       row.Archived != 0,
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
// into a filter flag is trusted as SQL text). The hide clauses come from
// hideClausesFromFilter and are skipped wholesale when ShowAll is set.
func whereClauseFromFilter(f Filter, params map[string]any) (string, []string) {
	clauses := baseClausesFromFilter(f, params)
	clauses = append(clauses, hideClausesFromFilter(f, params)...)
	return "", clauses
}

// baseClausesFromFilter gathers the non-hide filter clauses (agent, since,
// until, repo, tag): the predicate every listing shares before any hide rule
// runs. The hidden-count queries are built from these clauses alone.
func baseClausesFromFilter(f Filter, params map[string]any) []string {
	var clauses []string
	if f.Agent != "" {
		params["agent"] = f.Agent
		clauses = append(clauses, "agent")
	}
	if f.Client != "" {
		params["client"] = f.Client
		params["client_raw"] = session.ClientRaw(f.Client)
		clauses = append(clauses, "client")
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
	return clauses
}

// hideClausesFromFilter gathers the hide-rule clauses: the archive flag and
// every standing rule in f.Hide, skipped wholesale when ShowAll is set. Each
// path pattern gets its own clause key (hide_path_0, hide_path_1, ...) so
// buildPredicate can AND them without knowing the count ahead of time.
func hideClausesFromFilter(f Filter, params map[string]any) []string {
	if f.ShowAll {
		return nil
	}
	var clauses []string
	clauses = append(clauses, "hide_archived")
	if f.Hide.NonInteractive {
		clauses = append(clauses, "hide_non_interactive")
	}
	if f.Hide.MinMessages > 0 {
		params["min_messages"] = f.Hide.MinMessages
		clauses = append(clauses, "hide_min_messages")
	}
	for i, pat := range f.Hide.Paths {
		key := fmt.Sprintf("hide_path_%d", i)
		params[key] = pat
		clauses = append(clauses, key)
	}
	return clauses
}

func buildPredicate(pf *sqlitex.ParamFile, clauses []string) string {
	var parts []string
	for _, c := range clauses {
		switch c {
		case "agent":
			parts = append(parts, "s.source = "+pf.Ref("agent"))
		case "client":
			// Either spelling matches: what the source recorded, or the
			// label a row shows for it. The two are compared, never
			// rewritten - a value with no known label resolves to itself,
			// so an unrecognised client is still filterable by its raw
			// name.
			parts = append(parts, "(s.client = "+pf.Ref("client")+" OR s.client = "+pf.Ref("client_raw")+")")
		case "since":
			parts = append(parts, "s.last_activity_at >= "+pf.Ref("since"))
		case "until":
			parts = append(parts, "s.last_activity_at <= "+pf.Ref("until"))
		case "repo":
			parts = append(parts, "(s.git_common_root = "+pf.Ref("repo")+" OR (s.git_common_root IS NULL AND s.cwd = "+pf.Ref("repo")+"))")
		case "tag":
			parts = append(parts, "EXISTS (SELECT 1 FROM tags tg WHERE tg.lineage_id = s.lineage_id AND tg.tag = "+pf.Ref("tag")+")")
		case "hide_archived":
			parts = append(parts, "l.archived_at IS NULL")
		case "hide_non_interactive":
			// Only the exact value 'automated' is hidden. 'unknown' must
			// NEVER be hidden - pi, omp and hermes report unknown for every
			// session, and hiding on absence of evidence would make three
			// sources vanish. A NULL origin reads back as unknown, so it is
			// guarded the same way rather than dropped by the comparison.
			parts = append(parts, "(s.origin IS NULL OR s.origin != 'automated')")
		case "hide_min_messages":
			// Unknown is not "small": a NULL message_count means the source
			// never recorded a count, and it must not be hidden by the
			// threshold.
			parts = append(parts, "(s.message_count IS NULL OR s.message_count >= "+pf.Ref("min_messages")+")")
		default:
			if strings.HasPrefix(c, "hide_path_") {
				// A NULL cwd matches no pattern and must not be hidden.
				// GLOB's '*' already crosses '/', so the '**' a config may
				// carry is collapsed to '*' at load and behaves identically
				// either way.
				parts = append(parts, "(s.cwd IS NULL OR s.cwd NOT GLOB "+pf.Ref(c)+")")
			}
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

// CountBase returns how many sessions match f's non-hide predicate - the
// user filters alone, before any hide rule or the archive flag runs. extra
// is appended to the WHERE clause verbatim so a caller can extend the base
// predicate with its own restriction (review counts only sessions needing
// attention); it is a program constant, never user input. The count is one
// COUNT(*) query, never a second full fetch of the rows, and is what lets
// the commands report what the hide rules suppressed.
func CountBase(db *sqlitex.Runner, f Filter, extra string) (int, error) {
	params := map[string]any{}
	clauses := baseClausesFromFilter(f, params)
	pf, err := db.WriteParams(params)
	if err != nil {
		return 0, err
	}
	defer pf.Close()

	predicate := buildPredicate(pf, clauses)
	if extra != "" {
		predicate += " AND " + extra
	}
	q := fmt.Sprintf(`SELECT COUNT(*) AS n FROM %s WHERE %s;`, itemFrom, predicate)

	var rows []struct {
		N int `json:"n"`
	}
	if err := db.Query(q, &rows); err != nil {
		return 0, fmt.Errorf("search: counting sessions: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].N, nil
}

// ListWithHidden returns the visible items and how many the hide rules and
// the archive flag suppressed: one COUNT(*) over the same predicate without
// the hide clauses, minus the visible count - never a second full fetch of
// the rows.
func ListWithHidden(db *sqlitex.Runner, f Filter) (items []Item, hidden int, err error) {
	items, err = List(db, f)
	if err != nil {
		return nil, 0, err
	}
	total, err := CountBase(db, f, "")
	if err != nil {
		return nil, 0, err
	}
	return items, total - len(items), nil
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
	if f.Client != "" {
		extra = append(extra, "client="+f.Client)
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
