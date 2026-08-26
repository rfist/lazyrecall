package cli

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"recall/internal/annotate"
	"recall/internal/profile"
	"recall/internal/schema"
	"recall/internal/search"
	"recall/internal/sqlitex"
)

// All fixtures below are synthetic, hand-written data - never real session
// content.

func browseTestDB(t *testing.T) *sqlitex.Runner {
	t.Helper()
	dir := t.TempDir()
	r := &sqlitex.Runner{DBPath: filepath.Join(dir, "recall.db")}
	if _, err := schema.Open(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func seedBrowseSession(t *testing.T, db *sqlitex.Runner, lineageID, sessionID, profileName string, handle int, extra map[string]any) {
	t.Helper()
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "handle"}, []map[string]any{
		{"id": lineageID, "profile": profileName, "orphaned": 0, "handle": handle},
	}); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{
		"id": sessionID, "source": "claude", "source_session_id": "s1", "lineage_id": lineageID,
		"end_state": "completed", "resumable": 1, "last_activity_at": 100,
	}
	for k, v := range extra {
		rec[k] = v
	}
	cols := make([]string, 0, len(rec))
	for k := range rec {
		cols = append(cols, k)
	}
	if err := b.BulkInsert("sessions", cols, []map[string]any{rec}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func newTestBrowser(db *sqlitex.Runner, name string, opts BrowserOptions) *browseModel {
	opts.DB = db
	opts.ProfileName = name
	m := newBrowseModel(opts)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return nm.(*browseModel)
}

// testResolve builds a resolver that returns the named profile verbatim -
// the browser's refresh/profile-switch actions need a profile.Profile, and
// profile.DBPath is derived from RECALL_HOME (set by the test) plus Name.
func testResolve(name string) func(string) (profile.Profile, error) {
	return func(n string) (profile.Profile, error) {
		if n != name {
			return profile.Profile{}, fmt.Errorf("no such profile %q", n)
		}
		return profile.Profile{Name: name}, nil
	}
}

// testProfiles builds a BrowserOptions.Profiles lister over a fixed,
// synthetic set of profile names - the profile-switch selection prompt
// (change choose-from-known-values) must never depend on this machine's
// real config roots, exactly like testResolve for the resolver.
func testProfiles(names ...string) func() []profile.Profile {
	return func() []profile.Profile {
		out := make([]profile.Profile, len(names))
		for i, n := range names {
			out[i] = profile.Profile{Name: n}
		}
		return out
	}
}

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// altKey builds an Alt-modified key press. Every test that uses it asserts
// the key is INERT - the browser must never bind an action to Alt (spec
// session-search, "The browser binds no Alt combinations").
func altKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Alt: true} }

func update(t *testing.T, m *browseModel, msg tea.Msg) *browseModel {
	t.Helper()
	nm, _ := m.Update(msg)
	return nm.(*browseModel)
}

// TestBrowseOpensOnRecentSessions covers spec session-search, "Opening the
// browser": the browser opens on the active profile's sessions, most recent
// first.
func TestBrowseOpensOnRecentSessions(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "older session", "last_activity_at": 100})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "newer session", "last_activity_at": 300})

	m := newTestBrowser(db, "p", BrowserOptions{})
	if len(m.visible) != 2 {
		t.Fatalf("visible = %d rows, want 2", len(m.visible))
	}
	if got := m.visible[0].Topic; got == nil || *got != "newer session" {
		t.Errorf("most recent first: first row topic = %v", got)
	}
	if !strings.Contains(m.View(), "profile: p") {
		t.Error("active profile must be visible at all times")
	}
	if !strings.Contains(m.View(), "newer session") {
		t.Error("list must render the sessions")
	}
}

// listPaneLines extracts the rows drawn between View()'s two separator
// rules - the list pane only, excluding the header, detail pane, input
// line, and footer, none of which are bound to a one-row-one-line
// invariant the way a decorated session row is.
func listPaneLines(view string) []string {
	lines := strings.Split(view, "\n")
	var seps []int
	for i, l := range lines {
		if l != "" && strings.Count(l, "─") == len([]rune(l)) {
			seps = append(seps, i)
		}
	}
	if len(seps) < 2 {
		return nil
	}
	return lines[seps[0]+1 : seps[1]]
}

// TestBrowseRowsFitWithDecorationAtSeveralWidths covers fix-row-width-budget
// tasks 1.1-1.3 and 3.1-3.3: a rendered row as actually drawn by the list -
// including the two-column selection marker "▸ " or its equivalent padding
// "  " that View() prepends - must fit within the terminal width at exactly
// one line, at a range of widths including one narrower than that decoration
// plus the shortest ordinary content. This is the defect itself: before the
// fix, RenderRow was handed the full m.width and the decoration was added
// afterwards, so every row was two columns over budget.
func TestBrowseRowsFitWithDecorationAtSeveralWidths(t *testing.T) {
	db := browseTestDB(t)
	longTopic := strings.Repeat("investigate why the retry loop drops tasks under load ", 3)
	longCWD := "/Users/example/personal/some/very/deeply/nested/project/directory/that/is/quite/long/indeed"
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{
		"topic": longTopic, "cwd": longCWD, "last_activity_at": 300,
	})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{
		"topic": "short", "cwd": "/short", "last_activity_at": 200,
	})

	m := newTestBrowser(db, "p", BrowserOptions{})
	for _, width := range []int{120, 80, 60, 40, 30, 25, 20} {
		nm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		mm := nm.(*browseModel)
		for _, line := range listPaneLines(mm.View()) {
			if !strings.HasPrefix(line, "▸ ") && !strings.HasPrefix(line, "  ") {
				t.Errorf("width %d: list-pane line missing its decoration prefix: %q", width, line)
				continue
			}
			if got := runewidth.StringWidth(line); got > width {
				t.Errorf("width %d: decorated row exceeds terminal width (%d cols): %q", width, got, line)
			}
		}
	}
}

// TestBrowseDrawnRowCountMatchesListHeight covers task 2.1: the number of
// rows the list believes it shows (listHeight) and the number of decorated
// row lines actually drawn in View() must come from - and therefore always
// agree with - the same calculation. Before the width-budget fix this could
// silently disagree whenever a row wrapped onto a second screen line.
func TestBrowseDrawnRowCountMatchesListHeight(t *testing.T) {
	db := browseTestDB(t)
	for i := 0; i < 10; i++ {
		seedBrowseSession(t, db, fmt.Sprintf("l%d", i), fmt.Sprintf("claude:p:%d", i), "p", i+1,
			map[string]any{"topic": fmt.Sprintf("session %d", i), "last_activity_at": 1000 - i})
	}
	m := newTestBrowser(db, "p", BrowserOptions{})

	drawn := len(listPaneLines(m.View()))
	want := m.listHeight()
	if want > len(m.visible) {
		want = len(m.visible)
	}
	if drawn != want {
		t.Errorf("drawn row lines = %d, want %d (listHeight=%d, visible=%d)", drawn, want, m.listHeight(), len(m.visible))
	}
}

