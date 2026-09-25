package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/search"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// All transcript content below is synthetic, hand-written test data -
// never real session content.

const fixtureTranscript = `{"type":"user","message":{"role":"user","content":"why is the migration failing"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The migration is missing a down step."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]},"timestamp":"2026-01-01T00:00:03Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Added the missing migration step."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:04Z"}
`

func writeFixtureTranscript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(fixtureTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// noProfile stands in for the install resolver the database-backed sources
// need. These tests all use claude, which reads a file and never asks.
func noProfile(search.Item) (profile.Profile, error) {
	return profile.Profile{}, errors.New("no profile in this test")
}

func transcriptItem(path string) search.Item {
	return search.Item{SessionID: "claude:p:s0", Source: "claude", TranscriptPath: &path}
}

func TestTranscriptTabRendersTheConversation(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, transcriptItem(writeFixtureTranscript(t)), "", noProfile, RenderOptions{Width: 60})

	for _, want := range []string{
		"you", "why is the migration failing",
		"agent", "The migration is missing a down step.",
		"tool Edit",
		"Added the missing migration step.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript is missing %q:\n%s", want, out)
		}
	}
	// The tool *result* is not a turn - it carries no text, so a line for
	// it would say only that something came back.
	if strings.Count(out, "tool") != 1 {
		t.Errorf("expected exactly one tool line:\n%s", out)
	}
}

// A database-backed source is read from its own database rather than from a
// transcript file, so the tab shows the conversation for it too. This is the
// browser end of adapter.ConversationReader; the per-source queries are
// tested in each adapter's own package.
func TestTranscriptTabReadsADatabaseBackedSource(t *testing.T) {
	root := t.TempDir()
	r := &sqlitex.Runner{DBPath: filepath.Join(root, "state.db")}
	if err := r.Exec(`
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
	content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL NOT NULL
);
INSERT INTO messages (session_id, role, content, timestamp) VALUES
  ('s_a', 'user', 'read from the database', 1000),
  ('s_a', 'assistant', 'and answered from it', 1001);`); err != nil {
		t.Fatal(err)
	}
	prof := func(search.Item) (profile.Profile, error) {
		return profile.Profile{Name: "p", Roots: map[string]string{"hermes": root}}, nil
	}

	c := &conversationCache{}
	it := search.Item{SessionID: "hermes:p:s_a", Source: "hermes", SourceSessionID: "s_a"}
	out := renderItemTranscript(c, it, "", prof, RenderOptions{Width: 60})

	if !strings.Contains(out, "read from the database") || !strings.Contains(out, "and answered from it") {
		t.Errorf("expected the database-backed conversation, got:\n%s", out)
	}
	if c.source != sourceKeepsDatabase {
		t.Errorf("cache source = %v, want sourceKeepsDatabase", c.source)
	}
}

// TestTranscriptTabUsesTheSessionsOwnInstall covers the fix at the heart of
// change group-sessions-in-one-index's Transcript tab: a hermes session
// belonging to install Y must be read from install Y's own state.db, not
// from any single "active profile" - a notion that no longer exists once
// one browsing session lists every install's sessions together. Both
// installs are given a session sharing the same source_session_id ("h1"),
// so a wrong lookup would silently read the wrong install's content instead
// of erroring in a way this test could catch some other way.
func TestTranscriptTabUsesTheSessionsOwnInstall(t *testing.T) {
	seedHermesDB := func(root, marker string) {
		r := &sqlitex.Runner{DBPath: filepath.Join(root, "state.db")}
		ddl := `
CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, created_at REAL, updated_at REAL);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
	content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
	timestamp REAL NOT NULL, finish_reason TEXT
);
INSERT INTO sessions (id, title) VALUES ('h1', 't');
`
		if err := r.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		if err := r.Exec(fmt.Sprintf(
			"INSERT INTO messages (session_id, role, content, timestamp) VALUES ('h1', 'user', %q, 1000);",
			marker,
		)); err != nil {
			t.Fatal(err)
		}
	}

	rootX := t.TempDir()
	rootY := t.TempDir()
	seedHermesDB(rootX, "installX's own content")
	seedHermesDB(rootY, "installY's own content")

	db := browseTestDB(t)
	seedBrowseSession(t, db, "lx", "hermes:installX:h1", "installX", 1, map[string]any{
		"source": "hermes", "source_session_id": "h1", "topic": "from installX",
	})

	installs := func() []profile.Profile {
		return []profile.Profile{
			{Name: "installX", Roots: map[string]string{"hermes": rootX}},
			{Name: "installY", Roots: map[string]string{"hermes": rootY}},
		}
	}
	m := newTestBrowser(db, "installX", BrowserOptions{Installs: installs})
	m.tab = tabTranscript
	got := m.detailContent(RenderOptions{Width: 60})
	if !strings.Contains(got, "installX's own content") {
		t.Errorf("transcript did not read from the session's own install:\n%s", got)
	}
	if strings.Contains(got, "installY's own content") {
		t.Errorf("transcript leaked installY's content:\n%s", got)
	}
}

