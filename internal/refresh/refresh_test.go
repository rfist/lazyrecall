package refresh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/search"
	"github.com/rfist/lazyrecall/internal/session"
)

// All fixtures below are synthetic, hand-written test data - no real
// session content anywhere in this file.

func sqlite3Path(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildTestProfile lays out synthetic claude, pi, and omp session data
// under a temp HOME plus lazyrecall's own data dir, and returns a Profile
// pointing at all of it (hermes is deliberately left unconfigured in most
// tests - its own package covers its adapter behavior, and DB-per-profile
// isolation is what this suite is verifying).
func buildTestProfile(t *testing.T) (profile.Profile, string) {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"help me fix the flaky retry test","pastedContents":{},"timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n")
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"help me fix the flaky retry test"},"cwd":"/work/repo","gitBranch":"main","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed the retry loop."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	return p, sqlite3Path(t)
}

func TestFullBuildIndexesClaudeSession(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := r.Refresh(Options{})
	if err != nil {
		t.Fatal(err)
	}

	var claudeStatus *SourceStatus
	for i := range sum.Sources {
		if sum.Sources[i].Source == "claude" {
			claudeStatus = &sum.Sources[i]
		}
	}
	if claudeStatus == nil || !claudeStatus.Available || claudeStatus.Sessions != 1 {
		t.Fatalf("claude status = %+v", claudeStatus)
	}

	var rows []struct {
		ID       string `json:"id"`
		EndState string `json:"end_state"`
		CWD      string `json:"cwd"`
		Topic    string `json:"topic"`
	}
	if err := r.DB.Query(`SELECT id, end_state, cwd, topic FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d session rows, want 1", len(rows))
	}
	if rows[0].EndState != string(session.EndStateCompleted) {
		t.Errorf("end_state = %q, want completed", rows[0].EndState)
	}
	if rows[0].CWD != "/work/repo" {
		t.Errorf("cwd = %q", rows[0].CWD)
	}

	// Tier 2: the prompt from history.jsonl must be searchable.
	var hits []struct {
		SessionID string `json:"session_id"`
	}
	if err := r.DB.Query(`SELECT session_id FROM prompt_fts WHERE prompt_fts MATCH 'flaky';`, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the claude prompt to be searchable, got %d hits", len(hits))
	}
}

func TestIncrementalRefreshOnlyReadsWhatChanged(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	// Append a new prompt to the same session, as a live agent would.
	transcriptPath := filepath.Join(p.Roots["claude"], "projects", "-work-repo", "c1.jsonl")
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","message":{"role":"user","content":"one more thing"},"cwd":"/work/repo","timestamp":"2026-01-01T00:01:00Z"}` + "\n")
	f.Close()

	sum, err := r.Refresh(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sum.Sources {
		if s.Source == "claude" && s.Sessions != 1 {
			t.Errorf("expected 1 claude session still, got %d", s.Sessions)
		}
	}

	var rows []struct {
		EndState string `json:"end_state"`
	}
	if err := r.DB.Query(`SELECT end_state FROM sessions WHERE id LIKE 'claude:%';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EndState != string(session.EndStateDangling) {
		t.Fatalf("after appending an unanswered prompt, expected dangling, got %+v", rows)
	}
}

func TestFullRebuildOption(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := r.DB.Query(`SELECT id FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("full rebuild should still find exactly 1 session, got %d", len(rows))
	}
}

func TestMissingSourceDoesNotBlockOthers(t *testing.T) {
	// A profile with only a pi root configured (no claude, omp, hermes):
	// refresh must still succeed and report the others unavailable rather
	// than failing outright (spec session-index, "Some agents are not
	// installed"; task 5.6).
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	piRoot := filepath.Join(home, "pi-home")
	writeFile(t, filepath.Join(piRoot, "agent", "sessions", "--x--", "2026-01-01T00-00-00-000Z_p1.jsonl"),
		`{"type":"session","cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"synthetic prompt"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	p := profile.Profile{Name: "default", Roots: map[string]string{"pi": piRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := r.Refresh(Options{})
	if err != nil {
		t.Fatalf("refresh must not fail just because claude/omp/hermes are absent: %v", err)
	}
	var piAvailable, othersUnavailable bool
	unavailableCount := 0
	for _, s := range sum.Sources {
		if s.Source == "pi" {
			piAvailable = s.Available && s.Sessions == 1
		} else if !s.Available {
			unavailableCount++
		}
	}
	othersUnavailable = unavailableCount == 3
	if !piAvailable {
		t.Errorf("expected pi to be indexed, got %+v", sum.Sources)
	}
	if !othersUnavailable {
		t.Errorf("expected the other 3 sources reported unavailable, got %+v", sum.Sources)
	}

	// pi has no prompt index: tier 2 must fall back to the transcript.
	var hits []struct {
		SessionID string `json:"session_id"`
	}
	if err := r.DB.Query(`SELECT session_id FROM prompt_fts WHERE prompt_fts MATCH 'synthetic';`, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected pi's transcript-extracted prompt to be searchable via fallback, got %d hits", len(hits))
	}
}

func TestOrphanedLineageRetainsAnnotations(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var lineages []struct {
		ID string `json:"id"`
	}
	if err := r.DB.Query(`SELECT id FROM lineages;`, &lineages); err != nil {
		t.Fatal(err)
	}
	if len(lineages) != 1 {
		t.Fatalf("expected 1 lineage, got %d", len(lineages))
	}
	lineageID := lineages[0].ID

	// Attach an annotation directly (annotate package covers the public
	// API; this test only needs a row to exist).
	if err := r.DB.Exec(`INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('` + lineageID + `', 'keep me', 1, 1);`); err != nil {
		t.Fatal(err)
	}

	// The session disappears from its source entirely.
	if err := os.RemoveAll(filepath.Join(p.Roots["claude"], "projects")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Roots["claude"], "history.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var afterLineages []struct {
		ID       string `json:"id"`
		Orphaned int    `json:"orphaned"`
	}
	if err := r.DB.Query(`SELECT id, orphaned FROM lineages;`, &afterLineages); err != nil {
		t.Fatal(err)
	}
	if len(afterLineages) != 1 || afterLineages[0].ID != lineageID || afterLineages[0].Orphaned != 1 {
		t.Fatalf("expected the lineage to be retained and marked orphaned, got %+v", afterLineages)
	}

	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.DB.Query(`SELECT body FROM comments WHERE lineage_id = '`+lineageID+`';`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Fatalf("expected the annotation to survive orphaning, got %+v", comments)
	}
}

// TestAnnotationsSurviveFullRebuildForAStillPresentSession covers spec
// session-annotations, "Session is re-indexed": a lineage id must be
// deterministic across rebuilds so annotations on a session that is still
// present in its source stay attached - not just retained-as-orphaned.
func TestAnnotationsSurviveFullRebuildForAStillPresentSession(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var before []struct {
		ID string `json:"lineage_id"`
	}
	if err := r.DB.Query(`SELECT lineage_id FROM sessions;`, &before); err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("got %d sessions", len(before))
	}
	lineageID := before[0].ID
	if err := r.DB.Exec(`INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('` + lineageID + `', 'still here', 1, 1);`); err != nil {
		t.Fatal(err)
	}

	// A full rebuild re-derives everything from scratch, but the session
	// is still present in its source (nothing was deleted this time).
	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}

	var after []struct {
		LineageID string `json:"lineage_id"`
	}
	if err := r.DB.Query(`SELECT lineage_id FROM sessions;`, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].LineageID != lineageID {
		t.Fatalf("expected the same lineage id to be re-derived, got %+v (was %q)", after, lineageID)
	}

	var lineages []struct {
		Orphaned int `json:"orphaned"`
	}
	if err := r.DB.Query(`SELECT orphaned FROM lineages WHERE id = '`+lineageID+`';`, &lineages); err != nil {
		t.Fatal(err)
	}
	if len(lineages) != 1 || lineages[0].Orphaned != 0 {
		t.Fatalf("a still-present session's lineage must not be orphaned, got %+v", lineages)
	}

	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.DB.Query(`SELECT body FROM comments WHERE lineage_id = '`+lineageID+`';`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "still here" {
		t.Fatalf("expected the comment to remain attached, got %+v", comments)
	}
}

// TestPiSourceSessionIDComesFromRecordNotFileName covers change
// fix-resume-session-identity, design.md decision 1: the transcript's file
// name carries a timestamp prefix pi's own "id" field does not, and the
// value LazyRecall stores (and would hand to `pi --resume`) must be the bare id
// alone.
func TestPiSourceSessionIDComesFromRecordNotFileName(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	piRoot := filepath.Join(home, "pi-home")
	// The file name is a timestamp joined to the id; the id the record
	// itself carries is only the second half.
	writeFile(t, filepath.Join(piRoot, "agent", "sessions", "--x--", "2026-01-01T00-00-00-000Z_019f9009-af52-781d-a202-5c5927ec2c4c.jsonl"),
		`{"type":"session","id":"019f9009-af52-781d-a202-5c5927ec2c4c","cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	p := profile.Profile{Name: "default", Roots: map[string]string{"pi": piRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID              string `json:"id"`
		SourceSessionID string `json:"source_session_id"`
	}
	if err := r.DB.Query(`SELECT id, source_session_id FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions, want 1", len(rows))
	}
	if rows[0].SourceSessionID != "019f9009-af52-781d-a202-5c5927ec2c4c" {
		t.Errorf("source_session_id = %q, want the bare record id, not the file-name stem", rows[0].SourceSessionID)
	}
	if rows[0].ID != "pi:default:019f9009-af52-781d-a202-5c5927ec2c4c" {
		t.Errorf("id = %q, want LazyRecall's composite id to reflect the corrected identifier", rows[0].ID)
	}
}

// TestIncrementalRefreshPreservesCorrectedIdentifierAndCursor covers a
// landmine found while implementing fix-resume-session-identity: the
// session-record line carrying pi's own id is only ever the first line of
// the file, so an incremental refresh (fromOffset > 0) never sees it again
// and must carry the correction forward from the previous pass's stored
// row - and the cursor bookkeeping key must stay stable across the
// correction, or every subsequent pass would re-scan the whole file from
// byte 0 forever, double-counting message_count each time.
func TestIncrementalRefreshPreservesCorrectedIdentifierAndCursor(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	piRoot := filepath.Join(home, "pi-home")
	transcriptPath := filepath.Join(piRoot, "agent", "sessions", "--x--", "2026-01-01T00-00-00-000Z_019f9009-af52-781d-a202-5c5927ec2c4c.jsonl")
	// The first line is padded past leadingBytesHash's 256-byte window
	// (leadingBytesChanged, task 6.2's rewrite detector) deliberately: a
	// file at or under that window's size sees its "leading bytes" grow
	// (not just extend) the moment a later append pushes the total past
	// 256 bytes, which leadingBytesChanged reads as a rewrite even though
	// nothing before the append point actually changed - a real, pre-existing
	// defect independent of this change, recorded in devdocs/fyi.md rather
	// than fixed here (out of scope for fix-resume-session-identity). Padding
	// keeps this test about what it is actually verifying instead of tripping
	// over that unrelated defect.
	pad := strings.Repeat("x", 300)
	writeFile(t, transcriptPath,
		`{"type":"session","id":"019f9009-af52-781d-a202-5c5927ec2c4c","cwd":"/x","timestamp":"2026-01-01T00:00:00Z","pad":"`+pad+`"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	p := profile.Profile{Name: "default", Roots: map[string]string{"pi": piRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	// A live agent appends more turns; the next pass only reads the delta,
	// never touching byte 0 again.
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"stop"},"timestamp":"2026-01-01T00:00:02Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		SourceSessionID string `json:"source_session_id"`
		MessageCount    int64  `json:"message_count"`
	}
	if err := r.DB.Query(`SELECT source_session_id, message_count FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions, want 1 (a second row would mean the composite id drifted across passes)", len(rows))
	}
	if rows[0].SourceSessionID != "019f9009-af52-781d-a202-5c5927ec2c4c" {
		t.Errorf("source_session_id = %q after an incremental pass, want the corrected id to survive without re-reading the header line", rows[0].SourceSessionID)
	}
	// Every recognized record counts, including the "session" meta record
	// itself (transcript.Scan increments RecordCount for any classified
	// record, regardless of Kind): 2 from the first pass (session + user
	// message) + 1 from the incremental pass (assistant message) = 3. If
	// the cursor key had drifted, the second pass would re-scan the whole
	// file from byte 0 and this would be 2 (pass 1) + 3 (pass 2's full
	// re-read) = 5.
	if rows[0].MessageCount != 3 {
		t.Errorf("message_count = %d, want 3 - a higher count means the file was re-scanned from the start instead of incrementally", rows[0].MessageCount)
	}
}

// TestFullRebuildCorrectsPiIdentifierAndMigratesAnnotations covers task 4.2:
// a full rebuild that corrects a pi session's SourceSessionID also changes
// its lineage id (a non-continuing session's lineage root is its own
// SourceSessionID - design.md decision 8), so comments/tags filed under the
// old (file-name-derived) lineage id must end up reachable under the new
// (record-derived) one, not silently orphaned.
func TestFullRebuildCorrectsPiIdentifierAndMigratesAnnotations(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	piRoot := filepath.Join(home, "pi-home")
	transcriptPath := filepath.Join(piRoot, "agent", "sessions", "--x--", "2026-01-01T00-00-00-000Z_019f9009-af52-781d-a202-5c5927ec2c4c.jsonl")
	writeFile(t, transcriptPath,
		`{"type":"session","id":"019f9009-af52-781d-a202-5c5927ec2c4c","cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	p := profile.Profile{Name: "default", Roots: map[string]string{"pi": piRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}

	// Seed the database exactly as the pre-fix code would have left it: a
	// session row keyed by the file-name stem (never actually run through
	// this fix's Refresh - hand-inserted to stand in for "already indexed
	// before this change shipped"), its lineage, and a comment + tag on
	// that lineage.
	wrongStem := "2026-01-01T00-00-00-000Z_019f9009-af52-781d-a202-5c5927ec2c4c"
	oldID := "pi:default:" + wrongStem
	oldLineage := session.LineageID("pi", "default", wrongStem)
	seed := `
INSERT INTO lineages (id, profile, orphaned) VALUES ('` + oldLineage + `', 'default', 0);
INSERT INTO sessions (id, source, source_session_id, lineage_id, end_state, transcript_path, resumable)
	VALUES ('` + oldID + `', 'pi', '` + wrongStem + `', '` + oldLineage + `', 'completed', '` + transcriptPath + `', 1);
INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('` + oldLineage + `', 'keep me', 1, 1);
INSERT INTO tags (lineage_id, tag, created_at) VALUES ('` + oldLineage + `', 'important', 1);
`
	if err := r.DB.Exec(seed); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID              string `json:"id"`
		SourceSessionID string `json:"source_session_id"`
		LineageID       string `json:"lineage_id"`
	}
	if err := r.DB.Query(`SELECT id, source_session_id, lineage_id FROM sessions;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions, want 1", len(rows))
	}
	newLineage := rows[0].LineageID
	if rows[0].SourceSessionID != "019f9009-af52-781d-a202-5c5927ec2c4c" {
		t.Fatalf("source_session_id after rebuild = %q, want the corrected id", rows[0].SourceSessionID)
	}
	if newLineage == oldLineage {
		t.Fatalf("expected the rebuild to change the lineage id (its root is now the corrected identifier), got the same %q", newLineage)
	}

	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.DB.Query(`SELECT body FROM comments WHERE lineage_id = '`+newLineage+`';`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "keep me" {
		t.Fatalf("expected the comment to be reachable under the new lineage id after migration, got %+v", comments)
	}

	var tags []struct {
		Tag string `json:"tag"`
	}
	if err := r.DB.Query(`SELECT tag FROM tags WHERE lineage_id = '`+newLineage+`';`, &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Tag != "important" {
		t.Fatalf("expected the tag to be reachable under the new lineage id after migration, got %+v", tags)
	}

	// The old lineage id must not still be carrying the annotations - they
	// were moved, not copied - though the old lineage row itself is allowed
	// to remain (orphaned, never deleted, same as any other lineage whose
	// session disappeared - spec session-annotations).
	var oldComments []struct {
		Body string `json:"body"`
	}
	if err := r.DB.Query(`SELECT body FROM comments WHERE lineage_id = '`+oldLineage+`';`, &oldComments); err != nil {
		t.Fatal(err)
	}
	if len(oldComments) != 0 {
		t.Errorf("expected no comments left under the old lineage id, got %+v", oldComments)
	}
}

// TestContinuationChainSharesLineage covers spec session-annotations,
// "Session continues under a new identifier": hermes's parent_session_id
// is the one real continuation marker among the four sources (see fyi.md
// group 9 notes - grepped claude/omp transcripts for any resume/fork field
// and found none). Two hermes sessions linked by parent_session_id must
// resolve to the same lineage, and a comment made via the first session id
// must be reachable by resolving the second.
func TestContinuationChainSharesLineage(t *testing.T) {
	bin := sqlite3Path(t)
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	hermesRoot := filepath.Join(home, "hermes-home")
	dbPath := filepath.Join(hermesRoot, "state.db")
	if err := os.MkdirAll(hermesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, title TEXT, parent_session_id TEXT, started_at REAL, ended_at REAL, end_reason TEXT, message_count INTEGER, cwd TEXT, git_branch TEXT, git_repo_root TEXT, last_activity_at REAL);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT, content TEXT, finish_reason TEXT, tool_calls TEXT, timestamp REAL);
INSERT INTO sessions (id, source, title, parent_session_id, started_at, cwd) VALUES ('h1', 'cli', 'first', NULL, 1700000000, '/x');
INSERT INTO sessions (id, source, title, parent_session_id, started_at, cwd) VALUES ('h2', 'cli', 'continued', 'h1', 1700001000, '/x');
INSERT INTO messages (session_id, role, content, finish_reason, timestamp) VALUES ('h1', 'assistant', 'reply', 'stop', 1700000001);
INSERT INTO messages (session_id, role, content, finish_reason, timestamp) VALUES ('h2', 'assistant', 'reply2', 'stop', 1700001001);
`
	cmd := exec.Command(bin, dbPath, seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding: %v: %s", err, out)
	}

	p := profile.Profile{Name: "hermes-only", Roots: map[string]string{"hermes": hermesRoot}}
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID        string `json:"id"`
		LineageID string `json:"lineage_id"`
	}
	if err := r.DB.Query(`SELECT id, lineage_id FROM sessions ORDER BY id;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d sessions", len(rows))
	}
	if rows[0].LineageID != rows[1].LineageID {
		t.Fatalf("continuation chain must share one lineage: %+v", rows)
	}

	// A comment attached via the first session id must be reachable
	// through the second - "the relationship between the two is visible
	// to the user."
	if err := r.DB.Exec(`INSERT INTO comments (lineage_id, body, created_at, updated_at) VALUES ('` + rows[0].LineageID + `', 'from the first session', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	var comments []struct {
		Body string `json:"body"`
	}
	if err := r.DB.Query(`SELECT c.body FROM comments c JOIN sessions s ON s.lineage_id = c.lineage_id WHERE s.id = 'hermes:hermes-only:h2';`, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "from the first session" {
		t.Fatalf("expected the annotation to be reachable from the continued session, got %+v", comments)
	}
}

// TestHandleAllocatedOnFirstSightAndStableAcrossRebuild covers tasks
// 1.2/1.4 (spec session-search, "Handle survives an index rebuild"): a
// lineage's handle is assigned the first time it's seen, and a full rebuild
// - which discards and re-derives the disposable sessions table but never
// touches lineages directly - must not reassign it.
func TestHandleAllocatedOnFirstSightAndStableAcrossRebuild(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var before []struct {
		Handle int `json:"handle"`
	}
	if err := r.DB.Query(`SELECT handle FROM lineages;`, &before); err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Handle == 0 {
		t.Fatalf("expected exactly one lineage with a nonzero handle assigned, got %+v", before)
	}
	handle := before[0].Handle

	// A second, no-op refresh must not change it.
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	// A full rebuild re-derives the same lineage id (deterministic hash)
	// and must reuse the same handle rather than allocating a new one.
	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}

	var after []struct {
		Handle int `json:"handle"`
	}
	if err := r.DB.Query(`SELECT handle FROM lineages;`, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Handle != handle {
		t.Fatalf("expected the handle to survive refreshes and a full rebuild unchanged (%d), got %+v", handle, after)
	}
}

// TestHandlesAreSequentialAndNeverReusedWithinAProfile covers tasks
// 1.2/2.2/spec session-search "Handle of a session that is gone": each new
// lineage in a profile gets the next handle, and a handle that belonged to
// a lineage which then disappears (orphaned, not deleted) is never handed
// to a different lineage afterward.
func TestHandlesAreSequentialAndNeverReusedWithinAProfile(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-a", "a.jsonl"),
		`{"type":"user","message":{"role":"user","content":"first session prompt"},"cwd":"/work/a","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-b", "b.jsonl"),
		`{"type":"user","message":{"role":"user","content":"second session prompt"},"cwd":"/work/b","timestamp":"2026-01-02T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"},"timestamp":"2026-01-02T00:00:01Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		Handle int `json:"handle"`
	}
	if err := r.DB.Query(`SELECT handle FROM lineages ORDER BY handle;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d lineages, want 2", len(rows))
	}
	if rows[0].Handle == 0 || rows[1].Handle == 0 || rows[0].Handle == rows[1].Handle {
		t.Fatalf("expected two distinct nonzero handles, got %+v", rows)
	}

	// One session's source data disappears entirely: its lineage becomes
	// orphaned but its handle must never be reassigned to whatever new
	// lineage appears next.
	if err := os.RemoveAll(filepath.Join(claudeRoot, "projects", "-work-a")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-c", "c.jsonl"),
		`{"type":"user","message":{"role":"user","content":"third session prompt"},"cwd":"/work/c","timestamp":"2026-01-03T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"},"timestamp":"2026-01-03T00:00:01Z"}`+"\n")
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var afterRows []struct {
		Handle   int `json:"handle"`
		Orphaned int `json:"orphaned"`
	}
	if err := r.DB.Query(`SELECT handle, orphaned FROM lineages ORDER BY handle;`, &afterRows); err != nil {
		t.Fatal(err)
	}
	if len(afterRows) != 3 {
		t.Fatalf("got %d lineages, want 3 (two originals retained-as-orphaned/live, plus the new one)", len(afterRows))
	}
	seen := map[int]bool{}
	for _, row := range afterRows {
		if seen[row.Handle] {
			t.Fatalf("handle %d reused across two lineages: %+v", row.Handle, afterRows)
		}
		seen[row.Handle] = true
	}
	if !seen[rows[0].Handle] || !seen[rows[1].Handle] {
		t.Fatalf("expected the two original handles to still be present, got %+v (originals %+v)", afterRows, rows)
	}
}

// TestFullRebuildDropsPreviouslyIngestedLocalCommandOutput covers change
// add-readable-session-listing, task 3.4 (design.md "Correction of
// already-indexed data": "a full rebuild re-extracts prompts and drops what
// should never have been ingested. No migration is written for this; the
// index is derived data and rebuilding is the supported path."). This
// simulates data ingested by a pre-fix build (a local-command-output
// artifact sitting in prompt_fts, as it would have before task 3.1's
// exclusion existed) and asserts a full rebuild against the fixed vocab
// removes it, since FullRebuild always clears prompt_fts before
// re-extracting.
func TestFullRebuildDropsPreviouslyIngestedLocalCommandOutput(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	// Seed a bogus prompt_fts row as if an old, pre-fix build had ingested
	// a local-command-stdout artifact as a "prompt".
	if err := r.DB.Exec(`INSERT INTO prompt_fts (session_id, kind, text) VALUES ('claude:claude-personal:c1', 'prompt', 'local-command-stdout-leftover-text');`); err != nil {
		t.Fatal(err)
	}
	var before []struct {
		SessionID string `json:"session_id"`
	}
	if err := r.DB.Query(`SELECT session_id FROM prompt_fts WHERE prompt_fts MATCH 'leftover';`, &before); err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("expected the seeded bogus row to be present before rebuild, got %d", len(before))
	}

	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}

	var after []struct {
		SessionID string `json:"session_id"`
	}
	if err := r.DB.Query(`SELECT session_id FROM prompt_fts WHERE prompt_fts MATCH 'leftover';`, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("expected a full rebuild to drop previously-ingested non-user text, still found %+v", after)
	}
}

func TestProfileIsolationSeparateDatabaseFiles(t *testing.T) {
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	p1 := profile.Profile{Name: "claude-personal"}
	p2 := profile.Profile{Name: "claude"}
	path1 := profile.DBPath(p1)
	path2 := profile.DBPath(p2)
	if path1 == path2 {
		t.Fatal("two profiles must never resolve to the same database file")
	}

	bin := sqlite3Path(t)
	r1, err := New(p1, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path2); err == nil {
		t.Fatal("refreshing one profile must not create the other profile's database file")
	}
}

// TestSessionNameIndexedAndSurvivesIncrementalRefresh covers change
// show-session-names end to end: a rename recorded in a Claude transcript
// lands in sessions.name, a later rename supersedes it, and a subsequent
// incremental pass whose delta contains no rename record keeps the name
// already known rather than dropping it back to unknown.
func TestSessionNameIndexedAndSurvivesIncrementalRefresh(t *testing.T) {
	p, bin := buildTestProfile(t)
	transcript := filepath.Join(p.Roots["claude"], "projects", "-work-repo", "c1.jsonl")
	appendLine := func(line string) {
		t.Helper()
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	nameNow := func(r *Refresher) *string {
		t.Helper()
		var rows []struct {
			Name *string `json:"name"`
		}
		if err := r.DB.Query(`SELECT name FROM sessions WHERE source = 'claude';`, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d claude session rows, want 1", len(rows))
		}
		return rows[0].Name
	}

	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := nameNow(r); got != nil {
		t.Errorf("name = %q, want NULL for a session that was never renamed", *got)
	}

	appendLine(`{"type":"custom-title","customTitle":"retry-loop","sessionId":"c1"}`)
	appendLine(`{"type":"custom-title","customTitle":"retry-loop-v2","sessionId":"c1"}`)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := nameNow(r); got == nil || *got != "retry-loop-v2" {
		t.Fatalf("name = %v, want the latest rename", got)
	}

	// A delta with no rename in it must not clear the name.
	appendLine(`{"type":"user","message":{"role":"user","content":"and now the timeout"},"cwd":"/work/repo","timestamp":"2026-01-01T00:01:00Z"}`)
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := nameNow(r); got == nil || *got != "retry-loop-v2" {
		t.Fatalf("name = %v after an unrelated delta, want the name already known", got)
	}
}

// TestOriginPersistedFromTranscript covers the origin persistence path end
// to end: a scanned session's origin, classified from the claude
// entrypoint field, reaches the sessions row and comes back out through
// search.List - while a session whose records never named an origin is
// stored as "unknown", never as an empty string or NULL.
func TestOriginPersistedFromTranscript(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"automated run","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n"+
			`{"display":"manual run","timestamp":1700000000001,"project":"/work/repo","sessionId":"c2"}`+"\n")
	// c1 is driven by an SDK: every record carries entrypoint "sdk-cli".
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"run the batch job"},"cwd":"/work/repo","gitBranch":"main","entrypoint":"sdk-cli","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"},"entrypoint":"sdk-cli","timestamp":"2026-01-01T00:00:05Z"}`+"\n")
	// c2 records no entrypoint at all, like the other sources do.
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-repo", "c2.jsonl"),
		`{"type":"user","message":{"role":"user","content":"hi there"},"cwd":"/work/repo","gitBranch":"main","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID     string `json:"id"`
		Origin string `json:"origin"`
	}
	if err := r.DB.Query(`SELECT id, origin FROM sessions ORDER BY id;`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d session rows, want 2", len(rows))
	}
	byID := map[string]string{}
	for _, row := range rows {
		byID[row.ID] = row.Origin
	}
	if got := byID["claude:claude-personal:c1"]; got != string(session.OriginAutomated) {
		t.Errorf("origin for the sdk-cli session = %q, want %q", got, session.OriginAutomated)
	}
	if got := byID["claude:claude-personal:c2"]; got != string(session.OriginUnknown) {
		t.Errorf("origin for the no-entrypoint session = %q, want %q (never an empty string)", got, session.OriginUnknown)
	}

	items, err := search.List(r.DB, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("search.List returned %d items, want 2", len(items))
	}
	itemByID := map[string]session.Origin{}
	for _, it := range items {
		itemByID[it.SessionID] = it.Origin
	}
	if itemByID["claude:claude-personal:c1"] != session.OriginAutomated {
		t.Errorf("Item.Origin for the sdk-cli session = %q, want %q", itemByID["claude:claude-personal:c1"], session.OriginAutomated)
	}
	if itemByID["claude:claude-personal:c2"] != session.OriginUnknown {
		t.Errorf("Item.Origin for the no-entrypoint session = %q, want %q", itemByID["claude:claude-personal:c2"], session.OriginUnknown)
	}
}

// TestEditorClientSessionIsVisibleAndStaysVisible covers the whole path
// for a session driven from an editor (change show-editor-clients): the
// client reaches the sessions row, the session counts as interactive
// despite its SDK entrypoint because its prompts are marked human, and -
// the part an incremental refresh can get wrong - it is still interactive
// after a delta made only of the agent's own records, which carry the SDK
// entrypoint and nothing to say a person is there.
func TestEditorClientSessionIsVisibleAndStaysVisible(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"plan the next step","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n")
	transcript := filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl")
	writeFile(t, transcript,
		`{"type":"user","message":{"role":"user","content":"plan the next step"},"cwd":"/work/repo","gitBranch":"main","entrypoint":"sdk-ts","origin":{"kind":"human"},"timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Here it is."}],"stop_reason":"end_turn"},"entrypoint":"sdk-ts","timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	state := func(when string) (client *string, origin string) {
		t.Helper()
		var rows []struct {
			Client *string `json:"client"`
			Origin string  `json:"origin"`
		}
		if err := r.DB.Query(`SELECT client, origin FROM sessions WHERE source = 'claude';`, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: got %d claude session rows, want 1", when, len(rows))
		}
		return rows[0].Client, rows[0].Origin
	}

	client, origin := state("first pass")
	if client == nil || *client != "sdk-ts" {
		t.Errorf("client = %v, want the entrypoint the source recorded", client)
	}
	if origin != string(session.OriginInteractive) {
		t.Errorf("origin = %q, want %q: a person was typing, whatever the entrypoint says", origin, session.OriginInteractive)
	}

	// The default hide rules must not swallow it: this is the whole point.
	items, err := search.List(r.DB, search.Filter{Hide: config.Hide{NonInteractive: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("an editor chat is listed under the standing hide rules: got %d items, want 1", len(items))
	}
	if items[0].Client == nil || *items[0].Client != "sdk-ts" {
		t.Errorf("Item.Client = %v, want the stored client", items[0].Client)
	}

	// A delta of the agent's own records only - no human prompt in range.
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"And a follow-up."}],"stop_reason":"end_turn"},"entrypoint":"sdk-ts","timestamp":"2026-01-01T00:00:09Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	client, origin = state("after an incremental delta")
	if client == nil || *client != "sdk-ts" {
		t.Errorf("client = %v after a delta, want the client already known", client)
	}
	if origin != string(session.OriginInteractive) {
		t.Errorf("origin = %q after a delta of agent records, want %q: the chat did not become automation", origin, session.OriginInteractive)
	}
}

// TestClientFilterMatchesLabelAndRawValue: --client accepts what the row
// shows ("acp") and what the source recorded ("sdk-ts") alike, so a user
// can filter by the name they just read on screen.
func TestClientFilterMatchesLabelAndRawValue(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"from the editor","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n"+
			`{"display":"from the terminal","timestamp":1700000000001,"project":"/work/repo","sessionId":"c2"}`+"\n")
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"from the editor"},"cwd":"/work/repo","entrypoint":"sdk-ts","origin":{"kind":"human"},"timestamp":"2026-01-01T00:00:00Z"}`+"\n")
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-repo", "c2.jsonl"),
		`{"type":"user","message":{"role":"user","content":"from the terminal"},"cwd":"/work/repo","entrypoint":"cli","timestamp":"2026-01-01T00:00:00Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"acp", "sdk-ts"} {
		items, err := search.List(r.DB, search.Filter{Client: name})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].SessionID != "claude:claude-personal:c1" {
			t.Errorf("--client=%s matched %d items, want only the editor session", name, len(items))
		}
	}

	items, err := search.List(r.DB, search.Filter{Client: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SessionID != "claude:claude-personal:c2" {
		t.Errorf("--client=cli matched %d items, want only the terminal session", len(items))
	}
}

// TestOriginAgreesAcrossIncrementalAndFullRebuild reproduces the audit
// finding on commit 9feca0a (defect 1): a session that starts at a plain
// terminal (entrypoint "cli", no origin.kind "human" anywhere - a script
// never sends one) and is later driven by automation (entrypoint
// "sdk-cli") must classify as automated on BOTH an incremental refresh of
// just the new records and a --full rebuild that rescans everything, not
// one or the other depending on which was run. Before the fix, the
// incremental path's "stay interactive if the prior row already was"
// stickiness rule kept this session interactive forever once claudeOrigin's
// entrypoint-"cli" default had set it once, while --full (which folds
// every record fresh and finds no human marker) correctly called it
// automated - the exact disagreement-by-refresh-path the audit flagged.
func TestOriginAgreesAcrossIncrementalAndFullRebuild(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"start it from the terminal","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n")
	transcript := filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl")
	// Neither record here carries origin.kind "human" - "cli" alone is
	// enough for claudeOrigin's allowlist-of-automated fallback to call
	// this interactive, same as any transcript this build has never seen
	// a reason to distrust.
	writeFile(t, transcript,
		`{"type":"user","message":{"role":"user","content":"start it from the terminal"},"cwd":"/work/repo","gitBranch":"main","entrypoint":"cli","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"On it."}],"stop_reason":"end_turn"},"entrypoint":"cli","timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}

	origin := func(when string) string {
		t.Helper()
		var rows []struct {
			Origin string `json:"origin"`
		}
		if err := r.DB.Query(`SELECT origin FROM sessions WHERE source = 'claude';`, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: got %d claude session rows, want 1", when, len(rows))
		}
		return rows[0].Origin
	}

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := origin("cli-only pass"); got != string(session.OriginInteractive) {
		t.Fatalf("origin after the cli-only pass = %q, want %q", got, session.OriginInteractive)
	}

	// Automation appends to the same transcript. No record anywhere in it,
	// before or after, ever carries origin.kind "human".
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Automated follow-up."}],"stop_reason":"end_turn"},"entrypoint":"sdk-cli","timestamp":"2026-01-01T00:01:00Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	incrementalOrigin := origin("after the incremental delta")
	if incrementalOrigin != string(session.OriginAutomated) {
		t.Errorf("origin after an incremental refresh of the automated delta = %q, want %q: no record in this transcript ever marks a human prompt", incrementalOrigin, session.OriginAutomated)
	}

	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}
	fullOrigin := origin("after refresh --full")

	if incrementalOrigin != fullOrigin {
		t.Fatalf("origin diverges by refresh path: incremental = %q, refresh --full = %q - the same session must classify the same way regardless of how it was refreshed", incrementalOrigin, fullOrigin)
	}
}

// TestOriginAgreesAcrossIncrementalAndFullRebuildWhenAutomationComesFirst
// covers the ordering the earlier regression did not: the first pass already
// finds automation, and a later plain-cli delta carries an interactive
// fallback. The first record is padded beyond the rewrite detector's window,
// so appending the second cannot accidentally turn this into a from-zero scan:
// the assertion is specifically about merging two separate folds.
func TestOriginAgreesAcrossIncrementalAndFullRebuildWhenAutomationComesFirst(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"run it unattended","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n")
	transcript := filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl")
	writeFile(t, transcript,
		`{"type":"user","message":{"role":"user","content":"run it unattended"},"cwd":"/work/repo","entrypoint":"sdk-cli","pad":"`+strings.Repeat("x", 300)+`","timestamp":"2026-01-01T00:00:00Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}

	state := func(when string) (origin string, messageCount int64) {
		t.Helper()
		var rows []struct {
			Origin       string `json:"origin"`
			MessageCount int64  `json:"message_count"`
		}
		if err := r.DB.Query(`SELECT origin, message_count FROM sessions WHERE source = 'claude';`, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: got %d claude session rows, want 1", when, len(rows))
		}
		return rows[0].Origin, rows[0].MessageCount
	}

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if origin, _ := state("automated-only pass"); origin != string(session.OriginAutomated) {
		t.Fatalf("origin after the sdk-cli-only pass = %q, want %q", origin, session.OriginAutomated)
	}

	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The script completed."}],"stop_reason":"end_turn"},"entrypoint":"cli","timestamp":"2026-01-01T00:01:00Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	incrementalOrigin, messageCount := state("after the plain-cli delta")
	if messageCount != 2 {
		t.Fatalf("message_count after the separate incremental delta = %d, want 2 - a different count means this test tripped a rewrite re-scan instead", messageCount)
	}
	if incrementalOrigin != string(session.OriginAutomated) {
		t.Errorf("origin after an incremental refresh of the plain-cli delta = %q, want %q: one automated record in the earlier pass decides the whole session", incrementalOrigin, session.OriginAutomated)
	}

	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}
	fullOrigin, _ := state("after refresh --full")
	if incrementalOrigin != fullOrigin {
		t.Fatalf("origin diverges by refresh path: incremental = %q, refresh --full = %q - the same session must classify the same way regardless of record ordering", incrementalOrigin, fullOrigin)
	}
}

// TestRewriteDiscardsPriorTranscriptState covers the case no ordinary
// incremental merge can represent: once a file is known to have been
// replaced, every fact read from its old bytes is stale. The replacement
// deliberately removes the human prompt and editor client, and has one
// recognizable record, so origin, client, and message_count each prove that
// the refresh used replacement evidence alone.
func TestRewriteDiscardsPriorTranscriptState(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"started in an editor","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n")
	transcript := filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl")
	writeFile(t, transcript,
		`{"type":"user","message":{"role":"user","content":"started in an editor"},"cwd":"/work/repo","entrypoint":"sdk-ts","origin":{"kind":"human"},"pad":"`+strings.Repeat("x", 300)+`","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Acknowledged."}],"stop_reason":"end_turn"},"entrypoint":"sdk-ts","timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var initial []struct {
		Origin string  `json:"origin"`
		Client *string `json:"client"`
	}
	if err := r.DB.Query(`SELECT origin, client FROM sessions WHERE source = 'claude';`, &initial); err != nil {
		t.Fatal(err)
	}
	if len(initial) != 1 || initial[0].Origin != string(session.OriginInteractive) || initial[0].Client == nil || *initial[0].Client != "sdk-ts" {
		t.Fatalf("initial transcript state = %+v, want interactive sdk-ts", initial)
	}

	writeFile(t, transcript,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Replacement automation."}],"stop_reason":"end_turn"},"entrypoint":"sdk-cli","timestamp":"2026-01-01T00:01:00Z"}`+"\n")
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		Origin       string  `json:"origin"`
		Client       *string `json:"client"`
		MessageCount int64   `json:"message_count"`
	}
	if err := r.DB.Query(`SELECT origin, client, message_count FROM sessions WHERE source = 'claude';`, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d claude session rows after replacement, want 1", len(rows))
	}
	if rows[0].Origin != string(session.OriginAutomated) {
		t.Errorf("origin after replacement = %q, want %q: the old human prompt no longer exists", rows[0].Origin, session.OriginAutomated)
	}
	if rows[0].Client == nil || *rows[0].Client != "sdk-cli" {
		// Deref for the message: a *string prints as an address, which
		// tells the reader nothing about which client was wrongly kept.
		got := "<nil>"
		if rows[0].Client != nil {
			got = *rows[0].Client
		}
		t.Errorf("client after replacement = %q, want sdk-cli: the old editor client no longer exists", got)
	}
	if rows[0].MessageCount != 1 {
		t.Errorf("message_count after replacement = %d, want 1: the old transcript's records must not be added to the replacement's scan", rows[0].MessageCount)
	}
}