// TestBrowseScrollKeepsSelectionVisible covers tasks 2.2 and 2.3: moving the
// selection past either end of the visible window scrolls the list so the
// selection stays drawn, all the way through a list longer than one screen,
// and jumping to the first/last session leaves it visible too. This is the
// scenario the reported bug broke: once rows silently wrapped, the selection
// and detail pane kept moving but the visible rows never changed, because
// the scroll math no longer matched what the terminal actually drew.
func TestBrowseScrollKeepsSelectionVisible(t *testing.T) {
	db := browseTestDB(t)
	const n = 50
	for i := 0; i < n; i++ {
		seedBrowseSession(t, db, fmt.Sprintf("l%d", i), fmt.Sprintf("claude:p:%d", i), "p", i+1,
			map[string]any{"topic": fmt.Sprintf("session %d", i), "last_activity_at": 1000 - i})
	}
	m := newTestBrowser(db, "p", BrowserOptions{})
	if h := m.listHeight(); h >= n {
		t.Fatalf("setup: need a list shorter than the row count to exercise scrolling, listHeight=%d", h)
	}

	for i := 0; i < n-1; i++ {
		m = update(t, m, keyRunes("j"))
		if m.cursor < m.listTop || m.cursor >= m.listTop+m.listHeight() {
			t.Fatalf("step %d: cursor %d not within the visible window [%d,%d)", i, m.cursor, m.listTop, m.listTop+m.listHeight())
		}
	}
	if m.cursor != n-1 {
		t.Errorf("expected the selection to reach the last session, got cursor=%d", m.cursor)
	}

	m = update(t, m, keyRunes("g"))
	if m.cursor != 0 || m.listTop != 0 {
		t.Errorf("g must jump to the first session and scroll it into view: cursor=%d listTop=%d", m.cursor, m.listTop)
	}
	m = update(t, m, keyRunes("G"))
	if m.cursor != n-1 {
		t.Errorf("G must jump to the last session: cursor=%d", m.cursor)
	}
	if m.cursor < m.listTop || m.cursor >= m.listTop+m.listHeight() {
		t.Errorf("last session must remain visible after G: cursor=%d listTop=%d listHeight=%d", m.cursor, m.listTop, m.listHeight())
	}
}