// antigravity is the one source whose conversations cannot be read at all -
// protobuf with no available schema. That is a fact about the agent, not a
// failure, and the tab says so in those words while pointing at the tab that
// does have something.
func TestTranscriptTabExplainsAnUndecodableSource(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, search.Item{SessionID: "antigravity:p:s0", Source: "antigravity"}, "", noProfile, RenderOptions{Width: 60})

	if !strings.Contains(out, "cannot decode") {
		t.Errorf("expected an explanation that the format cannot be decoded, got:\n%s", out)
	}
	if !strings.Contains(out, "Prompts") {
		t.Errorf("expected a pointer to the Prompts tab, got:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "error") {
		t.Errorf("an undecodable source is not an error:\n%s", out)
	}
}

// A source that does keep transcripts, but whose session has no path in the
// index, is a different situation from a source that keeps none - and it is
// one a refresh can fix, so the message says so rather than blaming the
// agent's storage shape.
func TestTranscriptTabDistinguishesAMissingPathFromADatabaseSource(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, search.Item{SessionID: "claude:p:s0", Source: "claude"}, "", noProfile, RenderOptions{Width: 60})

	if strings.Contains(out, "database") {
		t.Errorf("claude keeps transcripts; this is a missing path, not a database source:\n%s", out)
	}
	if !strings.Contains(out, "no transcript file recorded") {
		t.Errorf("expected the missing-path explanation, got:\n%s", out)
	}
}

// A transcript the index knows a path for but that has since been cleaned
// up by the agent is the case a tagged, commented session most needs
// explained - "gone" rather than a bare open error.
func TestTranscriptTabExplainsAMissingFile(t *testing.T) {
	c := &conversationCache{}
	gone := filepath.Join(t.TempDir(), "cleaned-up.jsonl")
	out := renderItemTranscript(c, transcriptItem(gone), "", noProfile, RenderOptions{Width: 60})

	if !strings.Contains(out, "cleaned it up") {
		t.Errorf("expected the cleanup explanation, got:\n%s", out)
	}
}

func TestTranscriptTabRecordsMatchLinesForTheSearchPhrase(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, transcriptItem(writeFixtureTranscript(t)), "migration", noProfile, RenderOptions{Width: 60})

	if len(c.hits) != 3 {
		t.Fatalf("hits = %v, want one per line containing the phrase:\n%s", c.hits, out)
	}
	// Every recorded offset must name a line that really does contain the
	// phrase - an offset measured against different content would scroll
	// the pane to the wrong place.
	lines := strings.Split(out, "\n")
	for _, h := range c.hits {
		if h >= len(lines) {
			t.Fatalf("hit line %d is past the end of %d rendered lines", h, len(lines))
		}
		if !strings.Contains(strings.ToLower(lines[h]), "migration") {
			t.Errorf("hit line %d does not contain the phrase: %q", h, lines[h])
		}
	}
}

