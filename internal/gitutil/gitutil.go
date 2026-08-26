// Package gitutil resolves the git identity of a working directory: its
// own worktree root, and the canonical repository it belongs to, so
// sessions from a repository's worktrees can be grouped together (spec
// session-search, "Grouping by repository and worktree"). It only ever
// reads (rev-parse), never writes.
package gitutil

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// Info is one working directory's git identity.
type Info struct {
	// RepoRoot is this working directory's own worktree root - what "git
	// rev-parse --show-toplevel" returns. For the main worktree this is
	// also the canonical repo root; for a linked worktree it is that
	// worktree's own directory, not the main repo's.
	RepoRoot string

	// CommonRoot is the canonical identity shared by a repository's main
	// worktree and every worktree linked to it - the parent of the shared
	// ".git" directory ("git rev-parse --git-common-dir"). Sessions should
	// be grouped by this, not by RepoRoot, so worktrees nest under one
	// repository instead of appearing as unrelated locations.
	CommonRoot string

	// IsWorktree is true when RepoRoot != CommonRoot - a linked worktree
	// rather than a repository's main working copy.
	IsWorktree bool
}

// Resolve runs read-only git plumbing against dir and reports its
// repository identity. ok is false when dir is not inside a git repository
// at all (spec session-search, "Session not in a git repository").
func Resolve(dir string) (Info, bool) {
	root, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return Info{}, false
	}

	commonDir, err := run(dir, "rev-parse", "--git-common-dir")
	if err != nil || commonDir == "" {
		return Info{RepoRoot: root, CommonRoot: root}, true
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir = filepath.Clean(commonDir)

	commonRoot := filepath.Dir(commonDir) // parent of the shared ".git"
	info := Info{RepoRoot: root, CommonRoot: commonRoot}
	info.IsWorktree = commonRoot != root
	return info, true
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