// TestSlashFiltersListedRows covers design.md decision 2's `/` binding and
// spec session-search "Beginning a filter": `/` enters input mode; the
// submitted value narrows the already-loaded rows in-process (fuzzy
// matching), distinct from `s` which re-queries the index. Submitting blank
// clears the filter again (task 2.4).
func TestSlashFiltersListedRows(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "configure goose mcp", "cwd": "/work/goose"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "refactor widget loader", "cwd": "/work/loader"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	if len(m.visible) != 2 {
		t.Fatalf("want 2 rows before filtering, got %d", len(m.visible))
	}

	m = update(t, m, keyRunes("/"))
	if m.mode != modeFuzzyFilter {
		t.Fatalf("/ must enter the fuzzy-filter input mode, got mode %d", m.mode)
	}
	m = update(t, m, keyRunes("goose"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeNone {
		t.Fatal("submitting the filter must return to normal mode")
	}
	if len(m.visible) != 1 {
		t.Fatalf("fuzzy narrowing: want 1 row, got %d", len(m.visible))
	}
	if got := m.visible[0].Topic; got == nil || *got != "configure goose mcp" {
		t.Errorf("fuzzy match = %v, want the goose session", got)
	}
	if !strings.Contains(m.View(), "filter: goose") {
		t.Error("the fuzzy filter in effect must be visible")
	}

	// Submitting blank to the same prompt clears the filter (task 2.4: "a
	// filter prompt" empty-submit clears that filter).
	m = update(t, m, keyRunes("/"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 2 {
		t.Errorf("blank submit on a filter prompt must clear it: got %d rows", len(m.visible))
	}
}

// TestSearchPhraseRunsExistingSearchPath covers design.md decision 2's `s`
// binding: setting a search phrase re-queries through search.Search, the
// FTS-backed path the non-interactive command uses - not just another
// client-side filter. `s` (index search) and `/` (row filter) stay distinct
// (design.md decision 2 notes).
func TestSearchPhraseRunsExistingSearchPath(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "unrelated topic"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "another topic"})
	b := db.NewBatch()
	if err := b.BulkInsert("prompt_fts", []string{"session_id", "kind", "text"}, []map[string]any{
		{"session_id": "claude:p:2", "kind": "prompt", "text": "the aurora password rotation"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("s"))
	if m.mode != modeSearchPhrase {
		t.Fatalf("s must enter the search-phrase input mode, got mode %d", m.mode)
	}
	m = update(t, m, keyRunes("aurora"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.visible) != 1 {
		t.Fatalf("search phrase: want 1 FTS match, got %d", len(m.visible))
	}
	if got := m.visible[0].SessionID; got != "claude:p:2" {
		t.Errorf("search match = %s, want the session whose prompt matched", got)
	}
	if !strings.Contains(m.View(), `search: "aurora"`) {
		t.Error("the search phrase must be shown as a filter in effect")
	}
}

// TestFilterPromptsAndClearingOneFilter covers the repository filter prompt
// (`r`, free text - task 2.4) and the agent filter prompt (`a`, a selection
// over the agents sessions actually exist for - task 2.2): setting a repo
// filter narrows; combining with a selected agent narrows further; clearing
// one filter leaves the others in effect.
func TestFilterPromptsAndClearingOneFilter(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"cwd": "/work/alpha", "git_common_root": "/work/alpha"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"cwd": "/work/beta", "git_common_root": "/work/beta", "topic": "beta work"})
	// A pi session in an unrelated repo - present so "pi" is an offered
	// agent candidate at all (task 2.2: candidates come from the sessions
	// actually present, not a hardcoded adapter list), and absent from
	// /work/alpha so combining it with the repo filter below still yields
	// zero rows, same as the scenario this test covered before.
	seedBrowseSession(t, db, "l3", "pi:p:3", "p", 3, map[string]any{"source": "pi", "cwd": "/other/repo", "git_common_root": "/other/repo"})

	m := newTestBrowser(db, "p", BrowserOptions{})

	// Set the repo filter to /work/alpha (free text - unchanged).
	m = update(t, m, keyRunes("r"))
	m = update(t, m, keyRunes("/work/alpha"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 1 || m.visible[0].SessionID != "claude:p:1" {
		t.Fatalf("repo filter: want only the alpha session, got %d rows", len(m.visible))
	}
	if !strings.Contains(m.View(), "repo: /work/alpha") {
		t.Error("the repo filter must be shown as in effect")
	}

	// Set an agent filter on top by selecting "pi" (matches nothing
	// combined with repo=/work/alpha, since the pi session lives
	// elsewhere).
	m = update(t, m, keyRunes("a"))
	if m.mode != modeSelect {
		t.Fatalf("a must open the selection prompt, mode = %v", m.mode)
	}
	if !reflect.DeepEqual(m.selectAll, []string{"claude", "pi"}) {
		t.Fatalf("agent candidates = %v, want [claude pi] from the sessions actually present", m.selectAll)
	}
	m = update(t, m, keyRunes("pi"))
	if !reflect.DeepEqual(m.selectFiltered, []string{"pi"}) {
		t.Fatalf("typing 'pi' must narrow the candidates to [pi], got %v", m.selectFiltered)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.agent != "pi" {
		t.Fatalf("agent = %q, want pi selected", m.agent)
	}
	if len(m.visible) != 0 {
		t.Fatalf("combined filters: want 0 rows, got %d", len(m.visible))
	}
	// Task 3.6: an empty result is reported without leaving the browser.
	if !strings.Contains(m.View(), "No session matched the filters in use.") {
		t.Error("empty result must be reported in the browser")
	}
	if m.mode != modeNone || !strings.Contains(m.View(), "profile: p") {
		t.Error("browser must remain open and usable on an empty result")
	}

	// Clear the agent filter by confirming the selection prompt with
	// nothing highlighted (design.md decision 4 - the selection-mode
	// equivalent of the old "submit blank"): the repo filter stays in
	// effect (task 3.5).
	m = update(t, m, keyRunes("a"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 1 || m.visible[0].SessionID != "claude:p:1" {
		t.Fatalf("clearing agent filter: want the alpha session back, got %d rows", len(m.visible))
	}
	if !strings.Contains(m.View(), "repo: /work/alpha") {
		t.Error("the other filter must stay in effect after clearing one")
	}
	if strings.Contains(m.View(), "agent: pi") {
		t.Error("the cleared filter must no longer be shown")
	}
}

// TestClearAllFiltersX covers design.md decision 2's `x` binding: it clears
// every filter dimension at once - repo, agent, tag, search phrase, and the
// `/` row filter - restoring the full list in one action.
func TestClearAllFiltersX(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"cwd": "/work/alpha", "git_common_root": "/work/alpha", "topic": "alpha work"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"cwd": "/work/beta", "git_common_root": "/work/beta", "topic": "beta work"})

	m := newTestBrowser(db, "p", BrowserOptions{Repo: "/work/alpha"})
	m = update(t, m, keyRunes("/"))
	m = update(t, m, keyRunes("alpha"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 1 {
		t.Fatalf("setup: want 1 row filtered, got %d", len(m.visible))
	}

	m = update(t, m, keyRunes("x"))
	if m.repo != "" || m.agent != "" || m.tag != "" || m.query != "" || m.fuzzyQuery != "" {
		t.Fatalf("x must clear every filter field: repo=%q agent=%q tag=%q query=%q fuzzy=%q",
			m.repo, m.agent, m.tag, m.query, m.fuzzyQuery)
	}
	if len(m.visible) != 2 {
		t.Errorf("after x, want every session listed again, got %d", len(m.visible))
	}
	v := m.View()
	if strings.Contains(v, "repo:") {
		t.Error("the view must not show a repo filter after x")
	}
	if !strings.Contains(v, "(no filters)") {
		t.Error("the view must show no filters in effect after x")
	}
}

// TestInputCancelAppliesNothing covers spec session-search, "Leaving input
// mode": Esc in an input mode returns to browsing with no change applied.
func TestInputCancelAppliesNothing(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "plain topic"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("s"))
	m = update(t, m, keyRunes("aurora"))
	if m.query != "" {
		t.Fatalf("query must not change before submit, got %q", m.query)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNone {
		t.Error("cancelling a prompt must return to browsing mode")
	}
	if m.query != "" {
		t.Errorf("cancelled prompt must not apply: query = %q", m.query)
	}
	if len(m.visible) != 1 {
		t.Errorf("cancelled prompt must not change the result set: %d rows", len(m.visible))
	}
}

// TestEmptySubmitAppliesNothing covers spec session-search, "Submitting an
// empty value" on a non-filter prompt (adding a tag): submitting a prompt
// with no text applies no change.
func TestEmptySubmitAppliesNothing(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "plain topic"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("m")) // add-tag prompt, not a filter
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	tags, err := annotate.TagsForLineage(db, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Errorf("blank submission must apply nothing, tags = %v", tags)
	}
}

// TestFilterPromptEmptySubmitClears covers task 2.4's exception: on a
// filter prompt specifically, submitting empty clears that one filter
// rather than applying nothing.
func TestFilterPromptEmptySubmitClears(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "plain topic"})

	m := newTestBrowser(db, "p", BrowserOptions{Query: "something"})
	m = update(t, m, keyRunes("s"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // blank submission
	if m.query != "" {
		t.Errorf("blank submission on the search-phrase filter prompt must clear it, query = %q", m.query)
	}
}

// TestTagAndCommentEditingUpdateImmediately covers the m/M/c/C bindings
// (design.md decision 2: "m/M add/remove tag", "c/C add/remove comment"):
// adding and removing a tag and a comment updates the list and detail
// immediately, without leaving the browser.
func TestTagAndCommentEditingUpdateImmediately(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "goose setup"})

	m := newTestBrowser(db, "p", BrowserOptions{})

	// Add a tag (m).
	m = update(t, m, keyRunes("m"))
	if m.mode != modeAddTag {
		t.Fatalf("m must enter the add-tag input mode, got %d", m.mode)
	}
	m = update(t, m, keyRunes("urgent"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeNone {
		t.Fatal("tag prompt must close after submit")
	}
	tags, err := annotate.TagsForLineage(db, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != "urgent" {
		t.Fatalf("tag was not persisted: %v", tags)
	}
	if !strings.Contains(m.View(), "#urgent") {
		t.Error("the list must show the new tag immediately")
	}

	// Add a comment (c).
	m = update(t, m, keyRunes("c"))
	if m.mode != modeAddComment {
		t.Fatalf("c must enter the add-comment input mode, got %d", m.mode)
	}
	m = update(t, m, keyRunes("needs verification"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	comments, err := annotate.CommentsForLineage(db, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "needs verification" {
		t.Fatalf("comment was not persisted: %+v", comments)
	}
	if !strings.Contains(m.View(), "needs verification") {
		t.Error("the detail must show the new comment immediately")
	}

	// Remove the tag (M).
	m = update(t, m, keyRunes("M"))
	if m.mode != modeRemoveTag {
		t.Fatalf("M must enter the remove-tag input mode, got %d", m.mode)
	}
	m = update(t, m, keyRunes("urgent"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	tags, err = annotate.TagsForLineage(db, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Errorf("tag removal was not persisted: %v", tags)
	}

	// Remove the comment by id (C), shown in the detail pane.
	m = update(t, m, keyRunes("C"))
	if m.mode != modeRemoveComment {
		t.Fatalf("C must enter the remove-comment input mode, got %d", m.mode)
	}
	m = update(t, m, keyRunes("1"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	comments, err = annotate.CommentsForLineage(db, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Errorf("comment removal was not persisted: %+v", comments)
	}
}

// TestEnterResumesSelectedAndQQuits covers resuming and quitting: Enter
// selects the current session and quits; `q` quits without selecting.
func TestEnterResumesSelectedAndQQuits(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "pick me", "cwd": "/work/x"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter must produce a quit command")
	}
	entered := nm.(*browseModel)
	if !entered.selectedOK {
		t.Fatal("Enter must mark a session selected")
	}
	if got := entered.selected.SessionID; got != "claude:p:1" {
		t.Errorf("selected = %s, want the current session", got)
	}

	m2 := newTestBrowser(db, "p", BrowserOptions{})
	nm2, cmd := m2.Update(keyRunes("q"))
	if cmd == nil {
		t.Fatal("q must produce a quit command")
	}
	if nm2.(*browseModel).selectedOK {
		t.Error("q must not select a session")
	}
}

// TestCtrlCQuits covers design.md decision 2's "q / Ctrl-C quit": Ctrl-C
// quits without selecting, the same as q.
func TestCtrlCQuits(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("Ctrl-C must produce a quit command")
	}
	if nm.(*browseModel).selectedOK {
		t.Error("Ctrl-C must not select a session")
	}
}

// TestEscDoesNothingInNormalMode covers design.md decision 2: Esc is bound
// only inside input mode (to cancel) and is not a normal-mode quit key -
// quitting is q/Ctrl-C only. Pressing Esc while browsing leaves the browser
// open with no change.
func TestEscDoesNothingInNormalMode(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "still browsing"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("Esc in normal mode must not quit the browser")
	}
	if nm.(*browseModel).selectedOK {
		t.Error("Esc in normal mode must not select a session")
	}
	if !strings.Contains(nm.(*browseModel).View(), "still browsing") {
		t.Error("the browser must remain open, showing its rows, after Esc in normal mode")
	}
}

// TestMovementKeysJKGG covers design.md decision 2's movement keys: j/k
// move the selection by one row; g/G jump to the first/last session.
func TestMovementKeysJKGG(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "one", "last_activity_at": 300})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "two", "last_activity_at": 200})
	seedBrowseSession(t, db, "l3", "claude:p:3", "p", 3, map[string]any{"topic": "three", "last_activity_at": 100})

	m := newTestBrowser(db, "p", BrowserOptions{})
	if m.cursor != 0 {
		t.Fatalf("setup: cursor must start at 0, got %d", m.cursor)
	}

	m = update(t, m, keyRunes("j"))
	if m.cursor != 1 {
		t.Errorf("j must move the selection down by one: cursor = %d", m.cursor)
	}
	m = update(t, m, keyRunes("k"))
	if m.cursor != 0 {
		t.Errorf("k must move the selection up by one: cursor = %d", m.cursor)
	}
	m = update(t, m, keyRunes("G"))
	if m.cursor != 2 {
		t.Errorf("G must jump to the last session: cursor = %d", m.cursor)
	}
	m = update(t, m, keyRunes("g"))
	if m.cursor != 0 {
		t.Errorf("g must jump to the first session: cursor = %d", m.cursor)
	}
	// Arrow keys keep working alongside j/k (spec "Arrow keys still work").
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("down arrow must still move the selection: cursor = %d", m.cursor)
	}
}

// TestCtrlDCtrlUHalfScreen covers design.md decision 2's "Ctrl-D / Ctrl-U
// move down / up by half a screen".
func TestCtrlDCtrlUHalfScreen(t *testing.T) {
	db := browseTestDB(t)
	for i := 0; i < 40; i++ {
		seedBrowseSession(t, db, fmt.Sprintf("l%d", i), fmt.Sprintf("claude:p:%d", i), "p", i+1,
			map[string]any{"topic": fmt.Sprintf("session %d", i), "last_activity_at": 1000 - i})
	}

	m := newTestBrowser(db, "p", BrowserOptions{})
	half := m.halfScreen()
	if half < 1 {
		t.Fatalf("halfScreen must be at least 1, got %d", half)
	}

	m = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.cursor != half {
		t.Errorf("Ctrl-D must move the selection down by half a screen (%d): cursor = %d", half, m.cursor)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.cursor != 0 {
		t.Errorf("Ctrl-U must move the selection back up by half a screen: cursor = %d", m.cursor)
	}
}

// TestNormalModeLettersInvokeActionsNotText covers spec session-search,
// "Typing in normal mode": a letter bound to an action invokes it, and is
// never entered as text anywhere (there is no text buffer at all in normal
// mode - only input mode has one).
func TestNormalModeLettersInvokeActionsNotText(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "a"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "b"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("j"))
	if m.mode != modeNone {
		t.Fatal("j must invoke movement, not open an input mode")
	}
	if m.fuzzyQuery != "" || m.query != "" || m.repo != "" {
		t.Error("no normal-mode letter may leak into any filter/query field as text")
	}
	if m.cursor != 1 {
		t.Errorf("j must have moved the selection: cursor = %d", m.cursor)
	}
}

// TestInputModeTypingIsTextNotAction covers spec session-search, "Typing in
// input mode": once an input mode is active, typing a letter that is bound
// to an action in normal mode (like j, used here for movement) is entered
// as text instead, and the action is not invoked.
func TestInputModeTypingIsTextNotAction(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "a"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "b"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("/")) // enter input mode
	startCursor := m.cursor
	m = update(t, m, keyRunes("j")) // "j" is text here, not movement
	if m.cursor != startCursor {
		t.Errorf("j typed in input mode must not move the selection: cursor changed from %d to %d", startCursor, m.cursor)
	}
	if m.input.Value() != "j" {
		t.Errorf("j typed in input mode must be entered as text, input = %q", m.input.Value())
	}
}

// TestAltKeysAreInert covers spec session-search, "The browser binds no Alt
// combinations": every letter that appears anywhere in the binding table,
// pressed with Alt held, must do nothing - no input mode opens, no filter
// changes, no command runs, and (task 1.3) no action anywhere is reachable
// through an Alt or Meta combination.
func TestAltKeysAreInert(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})

	letters := []string{"s", "r", "a", "t", "p", "x", "l", "g", "j", "k", "m", "c", "q"}
	for _, l := range letters {
		m := newTestBrowser(db, "p", BrowserOptions{})
		nm, cmd := m.Update(altKey(l))
		got := nm.(*browseModel)
		if got.mode != modeNone {
			t.Errorf("alt-%s must not open an input mode", l)
		}
		if got.help {
			t.Errorf("alt-%s must not open help", l)
		}
		if got.selectedOK {
			t.Errorf("alt-%s must not select/quit", l)
		}
		if cmd != nil {
			t.Errorf("alt-%s must not produce a command", l)
		}
		if got.repo != "" || got.agent != "" || got.tag != "" || got.query != "" || got.fuzzyQuery != "" {
			t.Errorf("alt-%s must not change any filter", l)
		}
	}
}

// TestProfileSwitchReplacesResultSet covers spec session-search, "Switching
// profile replaces the view": the new profile's sessions replace the listed
// ones entirely; sessions from two profiles are never listed together.
func TestProfileSwitchReplacesResultSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RECALL_HOME", home)

	pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "profile p session"})
	qdb := browseTestDBAt(t, filepath.Join(home, "q.db"))
	seedBrowseSession(t, qdb, "lq", "claude:q:1", "q", 1, map[string]any{"topic": "profile q session"})

	m := newTestBrowser(pdb, "p", BrowserOptions{Resolve: testResolve("q"), Profiles: testProfiles("p", "q")})
	if !strings.Contains(m.View(), "profile p session") {
		t.Fatal("setup: p's session should be listed")
	}

	// p opens the profile selection prompt (change choose-from-known-values:
	// this is now a selection over the discovered profiles, not free text) -
	// "q" is the only offered candidate (the active profile "p" is excluded
	// from its own switch-to list), so moving down once highlights it.
	m = update(t, m, keyRunes("p"))
	if m.mode != modeSelect {
		t.Fatalf("p must open the selection prompt, mode = %v", m.mode)
	}
	if !reflect.DeepEqual(m.selectFiltered, []string{"q"}) {
		t.Fatalf("profile candidates = %v, want just [q] (the active profile excluded)", m.selectFiltered)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("profile submit must produce a switch command")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("switch command must produce a message")
	}
	m = update(t, nm.(*browseModel), msg)

	if m.profileName != "q" {
		t.Errorf("profileName = %s, want q", m.profileName)
	}
	v := m.View()
	if !strings.Contains(v, "profile q session") {
		t.Error("the new profile's session must be listed")
	}
	if strings.Contains(v, "profile p session") {
		t.Error("the previous profile's session must never remain listed")
	}
}

// TestProfileSelectConfirmWithNothingHighlightedKeepsCurrent covers
// design.md decision 4: confirming a selection prompt with nothing
// highlighted applies nothing. The profile switch is the case that is not
// also a filter (agent/tag filters clear on the same input instead - see
// TestSelectFilterConfirmWithNothingHighlightedClears); a picker can no
// longer be pointed at a name that does not resolve at all (that entire
// defect class - the old free-text prompt's whole reason for existing bugs
// here - is gone by construction now that only real candidates are ever
// offered), so this replaces the old invalid-name test with the selection
// prompt's actual "decline to choose" path: open the prompt and press Enter
// immediately, before ever moving the highlight.
func TestProfileSelectConfirmWithNothingHighlightedKeepsCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RECALL_HOME", home)
	pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "still here"})

	m := newTestBrowser(pdb, "p", BrowserOptions{Resolve: testResolve("q"), Profiles: testProfiles("p", "q")})
	m = update(t, m, keyRunes("p"))
	if m.selectCursor != -1 {
		t.Fatalf("setup: a freshly opened selection prompt must start with nothing highlighted, got cursor=%d", m.selectCursor)
	}
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*browseModel)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Fatalf("confirming with nothing highlighted must not attempt a profile switch, got message %#v", msg)
		}
	}
	if m.mode != modeNone {
		t.Error("confirming must leave selection mode even when nothing was highlighted")
	}
	if m.profileName != "p" {
		t.Errorf("profileName = %s, want the current profile p unchanged", m.profileName)
	}
	if !strings.Contains(m.View(), "still here") {
		t.Error("the current profile's session must remain listed")
	}
}