// TestTranscriptTabHighlightsEachWordIndependently is the regression test
// for review fix #5 (change group-sessions-in-one-index): search matches
// each typed word as an independent prefix term
// (sqlitex.FTS5PrefixTerms), so a multi-word query can match a session with
// its words scattered across the transcript rather than adjacent. The old
// highlightPhrase/containsFold looked for the whole typed phrase as one
// contiguous substring, found it nowhere in this fixture (its two words
// never sit next to each other), and so recorded zero hits and highlighted
// nothing - even though "failing" and "step" each appear on their own line.
func TestTranscriptTabHighlightsEachWordIndependently(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, transcriptItem(writeFixtureTranscript(t)), "failing step", noProfile, RenderOptions{Width: 60, Style: true})

	// "failing" is on the first line only; "step" ends both the second and
	// fifth lines (see fixtureTranscript) - three lines total, none of them
	// containing "failing step" as one contiguous run.
	if len(c.hits) != 3 {
		t.Fatalf("hits = %v, want 3 (one line matching \"failing\", two matching \"step\")", c.hits)
	}
	if !strings.Contains(out, ansiReverse+"failing"+ansiReset) {
		t.Errorf("expected %q to be highlighted, got:\n%s", "failing", out)
	}
	if !strings.Contains(out, ansiReverse+"step"+ansiReset) {
		t.Errorf("expected %q to be highlighted, got:\n%s", "step", out)
	}
}

// TestTranscriptTabHighlightingUnchangedForASingleWord covers "A
// single-word query behaves exactly as before": containsAnyFold and
// highlightWords, given a one-element words slice, must find and highlight
// exactly what the old whole-phrase substring match did.
func TestTranscriptTabHighlightingUnchangedForASingleWord(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, transcriptItem(writeFixtureTranscript(t)), "migration", noProfile, RenderOptions{Width: 60, Style: true})

	if len(c.hits) != 3 {
		t.Fatalf("hits = %v, want 3, one per line containing \"migration\"", c.hits)
	}
	if got := strings.Count(out, ansiReverse+"migration"+ansiReset); got != 3 {
		t.Errorf("highlighted %q %d times, want 3", "migration", got)
	}
}