// TestClientAgreesAcrossIncrementalAndFullRebuild reproduces the audit
// finding on commit 9feca0a (defect 2): a chat created in an editor
// (entrypoint "sdk-ts") and later resumed in the source's own terminal
// (entrypoint "cli") must keep showing the client it started with - on
// both an incremental refresh of just the later delta and a --full
// rebuild - never the client of whichever records the most recent refresh
// happened to read. Before the fix, an incremental pass let the latest
// delta's first client overwrite the one already stored, while --full (one
// scan, always from byte 0) kept the true first client - so the stored
// client, and --client filtering, oscillated with refresh history alone.
func TestClientAgreesAcrossIncrementalAndFullRebuild(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"started in the editor","timestamp":1700000000000,"project":"/work/repo","sessionId":"c1"}`+"\n")
	transcript := filepath.Join(claudeRoot, "projects", "-work-repo", "c1.jsonl")
	writeFile(t, transcript,
		`{"type":"user","message":{"role":"user","content":"started in the editor"},"cwd":"/work/repo","gitBranch":"main","entrypoint":"sdk-ts","origin":{"kind":"human"},"timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Here you go."}],"stop_reason":"end_turn"},"entrypoint":"sdk-ts","timestamp":"2026-01-01T00:00:05Z"}`+"\n")

	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": claudeRoot}}
	r, err := New(p, sqlite3Path(t))
	if err != nil {
		t.Fatal(err)
	}

	client := func(when string) *string {
		t.Helper()
		var rows []struct {
			Client *string `json:"client"`
		}
		if err := r.DB.Query(`SELECT client FROM sessions WHERE source = 'claude';`, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: got %d claude session rows, want 1", when, len(rows))
		}
		return rows[0].Client
	}

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := client("editor-only pass"); got == nil || *got != "sdk-ts" {
		t.Fatalf("client after the editor-only pass = %v, want sdk-ts", got)
	}

	// The same conversation resumes in the source's own terminal - a
	// different frontend, appending records with a different entrypoint.
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"user","message":{"role":"user","content":"resuming from the terminal now"},"cwd":"/work/repo","entrypoint":"cli","timestamp":"2026-01-01T00:01:00Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	deref := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	incrementalClient := client("after the incremental delta")
	if incrementalClient == nil || *incrementalClient != "sdk-ts" {
		t.Errorf("client after an incremental refresh of the resumed-in-terminal delta = %s, want sdk-ts (the client the session started with)", deref(incrementalClient))
	}

	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatal(err)
	}
	fullClient := client("after refresh --full")
	if fullClient == nil || incrementalClient == nil || *fullClient != *incrementalClient {
		t.Fatalf("client diverges by refresh path: incremental = %s, refresh --full = %s - the same session must show the same client regardless of how it was refreshed", deref(incrementalClient), deref(fullClient))
	}
}