// chunkReader feeds a sequence of pre-formed byte chunks to a reader, one
// chunk per Read call, each in a single call rather than one byte at a
// time. bubbletea's key parser needs a full escape sequence (e.g. the three
// bytes of a Down arrow, "\x1b[B") available in one read to recognise it as
// one key rather than a lone Esc followed by two typed characters - a real
// terminal's tty driver always delivers a fast burst like that as one
// chunk, so this reproduces normal delivery rather than an artificially
// slow, one-byte-at-a-time source.
type chunkReader struct {
	chunks chan []byte
	buf    []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		b, ok := <-r.chunks
		if !ok {
			return 0, io.EOF
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// syncBuffer is an io.Writer safe for one writer goroutine (the running
// tea.Program) and one concurrent reader goroutine (the test, polling
// Contains) - a plain bytes.Buffer is not safe for that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buf.String(), s)
}

// TestProfileSwitchThroughRealProgramEventLoop covers change
// fix-row-newlines-and-profile-switch task 3.1: diagnosing "choosing another
// profile has no effect" before changing anything. Every other profile-
// switch test in this file drives the model by calling Update (and any
// returned tea.Cmd) directly - which proves the *model's* logic is correct,
// but not that bubbletea's own runtime actually delivers a real key press
// through Update and feeds the tea.Cmd it returns back in as a message,
// which is the one part of "the command it returns does not reach the
// browser" (design.md decision 4's second hypothesis) no purely manual test
// can rule out. This test runs the real tea.Program event loop - the same
// one RunBrowser constructs - end to end over a piped input, pressing "p",
// Down, and Enter as actual bytes a terminal would send, with no test code
// calling Update or a returned command itself.
//
// Established cause (recorded per design.md decision 4): there is none -
// this path already works. Driven through the real event loop with
// realistic key bytes, submitSelect -> submitInput -> switchProfileCmd ->
// dbSwitchedMsg correctly replaces both the listed sessions and the
// displayed profile. The bug this task set out to diagnose does not
// reproduce against the current code: the free-text profile prompt that
// could plausibly have carried a decorated value into submitInput (design.md
// decision 4's first hypothesis - matching the "switch to profile: switch
// to profile:" placeholder-doubling bug fixed in the prior
// choose-from-known-values change, task 3.1) was replaced by this
// selection-only prompt, which never submits anything but an exact,
// already-valid profile name - eliminating that entire defect class by
// construction. This test is the regression guard: it fails if that ever
// regresses, through any path (model-level or real event-loop-level).
func TestProfileSwitchThroughRealProgramEventLoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RECALL_HOME", home)

	pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "profile p session"})
	qdb := browseTestDBAt(t, filepath.Join(home, "q.db"))
	seedBrowseSession(t, qdb, "lq", "claude:q:1", "q", 1, map[string]any{"topic": "profile q session"})

	opts := BrowserOptions{
		DB: pdb, ProfileName: "p", Style: false,
		Resolve:  testResolve("q"),
		Profiles: testProfiles("p", "q"),
	}
	m := newBrowseModel(opts)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = *nm.(*browseModel)

	in := &chunkReader{chunks: make(chan []byte, 8)}
	var out syncBuffer
	p := tea.NewProgram(&m, tea.WithInput(in), tea.WithOutput(&out))

	done := make(chan struct{})
	var final tea.Model
	var runErr error
	go func() {
		final, runErr = p.Run()
		close(done)
	}()

	in.chunks <- []byte("p")      // open the profile selection prompt
	in.chunks <- []byte("\x1b[B") // Down arrow, delivered as one chunk
	in.chunks <- []byte("\r")     // confirm the highlighted candidate

	// switchProfileCmd (spawned by the "\r" above) is itself asynchronous -
	// bubbletea runs it in its own goroutine and only feeds its result back
	// in as a dbSwitchedMsg once that goroutine returns. Sending "q" without
	// waiting for that to land races the real quit against the real switch:
	// tea.Quit (from "q") can end the program before the switch's result
	// message is even in the queue, which looks exactly like "switching had
	// no effect" but is a race in this test's own timing, not a defect in
	// the program - so wait for the switched-to profile's session to
	// actually be drawn before quitting, the same way a person would wait
	// to see it happen before pressing the next key.
	deadline := time.Now().Add(5 * time.Second)
	for !out.Contains("profile q session") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !out.Contains("profile q session") {
		t.Fatal("profile q's session never appeared in the rendered output within 5s")
	}
	in.chunks <- []byte("q") // quit
	close(in.chunks)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not quit within 5s after q")
	}
	if runErr != nil {
		t.Fatalf("p.Run: %v", runErr)
	}

	fm := final.(*browseModel)
	if fm.profileName != "q" {
		t.Errorf("profileName = %s, want q - switching profile through the real event loop had no effect", fm.profileName)
	}
	view := fm.View()
	if !strings.Contains(view, "profile q session") {
		t.Error("the new profile's session must be listed after switching through the real event loop")
	}
	if strings.Contains(view, "profile p session") {
		t.Error("the previous profile's session must never remain listed - no listing may span two profiles")
	}
}