// The pane is rebuilt on every frame, so the read has to be cached or every
// keystroke re-reads the file. Deleting the transcript after the first
// render is how a test can see that the second one did not go to disk.
func TestTranscriptTabCachesTheReadForTheSameSession(t *testing.T) {
	path := writeFixtureTranscript(t)
	it := transcriptItem(path)
	c := &conversationCache{}

	first := renderItemTranscript(c, it, "", noProfile, RenderOptions{Width: 60})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second := renderItemTranscript(c, it, "", noProfile, RenderOptions{Width: 60})

	if first != second {
		t.Errorf("second render re-read the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// ...but a different session must not be served the previous one's
// transcript.
func TestTranscriptTabRereadsForADifferentSession(t *testing.T) {
	c := &conversationCache{}
	renderItemTranscript(c, transcriptItem(writeFixtureTranscript(t)), "", noProfile, RenderOptions{Width: 60})

	other := search.Item{SessionID: "claude:p:s1", Source: "claude"}
	out := renderItemTranscript(c, other, "", noProfile, RenderOptions{Width: 60})
	if strings.Contains(out, "why is the migration failing") {
		t.Errorf("a second session was served the first one's transcript:\n%s", out)
	}
}

// ---------------------------------------------------------------------
// n / N
// ---------------------------------------------------------------------

// writeScrollableTranscript writes a transcript long enough that the detail
// pane cannot show all of it at once. A short one would make n and N look
// broken when they are right: a viewport with nothing below the fold has
// nowhere to scroll to, so every match is already on screen.
func writeScrollableTranscript(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < 40; i++ {
		text := "routine question number " + strconv.Itoa(i)
		if i%10 == 0 {
			text = "why is the migration failing this time"
		}
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":%q},"timestamp":"2026-01-01T00:00:00Z"}`+"\n", text)
	}
	p := filepath.Join(t.TempDir(), "long.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// transcriptBrowser is a browser holding one claude session whose
// transcript is long enough to scroll, opened on the Transcript tab.
func transcriptBrowser(t *testing.T, query string) *browseModel {
	t.Helper()
	db := browseTestDB(t)
	seedBrowseSession(t, db, "L0", "claude:p:s0", "p", 1, map[string]any{
		"source": "claude", "cwd": "/work/repo", "git_common_root": "/work/repo",
		"topic": "the migration", "last_activity_at": 1700000000, "dir_exists": 1,
		"transcript_path": writeScrollableTranscript(t),
	})
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Installs: testInstalls("claude-personal", "claude-work"),
	})
	m.tab = tabTranscript
	m.textFilter = query
	// The hit offsets are recorded by the render that produces the lines
	// the viewport is showing, exactly as they are in the running program,
	// so the view has to be drawn before n means anything.
	m.View()
	return m
}

func TestNextMatchScrollsThroughTheTranscript(t *testing.T) {
	m := transcriptBrowser(t, "migration")
	if len(m.convo.hits) < 2 {
		t.Fatalf("fixture needs at least two matches, got %v", m.convo.hits)
	}

	m = update(t, m, keyRunes("n"))
	first := m.detail.YOffset
	if first != m.convo.hits[0] {
		t.Errorf("n scrolled to %d, want the first hit at %d", first, m.convo.hits[0])
	}

	m.View()
	m = update(t, m, keyRunes("n"))
	if m.detail.YOffset <= first {
		t.Errorf("a second n did not advance: still at %d", m.detail.YOffset)
	}
}

// n alone has to be enough to walk every match, so stepping past the last
// one comes back to the first rather than stopping.
func TestNextMatchWrapsAtTheEnd(t *testing.T) {
	m := transcriptBrowser(t, "migration")
	hits := m.convo.hits
	if len(hits) == 0 {
		t.Fatal("fixture produced no matches")
	}

	m.detail.SetYOffset(hits[len(hits)-1])
	m.View()
	m = update(t, m, keyRunes("n"))
	if m.detail.YOffset != hits[0] {
		t.Errorf("n past the last hit went to %d, want a wrap to %d", m.detail.YOffset, hits[0])
	}

	m.View()
	m = update(t, m, keyRunes("N"))
	if m.detail.YOffset != hits[len(hits)-1] {
		t.Errorf("N before the first hit went to %d, want a wrap to %d", m.detail.YOffset, hits[len(hits)-1])
	}
}

func TestNextMatchSaysWhyItDidNothing(t *testing.T) {
	// No phrase to step through.
	m := transcriptBrowser(t, "")
	m = update(t, m, keyRunes("n"))
	if !strings.Contains(m.notice, "No search phrase") {
		t.Errorf("notice = %q, want an explanation that there is no phrase", m.notice)
	}

	// A phrase that is not in the transcript.
	m = transcriptBrowser(t, "kubernetes")
	m = update(t, m, keyRunes("n"))
	if !strings.Contains(m.notice, "No match") {
		t.Errorf("notice = %q, want an explanation that nothing matched", m.notice)
	}

	// The wrong tab.
	m = transcriptBrowser(t, "migration")
	m.tab = tabDetail
	m.View()
	m = update(t, m, keyRunes("n"))
	if !strings.Contains(m.notice, "Transcript tab") {
		t.Errorf("notice = %q, want it to name the Transcript tab", m.notice)
	}
}

// ---------------------------------------------------------------------
// Clean mode (change clean-transcript-mode)
// ---------------------------------------------------------------------

// cleanModeFixture interleaves a tool call and a compaction boundary
// between two assistant replies, and gives every turn its own minute so a
// test can tell "the merged block kept the first reply's timestamp" apart
// from "it kept the last one's". Both hidden kinds appear once, so a test
// can check each is actually gone rather than one happening to stand in
// for the other.
const cleanModeFixture = `{"type":"user","message":{"role":"user","content":"question one"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first paragraph"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:01:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:02:00Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]},"timestamp":"2026-01-01T00:03:00Z"}
{"type":"system","subtype":"compact_boundary","timestamp":"2026-01-01T00:03:30Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second paragraph mentions migration"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:04:00Z"}
{"type":"user","message":{"role":"user","content":"question two"},"timestamp":"2026-01-01T00:05:00Z"}
`

func writeCleanModeFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clean.jsonl")
	if err := os.WriteFile(p, []byte(cleanModeFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCleanModeHidesToolCallsAndCompactionBoundaries covers the rendering
// rule at the heart of the feature: KindToolUse and KindCompactionBoundary
// turns disappear, and the two assistant replies that are left adjacent
// once they are gone merge into one "agent" block under the first reply's
// own timestamp, with a blank line between the merged paragraphs.
// KindUserPrompt turns are never merged, so "question one" and "question
// two" each stay their own turn.
func TestCleanModeHidesToolCallsAndCompactionBoundaries(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscriptMode(c, transcriptItem(writeCleanModeFixture(t)), "", noProfile, transcriptClean, RenderOptions{Width: 60})

	if strings.Contains(out, "tool") {
		t.Errorf("clean mode still shows the tool call:\n%s", out)
	}
	if strings.Contains(out, "compacted") {
		t.Errorf("clean mode still shows the compaction boundary:\n%s", out)
	}
	if got := strings.Count(out, "agent"); got != 1 {
		t.Errorf("clean mode has %d \"agent\" headers, want 1 (the two replies merged into one block):\n%s", got, out)
	}
	if !strings.Contains(out, "question one") || !strings.Contains(out, "question two") {
		t.Errorf("clean mode dropped a user turn it should never merge or hide:\n%s", out)
	}

	lines := strings.Split(out, "\n")
	first, blank, second := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "first paragraph"):
			first = i
		case strings.Contains(l, "second paragraph mentions migration"):
			second = i
		}
	}
	if first < 0 || second < 0 {
		t.Fatalf("merged paragraphs not both found:\n%s", out)
	}
	blank = first + 1
	if second != blank+1 {
		t.Fatalf("paragraphs are not separated by exactly one blank line (first=%d, second=%d):\n%s", first, second, out)
	}
	if strings.TrimSpace(lines[blank]) != "" {
		t.Errorf("line between the merged paragraphs is not blank: %q", lines[blank])
	}

	// The merged block's header must carry the FIRST reply's timestamp
	// (00:01), not the second's (00:04) - a header this exact would be
	// unreachable if it had merged the wrong way.
	agentHeaderLine := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "agent") {
			agentHeaderLine = i
			break
		}
	}
	if agentHeaderLine < 0 {
		t.Fatalf("no agent header found:\n%s", out)
	}
	if !strings.Contains(lines[agentHeaderLine], "00:01") {
		t.Errorf("agent header %q does not carry the first reply's timestamp", lines[agentHeaderLine])
	}
	if strings.Contains(lines[agentHeaderLine], "00:04") {
		t.Errorf("agent header %q carries the second reply's timestamp instead of the first's", lines[agentHeaderLine])
	}
}

