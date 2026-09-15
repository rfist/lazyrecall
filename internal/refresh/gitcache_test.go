package refresh

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/gitutil"
	"github.com/rfist/lazyrecall/internal/profile"
)

// This file covers the git-resolution caching fix: a refresh used to call
// gitutil.Resolve once per session (not once per distinct directory), and
// never cached a "not a git repository" result across passes at all - so a
// non-repo CWD shared by many sessions cost one git subprocess call per
// session, on every single refresh, forever (measured on real data: 95
// calls a pass for one directory; devdocs/fyi.md). These tests use the
// Refresher.GitResolve seam instead of a real git binary, both for speed
// and so the resolver's own call count can be asserted directly.

// countingResolver is a fake git resolver that records every directory it
// was asked to resolve, split out so each test can also make assertions on
// which specific directories were (or were not) re-asked.
type countingResolver struct {
	calls  []string
	answer map[string]gitutil.Info // dir -> Info to return with ok=true; absent means ok=false
}

func newCountingResolver() *countingResolver {
	return &countingResolver{answer: map[string]gitutil.Info{}}
}

func (c *countingResolver) resolve(dir string) (gitutil.Info, bool) {
	c.calls = append(c.calls, dir)
	info, ok := c.answer[dir]
	return info, ok
}

func (c *countingResolver) countFor(dir string) int {
	n := 0
	for _, d := range c.calls {
		if d == dir {
			n++
		}
	}
	return n
}

// writeClaudeSession lays out one synthetic Claude Code session transcript
// under root/projects/<projectDir>/<id>.jsonl, recording cwd so refresh has
// something to resolve git identity against. The project directory name is
// never decoded by the adapter (claude.go's own doc comment) - it exists
// only so the file can be found.
func writeClaudeSession(t *testing.T, root, projectDir, id, cwd, prompt string) {
	t.Helper()
	path := filepath.Join(root, "projects", projectDir, id+".jsonl")
	content := `{"type":"user","message":{"role":"user","content":"` + prompt + `"},"cwd":"` + cwd + `","timestamp":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:05Z"}` + "\n"
	writeFile(t, path, content)
}