// TestProfileSwitchCannotBeOpenedReportsAndKeepsCurrent covers task 3.3 and
// spec session-search scenario "Chosen profile cannot be opened": if the
// candidate profile fails to resolve (design.md decision 4 also names this
// as one of the paths worth ruling out explicitly - dbSwitchedMsg carrying
// an error), the browser reports why and stays on the current profile and
// its own listed sessions, rather than silently doing nothing or crashing.
func TestProfileSwitchCannotBeOpenedReportsAndKeepsCurrent(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "still here"})

	failResolve := func(name string) (profile.Profile, error) {
		return profile.Profile{}, fmt.Errorf("profile %q: database is locked", name)
	}
	m := newTestBrowser(db, "p", BrowserOptions{Resolve: failResolve, Profiles: testProfiles("p", "q")})
	m = update(t, m, keyRunes("p"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown}) // highlight "q"
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*browseModel)
	if cmd == nil {
		t.Fatal("confirming a real candidate must still produce a switch command")
	}
	msg := cmd()
	m2, _ := m.Update(msg)
	m = m2.(*browseModel)

	if m.profileName != "p" {
		t.Errorf("profileName = %s, want p unchanged - a profile that cannot be opened must not become active", m.profileName)
	}
	if m.notice == "" {
		t.Error("expected a notice explaining why the switch failed")
	}
	if !strings.Contains(m.notice, "database is locked") {
		t.Errorf("notice must report why, got %q", m.notice)
	}
	if !strings.Contains(m.View(), "still here") {
		t.Error("the current profile's session must remain listed")
	}
}

