// Package review answers "what did I leave unfinished" (spec
// session-review): classifying how each session ended is done during
// refresh (internal/transcript, internal/adapter/hermes); this package
// only reports the sessions that need attention.
package review

import (
	"fmt"

	"lazyrecall/internal/search"
	"lazyrecall/internal/session"
	"lazyrecall/internal/sqlitex"
)

// Entry is one unfinished session in the loose-ends report (spec
// session-review, "Report identifies where to resume": agent, working
// directory, and what the session was last doing).
type Entry struct {
	search.Item
}

// Report returns every session in the active profile whose end state is
// dangling, interrupted, or abandoned, most recently active first (spec
// session-review, "Loose ends report"). Sessions whose end state is
// completed are excluded entirely - not merely sorted last.
func Report(db *sqlitex.Runner) ([]Entry, error) {
	items, err := search.List(db, search.Filter{})
	if err != nil {
		return nil, fmt.Errorf("review: listing sessions: %w", err)
	}
	var out []Entry
	for _, it := range items {
		if it.EndState.NeedsAttention() {
			out = append(out, Entry{Item: it})
		}
	}
	return out, nil
}

// EmptyMessage is shown when nothing needs attention (spec session-review,
// "Nothing is unfinished": "the report states that no sessions need
// attention rather than returning an empty output with no explanation").
func EmptyMessage(profileName string) string {
	return fmt.Sprintf("No sessions need attention in profile %q - nothing is dangling, interrupted, or abandoned.", profileName)
}

// StateLabel renders an end state for display in the report.
func StateLabel(s session.EndState) string {
	return string(s)
}
