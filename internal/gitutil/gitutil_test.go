package gitutil

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestResolveNotAGitRepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	_, ok := Resolve(dir)
	if ok {
		t.Fatal("expected ok=false for a plain directory")
	}
}

func TestResolveWorktreesShareCommonRoot(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	main := filepath.Join(root, "main")
	runGit(t, root, "init", "-q", "-b", "main", main)
	runGit(t, main, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")

	wt := filepath.Join(root, "wt1")
	runGit(t, main, "worktree", "add", "-q", wt, "-b", "wt1-branch")

	mainInfo, ok := Resolve(main)
	if !ok {
		t.Fatal("expected main worktree to resolve")
	}
	if mainInfo.IsWorktree {
		t.Error("the main worktree must not be reported as a linked worktree")
	}

	wtInfo, ok := Resolve(wt)
	if !ok {
		t.Fatal("expected linked worktree to resolve")
	}
	if !wtInfo.IsWorktree {
		t.Error("expected the linked worktree to be reported as such")
	}
	if wtInfo.RepoRoot == mainInfo.RepoRoot {
		t.Error("a linked worktree's own root must differ from the main worktree's root")
	}
	if wtInfo.CommonRoot != mainInfo.CommonRoot {
		t.Errorf("worktrees of the same repo must share a CommonRoot: main=%q wt=%q", mainInfo.CommonRoot, wtInfo.CommonRoot)
	}
}