// TestSelectTypeToNarrowThenEnterAppliesTheMatch covers change
// fix-row-newlines-and-profile-switch task 3.1/3.2's ESTABLISHED CAUSE:
// choosing a profile appeared to have no effect because the real user flow
// - open the prompt, type to narrow ("claude"), press Enter, never
// touching Up/Down - left selectCursor at -1. refilterSelect used to only
// clamp the cursor into the narrowed bounds, never move it off -1, so
// Enter submitted an empty value and submitInput's "empty value applies
// nothing" case fired silently (a filter prompt would have looked like it
// "cleared" instead - for the profile prompt, "applies nothing" is exactly
// "choosing another profile has no effect"). This is the flow a person
// actually uses; the earlier TestProfileSwitchReplacesResultSet and
// TestProfileSwitchThroughRealProgramEventLoop both explicitly move the
// cursor with a Down keypress before confirming and so never exercised it.
//
// Fixed by auto-highlighting the best (first-ranked) match once typing has
// narrowed the list and the cursor has never been explicitly touched -
// this test drives all three selection prompts (profile, agent, tag) with
// exactly this flow and no other keys.
func TestSelectTypeToNarrowThenEnterAppliesTheMatch(t *testing.T) {
	t.Run("profile", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("RECALL_HOME", home)
		pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
		seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "profile p session"})
		qdb := browseTestDBAt(t, filepath.Join(home, "q.db"))
		seedBrowseSession(t, qdb, "lq", "claude:q:1", "q", 1, map[string]any{"topic": "profile q session"})

		m := newTestBrowser(pdb, "p", BrowserOptions{Resolve: testResolve("q"), Profiles: testProfiles("p", "q")})
		m = update(t, m, keyRunes("p"))
		m = update(t, m, keyRunes("q")) // type to narrow to the single "q" candidate - no Up/Down
		if m.selectCursor != 0 {
			t.Fatalf("typing to narrow to one match must auto-highlight it, selectCursor = %d", m.selectCursor)
		}
		nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = nm.(*browseModel)
		if cmd == nil {
			t.Fatal("Enter after narrowing to one match must produce a switch command")
		}
		msg := cmd()
		if msg == nil {
			t.Fatal("switch command must produce a message")
		}
		nm2, _ := m.Update(msg)
		m = nm2.(*browseModel)
		if m.profileName != "q" {
			t.Errorf("profileName = %s, want q", m.profileName)
		}
		if !strings.Contains(m.View(), "profile q session") {
			t.Error("the new profile's session must be listed")
		}
	})

	t.Run("agent", func(t *testing.T) {
		db := browseTestDB(t)
		seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
		seedBrowseSession(t, db, "l2", "pi:p:2", "p", 2, map[string]any{"source": "pi", "topic": "y"})

		m := newTestBrowser(db, "p", BrowserOptions{})
		m = update(t, m, keyRunes("a"))
		m = update(t, m, keyRunes("pi")) // narrow to the single "pi" candidate - no Up/Down
		if m.selectCursor != 0 {
			t.Fatalf("typing to narrow to one match must auto-highlight it, selectCursor = %d", m.selectCursor)
		}
		m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.agent != "pi" {
			t.Errorf("agent = %q, want pi selected purely by typing then Enter", m.agent)
		}
	})

	t.Run("tag", func(t *testing.T) {
		db := browseTestDB(t)
		seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
		if err := annotate.AddTag(db, "l1", "urgent"); err != nil {
			t.Fatal(err)
		}

		m := newTestBrowser(db, "p", BrowserOptions{})
		m = update(t, m, keyRunes("t"))
		m = update(t, m, keyRunes("urgent")) // narrow to the single "urgent" candidate - no Up/Down
		if m.selectCursor != 0 {
			t.Fatalf("typing to narrow to one match must auto-highlight it, selectCursor = %d", m.selectCursor)
		}
		m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.tag != "urgent" {
			t.Errorf("tag = %q, want urgent selected purely by typing then Enter", m.tag)
		}
	})
}

// TestSelectBlankConfirmStillAppliesNothingAfterTyping covers the
// preserved half of design.md decision 4 (choose-from-known-values):
// clearing the typed narrowing text back to blank, having never touched
// Up/Down, must revert to "nothing highlighted" - opening a prompt and
// confirming immediately (with or without a type-then-clear detour) must
// still apply nothing (or clear a filter), never select whatever the
// candidate list happened to contain.
func TestSelectBlankConfirmStillAppliesNothingAfterTyping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RECALL_HOME", home)
	pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "still here"})

	m := newTestBrowser(pdb, "p", BrowserOptions{Resolve: testResolve("q"), Profiles: testProfiles("p", "q")})
	m = update(t, m, keyRunes("p"))
	m = update(t, m, keyRunes("q"))
	if m.selectCursor != 0 {
		t.Fatalf("setup: typing must auto-highlight, got cursor=%d", m.selectCursor)
	}
	// Clear the typed text back to blank with backspace, never touching
	// Up/Down.
	m = update(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	if m.selectCursor != -1 {
		t.Errorf("clearing the typed text back to blank must revert to nothing highlighted, got cursor=%d", m.selectCursor)
	}
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*browseModel)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Fatalf("confirming after typing then clearing must apply nothing, got message %#v", msg)
		}
	}
	if m.profileName != "p" {
		t.Errorf("profileName = %s, want p unchanged", m.profileName)
	}
}