// newGitCacheTestRefresher builds a Refresher over one claude install with a
// resolver seam, so real git never runs.
func newGitCacheTestRefresher(t *testing.T, claudeRoot string) (*Refresher, *countingResolver) {
	t.Helper()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	p := profile.Profile{Name: "claude", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New([]profile.Profile{p}, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	resolver := newCountingResolver()
	r.GitResolve = resolver.resolve
	return r, resolver
}

// TestGitResolveOncePerDistinctCWDPerPass covers the per-pass half of the
// fix: several sessions sharing one non-repo CWD must cost the resolver one
// call, not one per session, in a single refresh pass.
func TestGitResolveOncePerDistinctCWDPerPass(t *testing.T) {
	claudeRoot := t.TempDir()
	nonRepoDir := t.TempDir() // a real directory the fake resolver reports as not a git repo (absent from resolver.answer)

	for i, id := range []string{"s1", "s2", "s3"} {
		writeClaudeSession(t, claudeRoot, "proj", id, nonRepoDir, "prompt "+id)
		_ = i
	}

	r, resolver := newGitCacheTestRefresher(t, claudeRoot)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	if got := resolver.countFor(nonRepoDir); got != 1 {
		t.Errorf("resolver called %d times for one shared non-repo CWD across 3 sessions in one pass, want 1 (calls: %v)", got, resolver.calls)
	}

	var rows []struct {
		GitCommonRoot *string `json:"git_common_root"`
		GitResolved   int64   `json:"git_resolved"`
	}
	if err := r.DB.Query(`SELECT git_common_root, git_resolved FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d session rows, want 3", len(rows))
	}
	for _, row := range rows {
		if row.GitCommonRoot != nil {
			t.Errorf("git_common_root = %v, want nil for a non-repo directory", *row.GitCommonRoot)
		}
		if row.GitResolved == 0 {
			t.Errorf("git_resolved = 0, want the attempt recorded even though it found nothing")
		}
	}
}

// TestGitResolveSkippedOnNoChangeSecondPass is the cross-pass half of the
// fix, and the one the old code could never do at all: a second refresh
// with nothing changed must not re-ask git about a directory it already
// confirmed - including the negative "not a git repository" result, which
// used to have no cache to fall back on (git_common_root reads back nil
// either way, whether or not it was ever asked).
func TestGitResolveSkippedOnNoChangeSecondPass(t *testing.T) {
	claudeRoot := t.TempDir()
	nonRepoDir := t.TempDir()
	writeClaudeSession(t, claudeRoot, "proj", "s1", nonRepoDir, "hello")
	writeClaudeSession(t, claudeRoot, "proj", "s2", nonRepoDir, "world")

	r, resolver := newGitCacheTestRefresher(t, claudeRoot)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.countFor(nonRepoDir); got != 1 {
		t.Fatalf("first pass: resolver called %d times, want 1", got)
	}

	resolver.calls = nil
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := len(resolver.calls); got != 0 {
		t.Errorf("second no-change pass called the git resolver %d times, want 0 (calls: %v)", got, resolver.calls)
	}
}

// TestGitResolveRepoDirCachedAcrossPasses covers the positive case
// symmetrically: a real repo's identity is resolved once and then reused,
// never re-asked while nothing changes.
func TestGitResolveRepoDirCachedAcrossPasses(t *testing.T) {
	claudeRoot := t.TempDir()
	repoDir := t.TempDir()
	writeClaudeSession(t, claudeRoot, "proj", "s1", repoDir, "hello")
	writeClaudeSession(t, claudeRoot, "proj", "s2", repoDir, "world")

	r, resolver := newGitCacheTestRefresher(t, claudeRoot)
	resolver.answer[repoDir] = gitutil.Info{RepoRoot: repoDir, CommonRoot: repoDir}

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.countFor(repoDir); got != 1 {
		t.Fatalf("first pass: resolver called %d times for 2 sessions sharing a repo CWD, want 1", got)
	}

	var rows []struct {
		GitCommonRoot *string `json:"git_common_root"`
	}
	if err := r.DB.Query(`SELECT git_common_root FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.GitCommonRoot == nil || *row.GitCommonRoot != repoDir {
			t.Errorf("git_common_root = %v, want %q", row.GitCommonRoot, repoDir)
		}
	}

	resolver.calls = nil
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := len(resolver.calls); got != 0 {
		t.Errorf("second no-change pass called the git resolver %d times, want 0", got)
	}
}

// TestGitResolveRereadsOnReplacedTranscript covers the first re-resolve
// trigger the fix must keep: a rewritten transcript's CWD evidence is
// stale, so its cached git identity must not be trusted forward even though
// git_resolved was already set.
func TestGitResolveRereadsOnReplacedTranscript(t *testing.T) {
	claudeRoot := t.TempDir()
	dirA := t.TempDir()
	writeClaudeSession(t, claudeRoot, "proj", "s1", dirA, "hello")

	r, resolver := newGitCacheTestRefresher(t, claudeRoot)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.countFor(dirA); got != 1 {
		t.Fatalf("first pass: resolver called %d times, want 1", got)
	}

	// Rewrite the transcript from scratch with different leading bytes - a
	// same-source compaction-in-place, the case transcriptRewritten's
	// leadingBytesChanged check exists to catch (pipeline.go).
	writeClaudeSession(t, claudeRoot, "proj", "s1", dirA, "a completely different opening prompt")

	resolver.calls = nil
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.countFor(dirA); got != 1 {
		t.Errorf("pass after a replaced transcript: resolver called %d times for the same CWD, want 1 (must not trust the stale cache)", got)
	}
}

// TestGitResolveRereadsOnChangedCWD covers the second re-resolve trigger:
// an incremental delta that records a different CWD than last time (a
// session resumed from elsewhere) must not reuse the old CWD's cached git
// identity for the new one.
func TestGitResolveRereadsOnChangedCWD(t *testing.T) {
	claudeRoot := t.TempDir()
	dirA := t.TempDir()
	dirB := t.TempDir()
	transcriptPath := filepath.Join(claudeRoot, "projects", "proj", "s1.jsonl")
	writeFile(t, transcriptPath,
		`{"type":"user","message":{"role":"user","content":"hello"},"cwd":"`+dirA+`","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	r, resolver := newGitCacheTestRefresher(t, claudeRoot)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.countFor(dirA); got != 1 {
		t.Fatalf("first pass: resolver called %d times for dirA, want 1", got)
	}

	// Append a new exchange recording a different cwd - an incremental
	// delta, not a rewrite, so transcriptRewritten stays false and only the
	// changed-CWD check can catch this.
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","message":{"role":"user","content":"one more thing"},"cwd":"` + dirB + `","timestamp":"2026-01-01T00:01:00Z"}` + "\n")
	f.Close()

	resolver.calls = nil
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.countFor(dirB); got != 1 {
		t.Errorf("pass after the recorded CWD changed: resolver called %d times for the new CWD, want 1 (calls: %v)", got, resolver.calls)
	}
	if got := resolver.countFor(dirA); got != 0 {
		t.Errorf("pass after the recorded CWD changed: resolver called %d times for the old CWD, want 0", got)
	}
}

// TestGitResolveNonRepoNeverAppearsAsRepo guards the representation choice
// itself: a confirmed non-repo session's git_repo_root/git_common_root must
// stay NULL, never an empty-string sentinel that Repos grouping or --repo
// filtering could mistake for a real (empty-named) repository.
func TestGitResolveNonRepoNeverAppearsAsRepo(t *testing.T) {
	claudeRoot := t.TempDir()
	nonRepoDir := t.TempDir()
	writeClaudeSession(t, claudeRoot, "proj", "s1", nonRepoDir, "hello")

	r, _ := newGitCacheTestRefresher(t, claudeRoot)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	// Refresh again so the cached (git_resolved, no-op) path also gets
	// covered, not just the first, freshly-resolved pass.
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		GitRepoRoot   *string `json:"git_repo_root"`
		GitCommonRoot *string `json:"git_common_root"`
	}
	if err := r.DB.Query(`SELECT git_repo_root, git_common_root FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.GitRepoRoot != nil {
			t.Errorf("git_repo_root = %q, want NULL, not an empty-string sentinel", *row.GitRepoRoot)
		}
		if row.GitCommonRoot != nil {
			t.Errorf("git_common_root = %q, want NULL, not an empty-string sentinel", *row.GitCommonRoot)
		}
	}

	// And directly against the same DB rows a search/group-by-repo query
	// would see: a non-repo session must never surface as a repo named "".
	var emptyRepoCount []struct {
		N int `json:"n"`
	}
	if err := r.DB.Query(`SELECT COUNT(*) AS n FROM sessions WHERE git_common_root = '';`, &emptyRepoCount); err != nil {
		t.Fatal(err)
	}
	if len(emptyRepoCount) != 1 || emptyRepoCount[0].N != 0 {
		t.Errorf("found %d session(s) with git_common_root = '' (empty-string sentinel), want 0", emptyRepoCount[0].N)
	}
}