// TestFullModeTranscriptUnaffectedByCleanMode is the regression test for
// "Full mode must render exactly what renders today": the same fixture,
// through the full-mode wrapper, must still show the tool call and the
// compaction boundary and must NOT merge the two replies.
func TestFullModeTranscriptUnaffectedByCleanMode(t *testing.T) {
	c := &conversationCache{}
	out := renderItemTranscript(c, transcriptItem(writeCleanModeFixture(t)), "", noProfile, RenderOptions{Width: 60})

	if !strings.Contains(out, "tool Bash") {
		t.Errorf("full mode dropped the tool call:\n%s", out)
	}
	if !strings.Contains(out, "compacted") {
		t.Errorf("full mode dropped the compaction boundary:\n%s", out)
	}
	if got := strings.Count(out, "agent"); got != 2 {
		t.Errorf("full mode has %d \"agent\" headers, want 2 (unmerged):\n%s", got, out)
	}
}

// TestCleanModeWithNothingLeftToShow covers the edge case where every turn
// Conversation kept was a tool call or a compaction boundary: clean mode
// must say so distinctly from an empty conversation, not render a blank
// pane that looks like a bug.
func TestCleanModeWithNothingLeftToShow(t *testing.T) {
	fixture := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"system","subtype":"compact_boundary","timestamp":"2026-01-01T00:00:01Z"}
