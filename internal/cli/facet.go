// Facets are the dimensions the browser slices sessions by - agent,
// repository, tag - each one a side panel with live counts (change
// lazy-style-browser, design.md decision 2). They are derived in this
// process from one unfiltered result set rather than re-queried per
// selection: a panel has to show what *would* be selected and how many
// sessions are behind it, which is a set of counts over the whole corpus,
// not the single filtered listing a WHERE clause returns. At a few hundred
// sessions per profile that is a map walk, and it means the browser holds
// exactly one query result instead of one per panel.
package cli

import (
	"sort"
	"strings"

	"github.com/rfist/lazyrecall/internal/search"
)

// facetRow is one selectable value in a facet panel. The zero Value is the
// synthetic "all" row every facet carries at the top: selecting it clears
// that facet, which is why clearing a filter needs no separate key.
type facetRow struct {
	Value string // "" for the "all" row
	Label string // what is drawn (Value, abbreviated/prettified)
	Count int
}

// facet is one side panel's state: the rows it currently offers, where the
// cursor sits in them, and which value is actually applied. Selection
// (Sel) is deliberately separate from the cursor - moving through a panel
// previews nothing and changes nothing until Enter, so an accidental
// keystroke can never silently re-filter the session list.
type facet struct {
	rows   []facetRow
	cursor int
	top    int
	Sel    string // "" == no filter from this facet

	// filter narrows which rows the panel offers (the "/" action). It is a
	// display-side narrowing only: it never changes Sel, so narrowing a
	// panel to find a value cannot by itself change what the session list
	// shows. A repository panel on a busy machine has dozens of rows and
	// is the reason this exists.
	filter string
}

// index returns the row the cursor is on, or nil when the panel is empty.
func (f *facet) index() *facetRow {
	if f.cursor >= 0 && f.cursor < len(f.rows) {
		return &f.rows[f.cursor]
	}
	return nil
}

// setRows replaces the panel's rows and re-anchors the cursor on the value
// that is currently applied, so a panel that is rebuilt underneath the user
// (another facet changed, the index refreshed) does not silently jump the
// cursor onto an unrelated row.
func (f *facet) setRows(rows []facetRow) {
	if f.filter != "" {
		// Literal substring, case-insensitive, in the order the rows
		// already had - the same rule the session list narrows by (change
		// literal-substring-filter). A repository list is the one place a
		// subsequence match could be defended, since these rows are paths
		// and paths are what fuzzy finders are for; it is not defended
		// here, because "/" is one gesture and it should not mean two
		// different things depending on which panel has focus.
		needle := strings.ToLower(f.filter)
		narrowed := make([]facetRow, 0, len(rows))
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Label), needle) {
				narrowed = append(narrowed, r)
			}
		}
		rows = narrowed
	}
	f.rows = rows
	for i, r := range rows {
		if r.Value == f.Sel {
			f.cursor = i
			return
		}
	}
	// The applied value no longer exists in this facet - it was filtered
	// away by another panel, or its last session is gone. Fall back to the
	// "all" row rather than leaving the cursor pointing past the end.
	f.cursor = 0
}

// countBy builds a facet's rows from items, using keysOf to say which
// values each item contributes to (one for agent and repository, zero or
// many for tags). Rows are ordered by descending count then by value, so
// the panel leads with where the sessions actually are, and label renders
// a value for display.
func countBy(items []search.Item, allLabel string, keysOf func(search.Item) []string, label func(string) string) []facetRow {
	counts := map[string]int{}
	for _, it := range items {
		for _, k := range keysOf(it) {
			if k == "" {
				continue
			}
			counts[k]++
		}
	}
	rows := make([]facetRow, 0, len(counts)+1)
	rows = append(rows, facetRow{Value: "", Label: allLabel, Count: len(items)})
	rest := make([]facetRow, 0, len(counts))
	for k, n := range counts {
		l := k
		if label != nil {
			l = label(k)
		}
		rest = append(rest, facetRow{Value: k, Label: l, Count: n})
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].Count != rest[j].Count {
			return rest[i].Count > rest[j].Count
		}
		return rest[i].Value < rest[j].Value
	})
	return append(rows, rest...)
}

// installKey / repoKey / tagKeys are the three facets' key functions.
//
// installKey facets the Agents panel by install rather than by bare source
// (change group-sessions-in-one-index): two Claude accounts now read as two
// rows ("cc", "ccp" once labelled) instead of collapsing into one "claude"
// row, which is what made "which account" unanswerable once one index held
// every install together. See matchesFacets for how a row's Value is
// matched back against an item.
//
// it.Install falls back to it.Source when empty - a row a refresh has not
// yet stamped with an install (schema.go's sessions.install predates this
// change and can be blank on an unmigrated row) still gets a facet key
// instead of silently vanishing from the panel.
//
// repoKey is Item.GroupKey - the same canonical repo root the repository
// filter and the grouped listing already use (internal/search), so a
// repository panel row means exactly what `--repo` means and there is no
// second notion of "which repo is this session in".
func installKey(it search.Item) []string {
	if it.Install != "" {
		return []string{it.Install}
	}
	return []string{it.Source}
}

func repoKey(it search.Item) []string {
	key, _ := it.GroupKey()
	return []string{key}
}

func tagKeys(it search.Item) []string { return it.Tags }

// matchesFacets reports whether it satisfies every applied facet value.
// Passing "" for a facet means that facet is not applied, which is the
// same shape the "all" row produces - so "no filter" needs no special case
// anywhere else.
//
// install and source are two separate dimensions of the same Agents panel,
// never conflated (change group-sessions-in-one-index, fixing a leak found
// against real data): install narrows to one specific discovered install -
// a panel row's own Value, matched against it.Install with a fallback to
// it.Source only for a row a refresh has not yet stamped with an install
// (see installKey) - while source narrows to every install of one source,
// which is what a bare `--agent=claude` seed means (resolveAgentFilter's
// Agent return) and which no single install row can express once two
// installs of the same source exist. Matching install against
// "it.Install == v OR it.Source == v" - the browser's first attempt - is
// wrong, not just imprecise: when one install is literally named the same
// as its own multi-install source (e.g. "claude" alongside
// "claude-personal", installName's ordinary result), selecting
// that one install's row matched every session of the shared source too,
// because the OR's source half never distinguished "this install" from
// "every install of this source". Keeping the two as separate parameters
// - set from separate model state (browseModel.agentSource vs
// browseModel.agents.Sel) - is what removes the ambiguity instead of
// papering over it.
func matchesFacets(it search.Item, install, source, repo, tag string) bool {
	if install != "" {
		got := it.Install
		if got == "" {
			got = it.Source
		}
		if got != install {
			return false
		}
	}
	if source != "" && it.Source != source {
		return false
	}
	if repo != "" {
		key, _ := it.GroupKey()
		if key != repo {
			return false
		}
	}
	if tag != "" {
		found := false
		for _, t := range it.Tags {
			if t == tag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// narrow returns the items matching the given facet values, in the order
// they were loaded (most recently active first, as the query returned them).
func narrow(items []search.Item, install, source, repo, tag string) []search.Item {
	out := make([]search.Item, 0, len(items))
	for _, it := range items {
		if matchesFacets(it, install, source, repo, tag) {
			out = append(out, it)
		}
	}
	return out
}