// TestSelectExplicitNavigationSurvivesFurtherTyping covers the other edge
// of the same fix: once the user has explicitly moved the cursor with
// Up/Down, further typing must never silently override their choice by
// auto-highlighting a different candidate - refilterSelect only clamps
// once selectCursorTouched is set.
func TestSelectExplicitNavigationSurvivesFurtherTyping(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
	seedBrowseSession(t, db, "l2", "pi:p:2", "p", 2, map[string]any{"source": "pi", "topic": "y"})
	seedBrowseSession(t, db, "l3", "omp:p:3", "p", 3, map[string]any{"source": "omp", "topic": "z"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("a"))
	if !reflect.DeepEqual(m.selectAll, []string{"claude", "omp", "pi"}) {
		t.Fatalf("setup: agent candidates = %v", m.selectAll)
	}
	// Move down twice (touched=true after the first press: -1 -> 0 ->
	// 1), landing on "omp" (index 1 of [claude omp pi]).
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.selectCursor != 1 || m.selectFiltered[m.selectCursor] != "omp" {
		t.Fatalf("setup: expected cursor on omp, got index %d (%v)", m.selectCursor, m.selectFiltered)
	}
	// Type a query that still matches multiple candidates including omp -
	// the explicit navigation must survive, not get reset to the new
	// first-ranked match.
	m = update(t, m, keyRunes("o"))
	if m.selectFiltered[m.selectCursor] != "omp" {
		t.Errorf("typing after explicit navigation must not move the cursor off the user's choice: filtered=%v cursor=%d", m.selectFiltered, m.selectCursor)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.agent != "omp" {
		t.Errorf("agent = %q, want omp (the explicitly navigated candidate) to survive further typing", m.agent)
	}
}

// TestSelectModeCancelAppliesNothing covers Esc on the selection mode
// (design.md decision 4's "cancel" side, mirroring TestInputCancelAppliesNothing
// for the free-text prompts): cancelling a selection prompt applies nothing,
// even after the user has typed and navigated within it.
func TestSelectModeCancelAppliesNothing(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
	if err := annotate.AddTag(db, "l1", "blue"); err != nil {
		t.Fatal(err)
	}

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("t"))
	if m.mode != modeSelect {
		t.Fatalf("t must open the selection prompt, mode = %v", m.mode)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNone {
		t.Error("Esc must return to browsing mode")
	}
	if m.tag != "" {
		t.Errorf("cancelled selection must not apply: tag = %q", m.tag)
	}
}

// TestSelectEmptyCandidateSetReportsAndAppliesNothing covers task 1.4 and
// design.md decision 2's consequence: when a selection prompt's candidate
// set is empty (no tags have been applied to anything yet), the browser
// says so and applies no change - it never opens selection mode over an
// empty list.
func TestSelectEmptyCandidateSetReportsAndAppliesNothing(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("t")) // tag filter - no tags exist anywhere yet
	if m.mode != modeNone {
		t.Errorf("an empty candidate set must not enter selection mode, mode = %v", m.mode)
	}
	if m.notice == "" {
		t.Error("an empty candidate set must be reported via a notice")
	}
	if m.tag != "" {
		t.Errorf("tag = %q, want unchanged", m.tag)
	}
}

// TestSelectTagCandidatesFromAllTags covers task 2.3: the tag filter's
// candidates are the tags currently in use (annotate.AllTags) - not, for
// instance, every tag ever applied anywhere without regard to current
// state, and not a hardcoded list.
func TestSelectTagCandidatesFromAllTags(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "y"})
	if err := annotate.AddTag(db, "l1", "urgent"); err != nil {
		t.Fatal(err)
	}
	if err := annotate.AddTag(db, "l2", "later"); err != nil {
		t.Fatal(err)
	}
	want, err := annotate.AllTags(db)
	if err != nil {
		t.Fatal(err)
	}

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("t"))
	if m.mode != modeSelect {
		t.Fatalf("t must open the selection prompt, mode = %v", m.mode)
	}
	if !reflect.DeepEqual(m.selectAll, want) {
		t.Errorf("tag candidates = %v, want annotate.AllTags's own result %v", m.selectAll, want)
	}
}

// TestFreeTextPromptsUnchangedByChooseFromKnownValues covers task 2.4: the
// repository filter, the search phrase, the in-list fuzzy filter, and
// comment/tag creation must stay free text - none of them enter modeSelect.
// Tag creation in particular must still accept a value that does not exist
// yet (design.md decision 3's stated asymmetry with the tag filter).
func TestFreeTextPromptsUnchangedByChooseFromKnownValues(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})

	cases := []struct {
		key  string
		mode inputMode
	}{
		{"r", modeRepoFilter},
		{"s", modeSearchPhrase},
		{"/", modeFuzzyFilter},
		{"m", modeAddTag},
		{"M", modeRemoveTag},
		{"c", modeAddComment},
		{"C", modeRemoveComment},
	}
	for _, c := range cases {
		m := newTestBrowser(db, "p", BrowserOptions{})
		m = update(t, m, keyRunes(c.key))
		if m.mode != c.mode {
			t.Errorf("key %q: mode = %v, want %v (free text, unchanged by this change)", c.key, m.mode, c.mode)
		}
	}

	// Tag creation specifically must accept a brand-new value.
	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("m"))
	m = update(t, m, keyRunes("brand-new-tag"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	tags, err := annotate.TagsForLineage(db, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != "brand-new-tag" {
		t.Errorf("expected the new tag to be accepted even though it did not previously exist, got %v", tags)
	}
}

// TestSelectPromptPlaceholderNotLabel covers task 3.1: a selection prompt's
// input placeholder must not repeat its own label (the same defect
// browse.go:506 had for free-text prompts before this change - the field
// used to read "switch to profile: switch to profile:").
func TestSelectPromptPlaceholderNotLabel(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
	if err := annotate.AddTag(db, "l1", "urgent"); err != nil {
		t.Fatal(err)
	}

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("t"))
	if m.mode != modeSelect {
		t.Fatalf("setup: t must open the selection prompt, mode = %v", m.mode)
	}
	if m.input.Placeholder == m.inputLabel {
		t.Errorf("placeholder must not repeat the label, got placeholder=%q label=%q", m.input.Placeholder, m.inputLabel)
	}
}

// TestPromptLabelShownExactlyOnce covers task 3.2, for both a free-text
// prompt and a selection prompt: the label appears exactly once in the
// rendered view.
func TestPromptLabelShownExactlyOnce(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
	if err := annotate.AddTag(db, "l1", "urgent"); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"r", "t"} { // one free-text prompt, one selection prompt
		m := newTestBrowser(db, "p", BrowserOptions{})
		m = update(t, m, keyRunes(key))
		if m.inputLabel == "" {
			t.Fatalf("key %q: setup: expected a non-empty label", key)
		}
		if got := strings.Count(m.View(), m.inputLabel); got != 1 {
			t.Errorf("key %q: label %q appears %d times in the view, want exactly once", key, m.inputLabel, got)
		}
	}
}

// TestRefreshActionReloads covers design.md decision 2's `R` binding: the
// explicit refresh action runs the refresh pass for the active profile and
// reports back without leaving the browser.
func TestRefreshActionReloads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RECALL_HOME", home)
	pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "session one"})

	m := newTestBrowser(pdb, "p", BrowserOptions{Resolve: testResolve("p")})
	nm, cmd := m.Update(keyRunes("R"))
	if cmd == nil {
		t.Fatal("R must produce a refresh command")
	}
	msg := cmd()
	if rd, ok := msg.(refreshDoneMsg); !ok || rd.err != nil {
		t.Fatalf("refresh command produced %#v, want a successful refreshDoneMsg", msg)
	}
	m = update(t, nm.(*browseModel), msg)
	if !strings.Contains(m.View(), "index refreshed.") {
		t.Error("the refresh outcome should be reported in the browser")
	}
	if len(m.visible) != 1 {
		t.Errorf("rows must reload after refresh: %d", len(m.visible))
	}
}

// TestHelpRevealsEveryAction covers spec session-search, "Learning what can
// be done" (task 5.4): '?' reveals every available action and its key; Esc
// closes the help and returns to browsing.
func TestHelpRevealsEveryAction(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})

	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, keyRunes("?"))
	if !m.help {
		t.Fatal("? must open the help overlay")
	}
	help := m.View()
	for _, a := range browseActions {
		if !strings.Contains(help, a.key) || !strings.Contains(help, a.label) {
			t.Errorf("help must list %q (%s)", a.key, a.label)
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.help {
		t.Error("Esc must close the help overlay")
	}
}

// TestHelpAndHintsHaveNoAltBinding covers design.md decision 3 together
// with the hard "no Alt" constraint: since the footer hints and the help
// overlay are both derived from the same browseActions table, asserting no
// entry mentions "alt" once here rules out both surfaces drifting back to
// an unreachable binding.
func TestHelpAndHintsHaveNoAltBinding(t *testing.T) {
	for _, a := range browseActions {
		lower := strings.ToLower(a.key)
		if strings.Contains(lower, "alt") || strings.Contains(lower, "meta") {
			t.Errorf("binding table entry %q must not reference Alt/Meta: %+v", a.key, a)
		}
	}
}

// TestCommonActionsVisibleWithoutBeingRequested covers spec
// session-search, "Common actions are visible without being requested"
// (task 5.3): the footer names the most common actions at all times.
func TestCommonActionsVisibleWithoutBeingRequested(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "x"})
	m := newTestBrowser(db, "p", BrowserOptions{})
	v := m.View()
	for _, a := range browseActions {
		if a.common && !strings.Contains(v, a.label) {
			t.Errorf("common action %q must be visible without being requested", a.label)
		}
	}
}