`
	p := filepath.Join(t.TempDir(), "tools-only.jsonl")
	if err := os.WriteFile(p, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &conversationCache{}
	out := renderItemTranscriptMode(c, transcriptItem(p), "", noProfile, transcriptClean, RenderOptions{Width: 60})
	if !strings.Contains(out, "t for the full transcript") {
		t.Errorf("expected a pointer to the full transcript, got:\n%s", out)
	}
	if strings.Contains(out, "No readable turns") {
		t.Errorf("this is not an empty conversation, it is an empty clean-mode view:\n%s", out)
	}
}

// ---------------------------------------------------------------------
// The `t` toggle
// ---------------------------------------------------------------------

// TestToggleTranscriptModeChangesRenderingAndNotice covers the `t` key
// itself: it flips the mode back and forth while the Transcript tab is
// showing, names the mode it switched to in the notice, and is inert (with
// an explanatory notice, like n/N's own tab guard) everywhere else.
func TestToggleTranscriptModeChangesRenderingAndNotice(t *testing.T) {
	m := transcriptBrowser(t, "")
	if m.transcriptMode != transcriptClean {
		t.Fatalf("default mode = %v, want clean", m.transcriptMode)
	}

	m = update(t, m, keyRunes("t"))
	if m.transcriptMode != transcriptFull {
		t.Errorf("t did not switch to full mode")
	}
	if !strings.Contains(m.notice, "full") {
		t.Errorf("notice = %q, want it to name full mode", m.notice)
	}

	m = update(t, m, keyRunes("t"))
	if m.transcriptMode != transcriptClean {
		t.Errorf("a second t did not switch back to clean mode")
	}
	if !strings.Contains(m.notice, "clean") {
		t.Errorf("notice = %q, want it to name clean mode", m.notice)
	}

	// Off the Transcript tab, t changes nothing and says why - the same
	// guard jumpToHit applies to n/N.
	m.tab = tabDetail
	before := m.transcriptMode
	m = update(t, m, keyRunes("t"))
	if m.transcriptMode != before {
		t.Errorf("t changed the mode while off the Transcript tab")
	}
	if !strings.Contains(m.notice, "Transcript tab") {
		t.Errorf("notice = %q, want it to name the Transcript tab", m.notice)
	}
}

// TestTranscriptModeHintNamesTheActiveModeAndKey covers the tab chrome: the
// detail pane's top border says which mode is active and which key
// switches it, only while the Transcript tab is showing.
func TestTranscriptModeHintNamesTheActiveModeAndKey(t *testing.T) {
	m := transcriptBrowser(t, "")
	if hint := m.transcriptModeHint(); !strings.Contains(hint, "clean") || !strings.Contains(hint, "t") {
		t.Errorf("hint %q does not name the default mode and the toggle key", hint)
	}

	m = update(t, m, keyRunes("t"))
	if hint := m.transcriptModeHint(); !strings.Contains(hint, "full") {
		t.Errorf("hint %q does not switch to naming full mode", hint)
	}

	m.tab = tabDetail
	if hint := m.transcriptModeHint(); hint != "" {
		t.Errorf("hint = %q, want empty off the Transcript tab", hint)
	}
}

// TestTranscriptModePersistsAcrossSessionSelection covers "the mode
// persists across session selections for the lifetime of the browser":
// moving the Sessions cursor must not reset a mode the user just chose,
// the way it does not reset the search phrase or any other browser-wide
// setting.
func TestTranscriptModePersistsAcrossSessionSelection(t *testing.T) {
	m := transcriptBrowser(t, "")
	m = update(t, m, keyRunes("t"))
	if m.transcriptMode != transcriptFull {
		t.Fatalf("t did not switch to full mode")
	}

	m.focus = panelSessions
	m.move(1)
	if m.transcriptMode != transcriptFull {
		t.Errorf("selecting a different session reset the transcript mode")
	}
}

// writeToggleModeTranscript writes a transcript long enough to scroll
// (writeScrollableTranscript's "nowhere to scroll to" concern applies here
// too), with one search match buried in a run of filler user turns and a
// second match in an assistant reply immediately preceded by a tool call.
// Removing that tool call's header line in clean mode shifts the second
// match's line offset relative to full mode - which is what a test can use
// to tell "hits were recomputed for this mode's layout" apart from "the
// same stale offsets are being reused".
func writeToggleModeTranscript(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < 30; i++ {
		switch i {
		case 10:
			b.WriteString(`{"type":"user","message":{"role":"user","content":"question about migration"},"timestamp":"2026-01-01T00:00:00Z"}` + "\n")
		case 20:
			b.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:00Z"}` + "\n")
			b.WriteString(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]},"timestamp":"2026-01-01T00:00:00Z"}` + "\n")
			b.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"migration fixed"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:00Z"}` + "\n")
		default:
			fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"routine filler number %d"},"timestamp":"2026-01-01T00:00:00Z"}`+"\n", i)
		}
	}
	p := filepath.Join(t.TempDir(), "toggle.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHitsAndNextMatchAreRecomputedAfterToggle is the regression test for
// "search hits are recomputed for the new line layout" and "n/N still
// correct": if convo.hits were left over from whichever mode was active
// before the toggle, n would either scroll to the wrong line or - since
// the two modes render different numbers of lines - potentially past the
// end of the new content entirely.
func TestHitsAndNextMatchAreRecomputedAfterToggle(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "L0", "claude:p:s0", "p", 1, map[string]any{
		"source": "claude", "cwd": "/work/repo", "git_common_root": "/work/repo",
		"topic": "the migration", "last_activity_at": 1700000000, "dir_exists": 1,
		"transcript_path": writeToggleModeTranscript(t),
	})
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Installs: testInstalls("claude-personal"),
	})
	m.tab = tabTranscript
	m.textFilter = "migration"
	m.View()

	cleanHits := append([]int(nil), m.convo.hits...)
	if len(cleanHits) != 2 {
		t.Fatalf("clean-mode hits = %v, want 2 matches", cleanHits)
	}

	m = update(t, m, keyRunes("n"))
	if m.detail.YOffset != cleanHits[0] {
		t.Errorf("n in clean mode went to %d, want the first hit at %d", m.detail.YOffset, cleanHits[0])
	}

	m.View()
	m = update(t, m, keyRunes("t"))
	if m.detail.YOffset != 0 {
		t.Errorf("toggling mode left the pane at %d, want it back at the top", m.detail.YOffset)
	}
	m.View()

	fullHits := append([]int(nil), m.convo.hits...)
	if len(fullHits) != 2 {
		t.Fatalf("full-mode hits after toggling = %v, want 2 matches", fullHits)
	}
	if reflect.DeepEqual(cleanHits, fullHits) {
		t.Errorf("hits were not recomputed for the new mode's line layout: still %v", fullHits)
	}

	m = update(t, m, keyRunes("n"))
	if m.detail.YOffset != fullHits[0] {
		t.Errorf("n in full mode went to %d, want the first hit at %d", m.detail.YOffset, fullHits[0])
	}
	m.View()
	m = update(t, m, keyRunes("n"))
	if m.detail.YOffset != fullHits[1] {
		t.Errorf("a second n in full mode went to %d, want the second hit at %d", m.detail.YOffset, fullHits[1])
	}
}
