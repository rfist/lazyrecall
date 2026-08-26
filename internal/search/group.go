package search

import (
	"path/filepath"
	"sort"
)

// RepoGroup is one place sessions happened: a git repository (possibly with
// worktrees nested under it) or, outside any repository, a bare working
// directory (spec session-search, "Grouping by repository and worktree").
type RepoGroup struct {
	// Key is the group identity: a GitCommonRoot for a real repository, or
	// a bare CWD when the session is not inside a git repository.
	Key    string
	IsRepo bool

	// Direct holds sessions whose own working directory *is* Key - the
	// repository's main worktree, or the bare directory itself.
	Direct []Item

	// Worktrees holds sessions from linked worktrees of this repository,
	// keyed by worktree name (the worktree's own directory basename) and
	// nested beneath the repository rather than listed as unrelated
	// locations (spec, "each worktree is identified by name").
	Worktrees map[string][]Item
}

// GroupByRepo groups items by repository, nesting worktree sessions under
// their repository (task 7.5). Sessions with no working directory at all
// (never recorded by their source) are returned separately, since they
// cannot be placed by directory or by repository.
func GroupByRepo(items []Item) (groups []RepoGroup, unplaced []Item) {
	byKey := map[string]*RepoGroup{}
	var order []string

	for _, it := range items {
		key, isRepo := it.GroupKey()
		if key == "" {
			unplaced = append(unplaced, it)
			continue
		}
		g, ok := byKey[key]
		if !ok {
			g = &RepoGroup{Key: key, IsRepo: isRepo, Worktrees: map[string][]Item{}}
			byKey[key] = g
			order = append(order, key)
		}
		if !isRepo {
			g.Direct = append(g.Direct, it)
			continue
		}
		// A repo-grouped item is either the main worktree (its own
		// GitRepoRoot equals the group's CommonRoot key) or a linked
		// worktree (GitRepoRoot differs) - only the latter is nested under
		// a worktree name.
		if it.GitRepoRoot != nil && *it.GitRepoRoot != key {
			name := worktreeName(*it.GitRepoRoot)
			g.Worktrees[name] = append(g.Worktrees[name], it)
		} else {
			g.Direct = append(g.Direct, it)
		}
	}

	sort.Strings(order)
	for _, k := range order {
		groups = append(groups, *byKey[k])
	}
	return groups, unplaced
}

func worktreeName(path string) string {
	return filepath.Base(path)
}