// TestNoStylingWhenDisabled covers spec session-search, "Styling disabled
// by the environment" (task 5.2): with style off, the browser emits no ANSI
// codes anywhere.
func TestNoStylingWhenDisabled(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "styled topic", "cwd": "/work/x"})
	m := newTestBrowser(db, "p", BrowserOptions{Style: false})
	if strings.Contains(m.View(), "\x1b") {
		t.Error("no styling must be emitted when the environment requests no colour")
	}
}

// TestStylingWhenEnabled covers spec session-search, "Browsing on a
// terminal": with style on, the rows and detail carry styling rendered as
// ANSI codes (and the detail's identifier/handle stay plain, copyable
// text - task 4.7).
func TestStylingWhenEnabled(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "styled topic", "cwd": "/work/x"})
	m := newTestBrowser(db, "p", BrowserOptions{Style: true})
	v := m.View()
	if !strings.Contains(v, "\x1b") {
		t.Error("styling must be emitted when enabled")
	}
	if !strings.Contains(v, "handle: #1") {
		t.Error("the detail must show the handle as plain copyable text")
	}
	if !strings.Contains(v, "id:     claude:p:1") {
		t.Error("the detail must show the composite identifier as plain copyable text")
	}
}

// TestTwoBrowsersDoNotDisturbEachOther covers task 6.5: two browsers
// running at once hold independent state; acting in one never changes the
// other.
func TestTwoBrowsersDoNotDisturbEachOther(t *testing.T) {
	db1 := browseTestDB(t)
	seedBrowseSession(t, db1, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "browser one"})
	db2 := browseTestDB(t)
	seedBrowseSession(t, db2, "l2", "claude:p:1", "p", 1, map[string]any{"topic": "browser two"})

	a := newTestBrowser(db1, "p", BrowserOptions{})
	b := newTestBrowser(db2, "p", BrowserOptions{})

	// Filter browser a only.
	a = update(t, a, keyRunes("/"))
	a = update(t, a, keyRunes("browser one"))
	a = update(t, a, tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.visible) != 1 || !strings.Contains(a.View(), "browser one") {
		t.Error("browser a must filter its own rows")
	}
	if len(b.visible) != 1 || !strings.Contains(b.View(), "browser two") {
		t.Error("browser b must be unaffected by browser a")
	}
	// Annotate in browser b only.
	b = update(t, b, keyRunes("m"))
	b = update(t, b, keyRunes("mine"))
	b = update(t, b, tea.KeyMsg{Type: tea.KeyEnter})
	tags, err := annotate.TagsForLineage(db2, "l2")
	if err != nil || len(tags) != 1 || tags[0] != "mine" {
		t.Errorf("browser b's tag must land in its own database: %v %v", tags, err)
	}
	tags, err = annotate.TagsForLineage(db1, "l1")
	if err != nil || len(tags) != 0 {
		t.Errorf("browser a's database must be untouched: %v %v", tags, err)
	}
}

// TestHighlightLineReappliesReverseAfterFieldResets: a styled row contains
// one ANSI reset per styled field; the reverse highlight must survive
// every one of them, and the stripped output must stay identical.
func TestHighlightLineReappliesReverseAfterFieldResets(t *testing.T) {
	line := "\x1b[36m#1\x1b[0m \x1b[2m[claude]\x1b[0m topic"
	hl := highlightLine(line)
	if !strings.HasPrefix(hl, "\x1b[7m") || !strings.HasSuffix(hl, "\x1b[0m") {
		t.Fatalf("highlight must wrap the line: %q", hl)
	}
	if strings.Count(hl, "\x1b[7m") < 2 {
		t.Errorf("reverse must be re-applied after each field reset: %q", hl)
	}
	if stripANSI(hl) != stripANSI(line) {
		t.Errorf("highlighting must not change the visible text")
	}
	if hl == line {
		t.Error("highlighting must actually change the line")
	}
}

// TestBrowseRowsSanitizedButDetailPaneShowsLineBreaks covers change
// fix-row-newlines-and-profile-switch tasks 1.3/1.4 and spec session-search
// scenarios "Topic containing a line break" / "Stored text is unaffected":
// a session whose topic contains a line break must occupy exactly one line
// in the list, with its row content sanitized, but the detail pane for that
// same session - shown alongside the list, spec session-search "detail
// alongside the list" - must still show the topic across lines, since
// design.md's non-goal is explicit that the detail pane may keep doing so.
func TestBrowseRowsSanitizedButDetailPaneShowsLineBreaks(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{
		"topic": "fix the\nauth bug", "cwd": "/repo",
	})
	m := newTestBrowser(db, "p", BrowserOptions{})

	row := RenderRow(m.visible[0], RenderOptions{Width: m.width, Style: false})
	if strings.Contains(row, "\n") {
		t.Errorf("row must not contain a line break: %q", row)
	}
	if !strings.Contains(row, "fix the") || !strings.Contains(row, "auth bug") {
		t.Errorf("row must still show both halves of the topic around the separator: %q", row)
	}

	detail := m.detailText(RenderOptions{Width: m.width, Style: false})
	if !strings.Contains(detail, "fix the\nauth bug") {
		t.Errorf("detail pane must keep showing the topic's real line break, got %q", detail)
	}
}

func browseTestDBAt(t *testing.T, path string) *sqlitex.Runner {
	t.Helper()
	r := &sqlitex.Runner{DBPath: path}
	if _, err := schema.Open(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestRenderItemDetailShowsNameAndTopicSeparately covers change
// show-session-names: the row shows one of the two, but the detail pane has
// the space for both, and seeing both is how you tell what a session was
// renamed away from.
func TestRenderItemDetailShowsNameAndTopicSeparately(t *testing.T) {
	db := browseTestDB(t)
	it := search.Item{
		SessionID: "claude:p:1", Source: "claude", LineageID: "l1", Handle: 3,
		CWD: strp("/work/repo"), EndState: "completed",
		Name: strp("retry-loop"), Topic: strp("Flaky retry loop investigation"),
	}
	out := renderItemDetail(db, it, RenderOptions{Width: 80, Style: false})
	if !strings.Contains(out, "name:   retry-loop") {
		t.Errorf("detail pane should show the session name:\n%s", out)
	}
	if !strings.Contains(out, "topic:  Flaky retry loop investigation") {
		t.Errorf("detail pane should still show the topic:\n%s", out)
	}

	unnamed := it
	unnamed.Name = nil
	out = renderItemDetail(db, unnamed, RenderOptions{Width: 80, Style: false})
	if strings.Contains(out, "name:") {
		t.Errorf("an unnamed session should have no name line:\n%s", out)
	}
}
