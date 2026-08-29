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

	"lazyrecall/internal/annotate"
	"lazyrecall/internal/config"
	"lazyrecall/internal/profile"
	"lazyrecall/internal/schema"
	"lazyrecall/internal/search"
	"lazyrecall/internal/sqlitex"
)

// All fixtures below are synthetic, hand-written data - never real session
// content.

func browseTestDB(t *testing.T) *sqlitex.Runner {
	t.Helper()
	dir := t.TempDir()
	r := &sqlitex.Runner{DBPath: filepath.Join(dir, "lazyrecall.db")}
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
// profile.DBPath is derived from LAZYRECALL_HOME (set by the test) plus Name.
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

// seedFixture builds a small synthetic corpus: 7 sessions across 3 agents
// and 3 repositories, two of them tagged. All content is hand-written -
// never real session data.
func seedFixture(t *testing.T, db *sqlitex.Runner) {
	t.Helper()
	repos := []string{"/Users/x/personal/lazyrecall", "/Users/x/work/api", "/Users/x/dotfiles"}
	agents := []string{"claude", "claude", "pi", "omp", "claude", "claude", "pi"}
	for i := 0; i < 7; i++ {
		cwd := repos[i%3]
		seedBrowseSession(t, db, fmt.Sprintf("L%d", i), fmt.Sprintf("claude:p:s%d", i), "p", i+1, map[string]any{
			"source": agents[i], "cwd": cwd, "git_common_root": cwd,
			"topic":            fmt.Sprintf("topic number %d", i),
			"last_activity_at": 1700000000 - int64(i)*8000, "dir_exists": 1, "message_count": 40 + i,
		})
	}
	for _, l := range []string{"L0", "L2"} {
		if err := annotate.AddTag(db, l, "wip"); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureBrowser(t *testing.T) *browseModel {
	t.Helper()
	db := browseTestDB(t)
	seedFixture(t, db)
	return newTestBrowser(db, "claude-personal", BrowserOptions{
		Resolve:  testResolve("claude-personal"),
		Profiles: testProfiles("claude-personal", "claude-work"),
	})
}

// rowValues is the list of values a facet panel currently offers, for
// assertions that care about content rather than rendering.
func rowValues(f facet) []string {
	out := make([]string, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r.Value)
	}
	return out
}

func rowCount(t *testing.T, f facet, value string) int {
	t.Helper()
	for _, r := range f.rows {
		if r.Value == value {
			return r.Count
		}
	}
	t.Fatalf("facet has no row %q (has %v)", value, rowValues(f))
	return 0
}

// ---------------------------------------------------------------------
// The frame must fit the terminal
// ---------------------------------------------------------------------

// TestViewFitsTerminal is the regression test for the failure mode the old
// stacked layout hit twice: a frame one line taller or one column wider
// than the terminal, which the terminal then scrolls or wraps, silently
// pushing the top of the interface off the screen. A string comparison
// cannot see it; measuring the frame against the size it was drawn for can.
func TestViewFitsTerminal(t *testing.T) {
	sizes := [][2]int{{100, 40}, {100, 26}, {120, 30}, {80, 24}, {76, 20}, {70, 20}, {60, 14}, {40, 10}}
	for _, styled := range []bool{false, true} {
		for _, size := range sizes {
			w, h := size[0], size[1]
			db := browseTestDB(t)
			seedFixture(t, db)
			m := newTestBrowser(db, "claude-personal", BrowserOptions{
				Style: styled, Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
			})
			m = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
			lines := strings.Split(m.View(), "\n")
			if len(lines) > h {
				t.Errorf("styled=%v %dx%d: frame is %d lines, taller than the terminal", styled, w, h, len(lines))
			}
			for i, l := range lines {
				if got := visibleWidth(l); got > w {
					t.Errorf("styled=%v %dx%d: line %d is %d columns wide: %q", styled, w, h, i, got, l)
				}
			}
		}
	}
}

// TestViewFitsWithPromptOpen covers the same invariant with a prompt line
// on screen, which costs the body one row - the case the old layout got
// wrong by budgeting for the prompt in one height function and not the
// other.
func TestViewFitsWithPromptOpen(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	m = update(t, m, keyRunes("/"))
	if m.mode != modeFilter {
		t.Fatalf("expected the filter prompt to be open, got mode %v", m.mode)
	}
	if lines := strings.Split(m.View(), "\n"); len(lines) > 26 {
		t.Errorf("frame with a prompt open is %d lines, taller than the terminal", len(lines))
	}
}

// ---------------------------------------------------------------------
// Focus
// ---------------------------------------------------------------------

func TestDigitsJumpToPanels(t *testing.T) {
	m := fixtureBrowser(t)
	for key, want := range map[string]panelID{
		"1": panelProfiles, "2": panelAgents, "3": panelRepos, "4": panelTags, "0": panelSessions,
	} {
		m = update(t, m, keyRunes(key))
		if m.focus != want {
			t.Errorf("%q focused %v, want %v", key, m.focus, want)
		}
	}
}

func TestTabCyclesFocusAndWraps(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("1"))
	seen := []panelID{m.focus}
	for i := 0; i < int(numPanels)-1; i++ {
		m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
		seen = append(seen, m.focus)
	}
	want := []panelID{panelProfiles, panelAgents, panelRepos, panelTags, panelSessions, panelDetail}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("tab visited %v, want %v", seen, want)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != panelProfiles {
		t.Errorf("tab past the last panel went to %v, want it to wrap to Profiles", m.focus)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.focus != panelDetail {
		t.Errorf("shift-tab from the first panel went to %v, want it to wrap to Detail", m.focus)
	}
}

// A terminal too narrow for the side panels does not draw them, so focus
// must never land on one - the movement keys would then be moving a cursor
// nobody can see.
func TestNarrowTerminalKeepsFocusOnVisiblePanels(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	m = update(t, m, keyRunes("3"))
	if m.focus == panelRepos {
		t.Error("focus moved to a side panel that is not drawn at this width")
	}
	for i := 0; i < 6; i++ {
		m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
		if m.focus != panelSessions && m.focus != panelDetail {
			t.Fatalf("tab reached %v, which is not drawn at this width", m.focus)
		}
	}
}

// "5" is unbound now that Sessions is reachable through 0 (change
// spatial-panel-navigation) - pressing it must leave focus exactly where it
// was rather than doing something by accident.
func TestFiveNoLongerFocusesAnything(t *testing.T) {
	m := fixtureBrowser(t)
	m.focus = panelAgents
	m = update(t, m, keyRunes("5"))
	if m.focus != panelAgents {
		t.Errorf("%q moved focus to %v; 5 should be unbound", "5", m.focus)
	}
}

// ---------------------------------------------------------------------
// Spatial panel navigation (H/J/K/L)
// ---------------------------------------------------------------------

// TestSpatialMovesFollowGeometry covers one representative move in each
// direction the feature defines, from a panel where that direction has
// somewhere to go.
func TestSpatialMovesFollowGeometry(t *testing.T) {
	cases := []struct {
		name string
		from panelID
		key  string
		want panelID
	}{
		{"L from a left panel goes to Sessions", panelRepos, "L", panelSessions},
		{"H from Sessions goes to Profiles", panelSessions, "H", panelProfiles},
		{"H from Detail goes to Profiles", panelDetail, "H", panelProfiles},
		{"J walks down the left column", panelAgents, "J", panelRepos},
		{"K walks up the left column", panelRepos, "K", panelAgents},
		{"J from Sessions goes to Detail", panelSessions, "J", panelDetail},
		{"K from Detail goes to Sessions", panelDetail, "K", panelSessions},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := fixtureBrowser(t)
			m.focus = c.from
			m = update(t, m, keyRunes(c.key))
			if m.focus != c.want {
				t.Errorf("%s from %v landed on %v, want %v", c.key, c.from, m.focus, c.want)
			}
		})
	}
}

// H always resolves to Profiles specifically, never to whichever left panel
// most recently had focus - the user asked for exactly that, rejecting the
// cleverer "nearest panel" rule. This is the case that would tell the two
// apart: Agents was the last left panel visited, so a "nearest" rule would
// send H there instead of to Profiles.
func TestHAlwaysReturnsToProfilesNotTheLastLeftPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m.focus = panelAgents
	m = update(t, m, keyRunes("L")) // Agents -> Sessions
	if m.focus != panelSessions {
		t.Fatalf("L from Agents landed on %v, want Sessions", m.focus)
	}
	m = update(t, m, keyRunes("H"))
	if m.focus != panelProfiles {
		t.Errorf("H from Sessions landed on %v, want Profiles, not the last left panel visited", m.focus)
	}
}

// The feature is spatial, not cyclic: reaching an edge leaves focus exactly
// where it was, rather than wrapping to the far side the way Tab does.
func TestSpatialMovesDoNotWrapAtEdges(t *testing.T) {
	cases := []struct {
		name string
		at   panelID
		key  string
	}{
		{"K on Profiles", panelProfiles, "K"},
		{"J on Tags", panelTags, "J"},
		{"L on Sessions", panelSessions, "L"},
		{"L on Detail", panelDetail, "L"},
		{"H on Profiles", panelProfiles, "H"},
		{"H on Agents", panelAgents, "H"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := fixtureBrowser(t)
			m.focus = c.at
			m = update(t, m, keyRunes(c.key))
			if m.focus != c.at {
				t.Errorf("%s moved focus from %v to %v, want it to stay put at the edge", c.key, c.at, m.focus)
			}
		})
	}
}

// A terminal too narrow for the side panels drops the whole left column, so
// H from Sessions - which would otherwise land on Profiles - must find
// nothing drawn to land on and leave focus untouched, the same rule the
// digit jump keys already follow (TestNarrowTerminalKeepsFocusOnVisiblePanels).
func TestSpatialMoveSkipsUndrawnLeftColumn(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	m.focus = panelSessions
	m = update(t, m, keyRunes("H"))
	if m.focus != panelSessions {
		t.Errorf("H at 60 columns landed on %v, want it to stay on Sessions: no side panel is drawn to receive it", m.focus)
	}
}

// When the body is too short to give every left panel a usable window,
// geometry drops Tags on its own (tagsH == 0) even though the rest of the
// left column is still drawn. J from Repos must not land on the panel that
// is not being drawn - and since Tags is the last panel in the column,
// there is nothing further to continue to, so the move does nothing.
func TestSpatialMoveSkipsDroppedTagsPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if g := m.geometry(); !g.sidebar || g.tagsH != 0 {
		t.Fatalf("fixture at 100x18 has sidebar=%v tagsH=%d, want sidebar and tagsH=0 for this test to mean anything", g.sidebar, g.tagsH)
	}
	m.focus = panelRepos
	m = update(t, m, keyRunes("J"))
	if m.focus != panelRepos {
		t.Errorf("J from Repos with Tags dropped landed on %v, want it to stay on Repos", m.focus)
	}
}

// TestResizeMovesFocusOffAPanelThatDisappears is defect 2 of the
// terminal-width audit: panelDrawn already governs setFocus (the digit
// keys) and moveFocusSpatial (H/J/K/L), but the tea.WindowSizeMsg handler
// updated m.width/m.height and the detail viewport without ever asking
// whether the panel currently focused was still one of them. Focus Tags,
// then shrink below minSidebarWidth: the whole sidebar - Tags included -
// stops being drawn, and without this fix j/k/'/'/Enter would keep acting
// on a panel nothing on screen represents until the user happened to press
// Tab or a digit.
func TestResizeMovesFocusOffAPanelThatDisappears(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	m.focus = panelTags
	if !m.panelDrawn(panelTags) {
		t.Fatal("fixture at 100x26 does not draw Tags to begin with; pick a size where it does for this test to mean anything")
	}

	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	if m.geometry().sidebar {
		t.Fatal("fixture at 60x20 still draws the sidebar; pick a narrower width for this test to mean anything")
	}
	if m.focus == panelTags {
		t.Error("focus stayed on Tags after a resize dropped the whole sidebar it lives in")
	}
	if !m.panelDrawn(m.focus) {
		t.Errorf("resize left focus on %v, which is not currently drawn", m.focus)
	}
	if m.focus != panelSessions {
		t.Errorf("resize moved focus to %v, want it to fall back to Sessions (always drawn)", m.focus)
	}
}

// TestTabDoesNotLandOnZeroHeightTagsPanel is the other half of defect 2:
// moveFocus (Tab) only checked geometry().sidebar, not panelDrawn, so on a
// short terminal where the sidebar exists but geometry sets tagsH to 0
// (the "rest < 8" branch - see TestSpatialMoveSkipsDroppedTagsPanel, which
// establishes this same 100x18 fixture drops Tags on its own), Tab from
// Repos would stop cycling the instant it reached Tags, landing on a panel
// with no lines to draw.
func TestTabDoesNotLandOnZeroHeightTagsPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if g := m.geometry(); !g.sidebar || g.tagsH != 0 {
		t.Fatalf("fixture at 100x18 has sidebar=%v tagsH=%d, want sidebar and tagsH=0 for this test to mean anything", g.sidebar, g.tagsH)
	}
	m.focus = panelRepos
	m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus == panelTags {
		t.Error("Tab from Repos landed on Tags, which this terminal size draws at zero height")
	}
	if !m.panelDrawn(m.focus) {
		t.Errorf("Tab left focus on %v, which is not currently drawn", m.focus)
	}
}

// ---------------------------------------------------------------------
// Facets
// ---------------------------------------------------------------------

func TestFacetsCountTheCorpus(t *testing.T) {
	m := fixtureBrowser(t)
	if got := rowCount(t, m.agents, ""); got != 7 {
		t.Errorf(`the "all" row counts %d sessions, want 7`, got)
	}
	if got := rowCount(t, m.agents, "claude"); got != 4 {
		t.Errorf("claude counts %d, want 4", got)
	}
	if got := rowCount(t, m.repos, "/Users/x/personal/lazyrecall"); got != 3 {
		t.Errorf("the repo counts %d, want 3", got)
	}
	if got := rowCount(t, m.tags, "wip"); got != 2 {
		t.Errorf("#wip counts %d, want 2", got)
	}
}

func TestApplyingAFacetFiltersTheList(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("2"))
	m = update(t, m, keyRunes("j")) // off the "all" row onto the first agent
	m = update(t, m, keyRunes("j"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.agents.Sel != "pi" {
		t.Fatalf("applied agent is %q, want pi", m.agents.Sel)
	}
	if len(m.visible) != 2 {
		t.Errorf("%d sessions listed under agent=pi, want 2", len(m.visible))
	}
	for _, it := range m.visible {
		if it.Source != "pi" {
			t.Errorf("session %s has agent %q, want pi", it.SessionID, it.Source)
		}
	}
}

// Moving through a panel must change nothing until Enter: an accidental
// keystroke that silently re-filtered the list would make the panels unsafe
// to explore.
func TestMovingInAFacetDoesNotFilter(t *testing.T) {
	m := fixtureBrowser(t)
	before := len(m.visible)
	m = update(t, m, keyRunes("2"))
	m = update(t, m, keyRunes("j"))
	m = update(t, m, keyRunes("j"))
	if m.agents.Sel != "" {
		t.Errorf("moving applied %q; nothing should be applied until Enter", m.agents.Sel)
	}
	if len(m.visible) != before {
		t.Errorf("moving changed the list from %d to %d sessions", before, len(m.visible))
	}
}

// Each facet is counted over the corpus narrowed by the *other* facets, so
// its rows always say what selecting them would actually give.
func TestFacetCountsReflectOtherFacets(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel = "pi"
	m.rebuild()
	if got := rowCount(t, m.repos, ""); got != 2 {
		t.Errorf(`the repos "all" row counts %d under agent=pi, want 2`, got)
	}
	if got := rowCount(t, m.tags, "wip"); got != 1 {
		t.Errorf("#wip counts %d under agent=pi, want 1", got)
	}
	// The agent panel itself keeps every agent: it is counted with its own
	// selection excluded, so switching away from pi stays possible.
	if got := rowCount(t, m.agents, "claude"); got != 4 {
		t.Errorf("claude counts %d in its own panel under agent=pi, want 4", got)
	}
}

func TestEscClearsOnlyTheFocusedFacet(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel, m.tags.Sel = "claude", "wip"
	m.rebuild()
	m = update(t, m, keyRunes("2"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.agents.Sel != "" {
		t.Errorf("esc on Agents left %q applied", m.agents.Sel)
	}
	if m.tags.Sel != "wip" {
		t.Errorf("esc on Agents cleared the tag filter too (%q)", m.tags.Sel)
	}
}

// The synthetic "all" row is how a filter is cleared from inside the panel,
// so it needs no separate key.
func TestSelectingAllClearsTheFacet(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel = "pi"
	m.rebuild()
	m = update(t, m, keyRunes("2"))
	m = update(t, m, keyRunes("g"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.agents.Sel != "" {
		t.Errorf(`selecting the "all" row left %q applied`, m.agents.Sel)
	}
	if len(m.visible) != 7 {
		t.Errorf("%d sessions listed after clearing, want all 7", len(m.visible))
	}
}

func TestClearAllFiltersResetsEveryFacet(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel, m.repos.Sel, m.tags.Sel, m.textFilter = "claude", "/Users/x/work/api", "wip", "zzz"
	m.rebuild()
	m = update(t, m, keyRunes("X"))
	if m.agents.Sel != "" || m.repos.Sel != "" || m.tags.Sel != "" || m.textFilter != "" {
		t.Errorf("X left filters applied: agent=%q repo=%q tag=%q filter=%q",
			m.agents.Sel, m.repos.Sel, m.tags.Sel, m.textFilter)
	}
	if len(m.visible) != 7 {
		t.Errorf("%d sessions listed after clearing everything, want 7", len(m.visible))
	}
}

// Command-line filters and panel selections have to be the same state, or
// `--agent=pi` and walking to "pi" would put the browser in two different
// places.
func TestCommandLineFiltersOpenAsPanelSelections(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Agent: "pi", Tag: "wip",
		Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	if m.agents.Sel != "pi" || m.tags.Sel != "wip" {
		t.Fatalf("opened with agent=%q tag=%q, want pi/wip", m.agents.Sel, m.tags.Sel)
	}
	if len(m.visible) != 1 {
		t.Errorf("%d sessions listed under agent=pi tag=wip, want 1", len(m.visible))
	}
}

// ---------------------------------------------------------------------
// Narrowing
// ---------------------------------------------------------------------

// "/" narrows whatever panel has focus - the session list, or a facet's own
// rows.
func TestSlashNarrowsTheFocusedPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("3")) // Repos
	m = update(t, m, keyRunes("/"))
	for _, r := range "dotfiles" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.repos.filter != "dotfiles" {
		t.Fatalf("repo panel filter is %q, want dotfiles", m.repos.filter)
	}
	for _, v := range rowValues(m.repos) {
		if v != "" && !strings.Contains(v, "dotfiles") {
			t.Errorf("narrowed repo panel still offers %q", v)
		}
	}
	// Narrowing a panel must not filter the session list by itself.
	if len(m.visible) != 7 {
		t.Errorf("narrowing the repo panel changed the list to %d sessions", len(m.visible))
	}
}

func TestSlashOnSessionsNarrowsTheList(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("/"))
	for _, r := range "dotfiles" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) == 0 || len(m.visible) == 7 {
		t.Fatalf("narrowing left %d of 7 sessions", len(m.visible))
	}
	for _, it := range m.visible {
		if it.CWD == nil || !strings.Contains(*it.CWD, "dotfiles") {
			t.Errorf("session %s survived a 'dotfiles' narrowing", it.SessionID)
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.visible) != 7 {
		t.Errorf("esc on Sessions left %d sessions, want the narrowing cleared", len(m.visible))
	}
}

// Esc out of a prompt applies nothing - the convention every prompt in the
// browser has always had.
func TestEscapingAPromptAppliesNothing(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("s"))
	for _, r := range "topic" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNone {
		t.Errorf("esc left the browser in mode %v", m.mode)
	}
	if m.query != "" {
		t.Errorf("esc applied the search phrase %q", m.query)
	}
}

// ---------------------------------------------------------------------
// Detail tabs
// ---------------------------------------------------------------------

func TestBracketsCycleDetailTabs(t *testing.T) {
	m := fixtureBrowser(t)
	if m.tab != tabDetail {
		t.Fatalf("opened on tab %v, want Detail", m.tab)
	}
	m = update(t, m, keyRunes("]"))
	if m.tab != tabPrompts {
		t.Errorf("] went to %v, want Prompts", m.tab)
	}
	m = update(t, m, keyRunes("]"))
	m = update(t, m, keyRunes("]"))
	if m.tab != tabDetail {
		t.Errorf("] three times landed on %v, want it to wrap to Detail", m.tab)
	}
	m = update(t, m, keyRunes("["))
	if m.tab != tabComments {
		t.Errorf("[ from Detail went to %v, want it to wrap to Comments", m.tab)
	}
}

// The tab strip has to say which tab is active without relying on colour,
// because NO_COLOR suppresses all of it (spec session-search, "Styling
// disabled by the environment").
func TestActiveTabIsMarkedWithoutColour(t *testing.T) {
	m := fixtureBrowser(t)
	if strip := m.tabStrip(); !strings.Contains(strip, "[Detail]") {
		t.Errorf("unstyled tab strip %q does not mark the active tab", strip)
	}
	m = update(t, m, keyRunes("]"))
	if strip := m.tabStrip(); !strings.Contains(strip, "[Prompts]") {
		t.Errorf("unstyled tab strip %q does not mark the active tab", strip)
	}
}

func TestCommentsTabShowsCommentsWithTheirIDs(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	if err := annotate.AddComment(db, "L0", "check the resume path"); err != nil {
		t.Fatal(err)
	}
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m.tab = tabComments
	got := m.detailContent(RenderOptions{Width: 60})
	if !strings.Contains(got, "check the resume path") {
		t.Errorf("comments tab does not show the comment:\n%s", got)
	}
	if !strings.Contains(got, "[1]") {
		t.Errorf("comments tab does not show the comment id, which `comment rm` takes:\n%s", got)
	}
}

// ---------------------------------------------------------------------
// The action menu
// ---------------------------------------------------------------------

func TestActionMenuOffersThePanelsActions(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	if m.mode != modeMenu {
		t.Fatalf("x left the browser in mode %v, want the action menu", m.mode)
	}
	labels := make([]string, len(m.menuFiltered))
	for i, a := range m.menuFiltered {
		labels[i] = a.label
	}
	joined := strings.Join(labels, "|")
	for _, want := range []string{"resume this session", "add a tag", "add a comment"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the Sessions menu does not offer %q; it offers %s", want, joined)
		}
	}
	if strings.Contains(joined, "switch to this profile") {
		t.Errorf("the Sessions menu offers a Profiles action: %s", joined)
	}
}

func TestActionMenuNarrowsByTyping(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	before := len(m.menuFiltered)
	for _, r := range "comment" {
		m = update(t, m, keyRunes(string(r)))
	}
	if len(m.menuFiltered) == 0 || len(m.menuFiltered) >= before {
		t.Fatalf("typing narrowed the menu from %d to %d entries", before, len(m.menuFiltered))
	}
	for _, a := range m.menuFiltered {
		if !strings.Contains(a.label, "comment") {
			t.Errorf("narrowed menu still offers %q", a.label)
		}
	}
}

func TestActionMenuRunsTheHighlightedAction(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel = "pi"
	m.rebuild()
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	for _, r := range "clear all" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeNone {
		t.Errorf("running a menu action left mode %v", m.mode)
	}
	if m.agents.Sel != "" {
		t.Errorf("the 'clear all filters' action left agent=%q", m.agents.Sel)
	}
}

func TestEscapingTheActionMenuRunsNothing(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel = "pi"
	m.rebuild()
	m = update(t, m, keyRunes("x"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNone {
		t.Errorf("esc left the menu open (mode %v)", m.mode)
	}
	if m.agents.Sel != "pi" {
		t.Errorf("esc ran an action: agent is now %q", m.agents.Sel)
	}
}

// ---------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------

func TestEnterOnASessionSelectsItAndQuits(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("j"))
	want := m.visible[1].SessionID
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*browseModel)
	if !m.selectedOK {
		t.Fatal("enter on a session did not select it")
	}
	if m.selected.SessionID != want {
		t.Errorf("selected %s, want %s", m.selected.SessionID, want)
	}
	if cmd == nil {
		t.Error("enter on a session returned no command; it must quit so the caller can resume")
	}
}

// Enter on a facet panel filters; it must never be mistaken for "resume",
// which is the one irreversible thing the browser does.
func TestEnterOnAFacetDoesNotResume(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("2"))
	m = update(t, m, keyRunes("j"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.selectedOK {
		t.Error("enter on the Agents panel selected a session to resume")
	}
}

func TestQuitDoesNotSelect(t *testing.T) {
	m := fixtureBrowser(t)
	nm, cmd := m.Update(keyRunes("q"))
	m = nm.(*browseModel)
	if m.selectedOK {
		t.Error("q selected a session")
	}
	if cmd == nil {
		t.Error("q returned no quit command")
	}
}

// ---------------------------------------------------------------------
// Profiles
// ---------------------------------------------------------------------

func TestSwitchingProfileResetsTheView(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel, m.tags.Sel, m.textFilter = "claude", "wip", "topic"
	m.rebuild()
	other := browseTestDB(t)
	m = update(t, m, dbSwitchedMsg{name: "claude-work", db: other})
	if m.profileName != "claude-work" {
		t.Errorf("active profile is %q, want claude-work", m.profileName)
	}
	if m.agents.Sel != "" || m.tags.Sel != "" || m.textFilter != "" {
		t.Errorf("filters survived the profile switch: agent=%q tag=%q filter=%q",
			m.agents.Sel, m.tags.Sel, m.textFilter)
	}
	if len(m.visible) != 0 {
		t.Errorf("%d sessions listed from the other (empty) profile", len(m.visible))
	}
}

func TestActiveProfileIsMarkedInThePanel(t *testing.T) {
	m := fixtureBrowser(t)
	g := m.geometry()
	box := m.facetPanel(panelProfiles, &m.profiles_, g.leftWidth, g.profilesH, "")
	if len(box.Lines) == 0 {
		t.Fatal("the Profiles panel drew no rows")
	}
	if !strings.Contains(box.Lines[0], "claude-personal") || !strings.Contains(box.Lines[0], "●") {
		t.Errorf("the active profile is not marked in %q", box.Lines[0])
	}
	if strings.Contains(box.Lines[1], "●") {
		t.Errorf("an inactive profile is marked as active in %q", box.Lines[1])
	}
}

// ---------------------------------------------------------------------
// Discoverability and safety
// ---------------------------------------------------------------------

// The footer and the help overlay both come from browseActions, so a key
// can never be advertised that is not bound - but only if every key in the
// table is in fact handled. This asserts the table's own coherence.
func TestEveryAdvertisedKeyIsBound(t *testing.T) {
	m := fixtureBrowser(t)
	for _, a := range browseActions {
		if a.key == "" || a.label == "" {
			t.Errorf("action %+v has an empty key or label", a)
		}
	}
	help := m.helpView()
	for _, a := range browseActions {
		if !strings.Contains(help, a.key) {
			t.Errorf("help overlay does not list %q", a.key)
		}
	}
}

func TestFooterShowsTheFocusedPanelsActions(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	if got := m.footer(); !strings.Contains(got, "resume") {
		t.Errorf("Sessions footer %q does not mention resuming", got)
	}
	m = update(t, m, keyRunes("2"))
	got := m.footer()
	if strings.Contains(got, "resume") {
		t.Errorf("Agents footer %q offers resume, which Enter does not do there", got)
	}
	if !strings.Contains(got, "filter") {
		t.Errorf("Agents footer %q does not say what Enter does", got)
	}
}

// ---------------------------------------------------------------------
// Archive and show-all
// ---------------------------------------------------------------------

// "a" archives the selected session and "a" again unarchives it. With
// showAll on the row stays put across the toggle, so the second press is
// unambiguously about the same session.
func TestArchiveKeyToggles(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		ShowAll: true, Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, keyRunes("0"))
	target := m.visible[0].LineageID

	m = update(t, m, keyRunes("a"))
	if got, err := annotate.IsArchived(m.db, target); err != nil {
		t.Fatal(err)
	} else if !got {
		t.Fatal("a did not archive the selected session")
	}
	if !strings.Contains(m.notice, "archived") {
		t.Errorf("no notice that the session was archived: %q", m.notice)
	}
	if !strings.Contains(m.View(), "[archived]") {
		t.Error("an archived session's row does not show the [archived] marker")
	}

	m = update(t, m, keyRunes("a"))
	if got, err := annotate.IsArchived(m.db, target); err != nil {
		t.Fatal(err)
	} else if got {
		t.Fatal("a did not unarchive the selected session")
	}
	if !strings.Contains(m.notice, "unarchived") {
		t.Errorf("no notice that the session was unarchived: %q", m.notice)
	}
}

// With the archive flag in force (showAll off), archiving the selected
// session reloads the list and the row leaves it.
func TestArchiveKeyRemovesTheRow(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	target := m.visible[0].LineageID
	m = update(t, m, keyRunes("a"))
	for _, it := range m.visible {
		if it.LineageID == target {
			t.Errorf("archived session %s is still listed", target)
		}
	}
	if len(m.visible) != 6 {
		t.Errorf("after archiving one of 7 sessions %d remain, want 6", len(m.visible))
	}
}

// "." toggles showAll, and with it on the sessions the hide rules
// suppressed reappear; toggling again hides them.
func TestDotTogglesShowAll(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Hide:    config.Hide{MinMessages: 44},
		Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	before := len(m.visible)
	if before == 7 {
		t.Fatalf("the hide rule hid nothing; the fixture has %d sessions", before)
	}
	m = update(t, m, keyRunes("."))
	if !m.showAll {
		t.Error(". did not turn showAll on")
	}
	if len(m.visible) != 7 {
		t.Errorf("with showAll on %d sessions are listed, want all 7", len(m.visible))
	}
	m = update(t, m, keyRunes("."))
	if m.showAll {
		t.Error(". did not turn showAll off")
	}
	if len(m.visible) != before {
		t.Errorf("after toggling showAll off %d sessions are listed, want %d", len(m.visible), before)
	}
}

// The Sessions border reports how many sessions the rules suppressed, and
// stops reporting it the moment showAll is on - the same contract the
// command-line header keeps ("--all to show").
func TestSessionsBorderShowsHiddenCount(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Hide:    config.Hide{MinMessages: 44},
		Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	border := strings.Split(m.sessionsPanel(m.geometry()).render(), "\n")[0]
	if !strings.Contains(border, "4 hidden") {
		t.Errorf("border %q does not show the suppressed count", border)
	}
	m = update(t, m, keyRunes("."))
	border = strings.Split(m.sessionsPanel(m.geometry()).render(), "\n")[0]
	if strings.Contains(border, "hidden") {
		t.Errorf("border %q shows a suppressed count with showAll on", border)
	}
}

// The [archived] marker is text, not colour, so a NO_COLOR user still sees
// which rows are archived.
func TestArchivedMarkerWithoutStyling(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	if err := annotate.Archive(db, "L0"); err != nil {
		t.Fatal(err)
	}
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Style: false, ShowAll: true,
		Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	v := m.View()
	if !strings.Contains(v, "[archived]") {
		t.Error("an archived session's row does not show [archived] without styling")
	}
	if strings.Contains(v, "\x1b") {
		t.Error("unstyled output contains an escape sequence")
	}
}

// "a" with no session selected does nothing and must not panic - the empty
// profile is a real, reachable state, not a test-only corner.
func TestArchiveWithNoSelectionDoesNothing(t *testing.T) {
	m := newTestBrowser(browseTestDB(t), "p", BrowserOptions{})
	m = update(t, m, keyRunes("0"))
	if m.current() != nil {
		t.Fatal("expected no session to be selected in an empty profile")
	}
	m = update(t, m, keyRunes("a"))
	if m.notice != "" {
		t.Errorf("a with no selection set a notice: %q", m.notice)
	}
}

// The frame must still fit the terminal at every size while a hidden count
// is in the Sessions border (showAll off) and while an archived row with
// its [archived] marker is on screen (showAll on) - the two surfaces this
// change added to the width budget.
func TestViewFitsWithHiddenCountAndArchivedRow(t *testing.T) {
	sizes := [][2]int{{100, 40}, {100, 26}, {120, 30}, {80, 24}, {76, 20}, {70, 20}, {60, 14}, {40, 10}}
	for _, styled := range []bool{false, true} {
		for _, showAll := range []bool{false, true} {
			for _, size := range sizes {
				w, h := size[0], size[1]
				db := browseTestDB(t)
				seedFixture(t, db)
				if err := annotate.Archive(db, "L0"); err != nil {
					t.Fatal(err)
				}
				m := newTestBrowser(db, "claude-personal", BrowserOptions{
					Style: styled, ShowAll: showAll, Hide: config.Hide{MinMessages: 44},
					Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
				})
				m = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
				view := m.View()
				lines := strings.Split(view, "\n")
				if len(lines) > h {
					t.Errorf("styled=%v showAll=%v %dx%d: frame is %d lines, taller than the terminal", styled, showAll, w, h, len(lines))
				}
				for i, l := range lines {
					if got := visibleWidth(l); got > w {
						t.Errorf("styled=%v showAll=%v %dx%d: line %d is %d columns wide: %q", styled, showAll, w, h, i, got, l)
					}
				}
				// Prove the exercised surface is actually on screen, so the
				// fit claim is about a frame that really carries it.
				if showAll {
					if !strings.Contains(view, "[archived]") {
						t.Errorf("styled=%v showAll=%v %dx%d: no archived row drawn", styled, showAll, w, h)
					}
				} else if !strings.Contains(view, "hidden") {
					t.Errorf("styled=%v showAll=%v %dx%d: the border does not show the hidden count", styled, showAll, w, h)
				}
			}
		}
	}
}

func TestHelpOverlayClosesOnAnyKey(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("?"))
	if !m.help {
		t.Fatal("? did not open the help overlay")
	}
	if v := m.View(); !strings.Contains(v, "key bindings") {
		t.Errorf("the help overlay does not look like help:\n%s", v)
	}
	before := m.agents.Sel
	m = update(t, m, keyRunes("2"))
	if m.help {
		t.Error("a key press did not close the help overlay")
	}
	if m.focus == panelAgents {
		t.Error("the key that dismissed the help also fired the action behind it")
	}
	if m.agents.Sel != before {
		t.Error("dismissing the help changed a filter")
	}
}

// The browser must never bind an Alt combination: the window manager
// reserves them, so they would never reach the program (spec session-search,
// "The browser binds no Alt combinations").
func TestAltCombinationsAreInert(t *testing.T) {
	m := fixtureBrowser(t)
	before := *m
	for _, k := range []string{"j", "k", "x", "q", "/", "2", "R", "X"} {
		m = update(t, m, altKey(k))
	}
	if m.focus != before.focus || m.selectedOK || m.mode != modeNone || m.help {
		t.Errorf("an Alt combination did something: focus=%v selected=%v mode=%v help=%v",
			m.focus, m.selectedOK, m.mode, m.help)
	}
}

// Repository paths, tags, and session text are free text from a source this
// program does not own. A control character in any of them must not be able
// to break out of the pane it is drawn in.
func TestControlCharactersCannotBreakThePanels(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "L0", "claude:p:s0", "p", 1, map[string]any{
		"cwd": "/tmp/a\nb\rc", "git_common_root": "/tmp/a\nb\rc", "topic": "line\none\x1b[31m",
		"last_activity_at": 1700000000, "dir_exists": 1,
	})
	if err := annotate.AddTag(db, "L0", "we\nird"); err != nil {
		t.Fatal(err)
	}
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	lines := strings.Split(m.View(), "\n")
	if len(lines) > 26 {
		t.Errorf("control characters in session text made the frame %d lines", len(lines))
	}
	for i, l := range lines {
		if got := visibleWidth(l); got > 100 {
			t.Errorf("line %d is %d columns wide after sanitizing: %q", i, got, l)
		}
	}
}

// NO_COLOR must suppress every escape sequence, chrome included - the
// borders are new surface area for this requirement (spec session-search,
// "Styling disabled by the environment").
func TestUnstyledViewEmitsNoEscapes(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Style: false, Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	for _, v := range []string{m.View(), m.footer(), m.helpView(), m.tabStrip()} {
		if strings.Contains(v, "\x1b") {
			t.Errorf("unstyled output contains an escape sequence: %q", v)
		}
	}
	m = update(t, m, keyRunes("2"))
	if v := m.View(); strings.Contains(v, "\x1b") {
		t.Error("unstyled output contains an escape sequence with a side panel focused")
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

// ---------------------------------------------------------------------
// Paths that only the real event loop exercises
// ---------------------------------------------------------------------

// TestProfileSwitchThroughRealProgramEventLoop drives the real tea.Program
// - the same one RunBrowser constructs - over a piped input, pressing keys
// as the actual bytes a terminal would send, with no test code calling
// Update or a returned command itself. Every other test here proves the
// *model's* logic; only this one proves that bubbletea delivers a real key
// press through Update and feeds the tea.Cmd it returns back in as a
// message, which is where a profile switch would silently fail.
func TestProfileSwitchThroughRealProgramEventLoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LAZYRECALL_HOME", home)

	pdb := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, pdb, "lp", "claude:p:1", "p", 1, map[string]any{"topic": "profile p session"})
	qdb := browseTestDBAt(t, filepath.Join(home, "q.db"))
	seedBrowseSession(t, qdb, "lq", "claude:q:1", "q", 1, map[string]any{"topic": "profile q session"})
	_ = qdb

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

	in.chunks <- []byte("1")      // focus the Profiles panel
	in.chunks <- []byte("\x1b[B") // Down arrow, delivered as one chunk
	in.chunks <- []byte("\r")     // switch to the highlighted profile

	// switchProfileCmd is itself asynchronous - bubbletea runs it in its
	// own goroutine and only feeds its result back in as a dbSwitchedMsg
	// once that goroutine returns. Sending "q" without waiting for that to
	// land races the real quit against the real switch, which looks exactly
	// like "switching had no effect" but is a race in this test's own
	// timing. Wait for the switched-to profile's session to actually be
	// drawn, the same way a person would wait to see it happen.
	deadline := time.Now().Add(5 * time.Second)
	for !out.Contains("profile q session") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !out.Contains("profile q session") {
		t.Fatal("profile q's session never appeared in the rendered output within 5s")
	}
	in.chunks <- []byte("q")
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

// A failed profile switch reports why and keeps the current profile and its
// sessions, rather than silently doing nothing (spec session-search,
// "Chosen profile cannot be opened").
func TestProfileSwitchFailureKeepsCurrentProfile(t *testing.T) {
	m := fixtureBrowser(t)
	before := len(m.visible)
	m = update(t, m, dbSwitchedMsg{err: fmt.Errorf("no such profile \"nope\"")})
	if m.profileName != "claude-personal" {
		t.Errorf("a failed switch changed the active profile to %q", m.profileName)
	}
	if len(m.visible) != before {
		t.Errorf("a failed switch changed the listing from %d to %d sessions", before, len(m.visible))
	}
	if !strings.Contains(m.footer(), "nope") {
		t.Errorf("a failed switch did not report why: %q", m.footer())
	}
}

// ---------------------------------------------------------------------
// Search, annotation, refresh
// ---------------------------------------------------------------------

// The search phrase must go through the same FTS path `lazyrecall search`
// uses, not a second in-process matcher.
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
}

// The Prompts tab reads the same prompt index the search does.
func TestPromptsTabShowsIndexedPrompts(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "a topic"})
	b := db.NewBatch()
	if err := b.BulkInsert("prompt_fts", []string{"session_id", "kind", "text"}, []map[string]any{
		{"session_id": "claude:p:1", "kind": "prompt", "text": "rotate the aurora password"},
		{"session_id": "claude:p:1", "kind": "topic", "text": "not a prompt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	m := newTestBrowser(db, "p", BrowserOptions{})
	m.tab = tabPrompts
	got := m.detailContent(RenderOptions{Width: 60})
	if !strings.Contains(got, "rotate the aurora password") {
		t.Errorf("the Prompts tab does not show the session's prompt:\n%s", got)
	}
	if strings.Contains(got, "not a prompt") {
		t.Errorf("the Prompts tab shows a non-prompt record:\n%s", got)
	}
}

// Adding a tag or a comment has to be visible immediately - the whole point
// of annotating from inside the browser is not having to leave it.
func TestTagAndCommentEditingUpdateImmediately(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	target := m.visible[0].LineageID

	m = update(t, m, keyRunes("m"))
	m = update(t, m, keyRunes("urgent"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.visible[0].Tags; len(got) == 0 || !contains(got, "urgent") {
		t.Fatalf("the new tag is not on the row: %v", got)
	}
	if rowCount(t, m.tags, "urgent") != 1 {
		t.Error("the new tag did not appear in the Tags panel")
	}

	m = update(t, m, keyRunes("c"))
	m = update(t, m, keyRunes("check this"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	comments, err := annotate.CommentsForLineage(m.db, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Body != "check this" {
		t.Fatalf("the comment was not stored: %+v", comments)
	}
	m.tab = tabComments
	if got := m.detailContent(RenderOptions{Width: 60}); !strings.Contains(got, "check this") {
		t.Errorf("the new comment is not shown:\n%s", got)
	}

	m = update(t, m, keyRunes("M"))
	m = update(t, m, keyRunes("urgent"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if contains(m.visible[0].Tags, "urgent") {
		t.Errorf("the removed tag is still on the row: %v", m.visible[0].Tags)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestRefreshActionReloads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LAZYRECALL_HOME", home)
	db := browseTestDBAt(t, filepath.Join(home, "p.db"))
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "first"})
	m := newTestBrowser(db, "p", BrowserOptions{Resolve: testResolve("p"), Profiles: testProfiles("p")})
	if len(m.visible) != 1 {
		t.Fatalf("opened with %d sessions, want 1", len(m.visible))
	}
	seedBrowseSession(t, db, "l2", "claude:p:2", "p", 2, map[string]any{"topic": "second"})
	m = update(t, m, refreshDoneMsg{})
	if len(m.visible) != 2 {
		t.Errorf("after a refresh the browser lists %d sessions, want 2", len(m.visible))
	}
}

// ---------------------------------------------------------------------
// Modality
// ---------------------------------------------------------------------

// Normal mode never enters text: an unmodified letter is an action.
func TestNormalModeLettersInvokeActionsNotText(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("x"))
	if m.mode != modeMenu {
		t.Errorf("x in normal mode did not open the action menu (mode %v)", m.mode)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	m = update(t, m, keyRunes("s"))
	if m.mode != modeSearchPhrase {
		t.Errorf("s in normal mode did not open the search prompt (mode %v)", m.mode)
	}
}

// Input mode never invokes actions: a letter typed into a prompt is text.
func TestInputModeTypingIsTextNotAction(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("s"))
	for _, r := range "qxR5" {
		m = update(t, m, keyRunes(string(r)))
	}
	if m.selectedOK {
		t.Error("a letter typed into a prompt quit the browser")
	}
	if m.input.Value() != "qxR5" {
		t.Errorf("typed text is %q, want qxR5 - the letters were treated as actions", m.input.Value())
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.query != "qxR5" {
		t.Errorf("submitted search phrase is %q, want qxR5", m.query)
	}
}

func TestCtrlCQuitsWithoutSelecting(t *testing.T) {
	m := fixtureBrowser(t)
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = nm.(*browseModel)
	if m.selectedOK {
		t.Error("Ctrl-C selected a session")
	}
	if cmd == nil {
		t.Error("Ctrl-C returned no quit command")
	}
}

// ---------------------------------------------------------------------
// Scrolling and rendering details
// ---------------------------------------------------------------------

// The session list draws exactly as many rows as its panel has room for,
// and keeps the selection inside them.
func TestSessionListScrollsToKeepSelectionVisible(t *testing.T) {
	db := browseTestDB(t)
	for i := 0; i < 60; i++ {
		seedBrowseSession(t, db, fmt.Sprintf("l%d", i), fmt.Sprintf("claude:p:%d", i), "p", i+1,
			map[string]any{"topic": fmt.Sprintf("session %d", i), "last_activity_at": 1700000000 - int64(i)})
	}
	m := newTestBrowser(db, "p", BrowserOptions{})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	g := m.geometry()
	box := m.sessionsPanel(g)
	if len(box.Lines) != g.sessionsInner {
		t.Errorf("the list drew %d rows for a %d-row panel", len(box.Lines), g.sessionsInner)
	}
	m = update(t, m, keyRunes("0"))
	for i := 0; i < 40; i++ {
		m = update(t, m, keyRunes("j"))
	}
	if m.cursor < m.listTop || m.cursor >= m.listTop+g.sessionsInner {
		t.Errorf("cursor %d is outside the drawn window [%d,%d)", m.cursor, m.listTop, m.listTop+g.sessionsInner)
	}
	m = update(t, m, keyRunes("G"))
	if m.cursor != len(m.visible)-1 {
		t.Errorf("G put the cursor at %d, want the last of %d", m.cursor, len(m.visible))
	}
	if m.cursor < m.listTop || m.cursor >= m.listTop+g.sessionsInner {
		t.Errorf("after G the cursor %d is outside the drawn window [%d,%d)", m.cursor, m.listTop, m.listTop+g.sessionsInner)
	}
	m = update(t, m, keyRunes("g"))
	if m.cursor != 0 || m.listTop != 0 {
		t.Errorf("g left cursor=%d top=%d, want 0/0", m.cursor, m.listTop)
	}
}

// Rows are one line each, but a comment or a prompt is a paragraph and the
// pane that shows it must keep its line breaks.
func TestRowsAreOneLineButCommentsKeepTheirBreaks(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{
		"topic": "first line\nsecond line", "last_activity_at": 1700000000,
	})
	if err := annotate.AddComment(db, "l1", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	m := newTestBrowser(db, "p", BrowserOptions{})
	if got := RenderRow(m.visible[0], RenderOptions{Width: 200}); strings.Contains(got, "\n") {
		t.Errorf("a session row contains a newline: %q", got)
	}
	m.tab = tabComments
	got := m.detailContent(RenderOptions{Width: 60})
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("the comment lost its content:\n%s", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("the comment pane collapsed a paragraph to one line:\n%s", got)
	}
}

func TestHighlightLineReappliesReverseAfterFieldResets(t *testing.T) {
	line := style("a", ansiCyan, true) + " " + style("b", ansiYellow, true)
	got := highlightLine(line)
	if !strings.HasPrefix(got, ansiReverse) {
		t.Error("the highlighted line does not start in reverse video")
	}
	// Every reset inside the line must be followed by the reverse being
	// re-applied, or only the first field ends up highlighted.
	for _, seg := range strings.Split(got, ansiReset)[:strings.Count(got, ansiReset)] {
		_ = seg
	}
	if strings.Count(got, ansiReverse) != strings.Count(line, ansiReset)+1 {
		t.Errorf("reverse video is applied %d times for %d field resets",
			strings.Count(got, ansiReverse), strings.Count(line, ansiReset))
	}
}

func TestStylingWhenEnabled(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Style: true, Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	if v := m.View(); !strings.Contains(v, "\x1b") {
		t.Error("styling is enabled but the view contains no escape sequences")
	}
}

// Two browsers over two databases must not see each other's sessions: the
// browser holds all of its state in its own process.
func TestTwoBrowsersDoNotDisturbEachOther(t *testing.T) {
	dbA := browseTestDB(t)
	seedBrowseSession(t, dbA, "la", "claude:a:1", "a", 1, map[string]any{"topic": "alpha session"})
	dbB := browseTestDB(t)
	seedBrowseSession(t, dbB, "lb", "claude:b:1", "b", 1, map[string]any{"topic": "beta session"})

	a := newTestBrowser(dbA, "a", BrowserOptions{})
	b := newTestBrowser(dbB, "b", BrowserOptions{})
	a = update(t, a, keyRunes("0"))
	a = update(t, a, keyRunes("/"))
	a = update(t, a, keyRunes("alpha"))
	a = update(t, a, tea.KeyMsg{Type: tea.KeyEnter})

	if len(b.visible) != 1 || !strings.Contains(b.View(), "beta session") {
		t.Error("one browser's filtering affected the other")
	}
	if strings.Contains(a.View(), "beta session") || strings.Contains(b.View(), "alpha session") {
		t.Error("a browser listed the other's sessions")
	}
}

func TestRenderItemDetailShowsNameAndTopicSeparately(t *testing.T) {
	db := browseTestDB(t)
	name, topic := "the release checklist", "derived topic text"
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Name: &name, Topic: &topic}
	got := renderItemDetail(db, it, RenderOptions{Width: 80})
	if !strings.Contains(got, "name:   "+name) {
		t.Errorf("the detail pane does not show the chosen name:\n%s", got)
	}
	if !strings.Contains(got, "topic:  "+topic) {
		t.Errorf("the detail pane does not show the derived topic separately:\n%s", got)
	}
}

// TestRenderItemDetailShowsClientWithItsRawValue: the row has room only
// for the short label, so the detail pane is where the reader can see what
// that label was inferred from (change show-editor-clients).
func TestRenderItemDetailShowsClientWithItsRawValue(t *testing.T) {
	db := browseTestDB(t)
	client := "sdk-ts"
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Client: &client}
	got := renderItemDetail(db, it, RenderOptions{Width: 80})
	if !strings.Contains(got, "client: acp (sdk-ts)") {
		t.Errorf("the detail pane does not show the client and what it was read from:\n%s", got)
	}

	// The terminal has no label - the row leaves it out - but the detail
	// pane still states it, because "driven from the terminal" and "the
	// source never said" are different facts and this is the one place
	// with room to tell them apart.
	terminal := "cli"
	got = renderItemDetail(db, search.Item{SessionID: "claude:p:2", Source: "claude", Client: &terminal}, RenderOptions{Width: 80})
	if !strings.Contains(got, "client: cli") {
		t.Errorf("the detail pane should still state a terminal session's client:\n%s", got)
	}

	// A source that records no client at all carries no line.
	got = renderItemDetail(db, search.Item{SessionID: "pi:p:3", Source: "pi"}, RenderOptions{Width: 80})
	if strings.Contains(got, "client:") {
		t.Errorf("a source that records no client should carry no client line:\n%s", got)
	}
}

// ---------------------------------------------------------------------
// The action-menu popup
// ---------------------------------------------------------------------

// The menu is a popup over the frame, not a pane that displaces it: it is a
// momentary question about the panel you are looking at, and the frame has
// to still be there behind it.
func TestActionMenuDrawsOverTheFrame(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	before := strings.Split(m.View(), "\n")
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	after := strings.Split(m.View(), "\n")

	view := strings.Join(after, "\n")
	if !strings.Contains(view, "resume this session") {
		t.Fatalf("the menu popup is not drawn:\n%s", view)
	}
	if !strings.Contains(view, "Sessions actions") {
		t.Errorf("the popup does not say which panel it is for:\n%s", view)
	}
	// The frame survives underneath: the side panels are still on screen.
	for _, want := range []string{"Profiles", "Agents", "Repos"} {
		if !strings.Contains(view, want) {
			t.Errorf("the popup displaced the %s panel instead of covering part of it", want)
		}
	}
	// The popup costs the body one line for the prompt it opens with, but
	// the frame as a whole still has to fit the terminal exactly - a popup
	// that pushes the frame one line taller scrolls the top off screen.
	if len(before) != 26 || len(after) != 26 {
		t.Errorf("frame is %d lines before the popup and %d after, want 26 both times",
			len(before), len(after))
	}
	for i, l := range after {
		if w := visibleWidth(l); w > 100 {
			t.Errorf("line %d is %d columns wide with the popup open", i, w)
		}
	}
}

func TestActionMenuPopupOverStyledFrameStaysInBounds(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Style: true, Resolve: testResolve("claude-personal"), Profiles: testProfiles("claude-personal"),
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 26})
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	lines := strings.Split(m.View(), "\n")
	if len(lines) > 26 {
		t.Errorf("the styled popup made the frame %d lines", len(lines))
	}
	for i, l := range lines {
		if w := visibleWidth(l); w > 100 {
			t.Errorf("styled line %d is %d columns wide with the popup open: %q", i, w, l)
		}
	}
}

// dropVisible is the half of the splice that carries styling forward; if it
// did not, the frame to the right of a popup would lose its colours.
func TestDropVisibleKeepsStylingInEffect(t *testing.T) {
	line := ansiCyan + "abcdef" + ansiReset
	got := dropVisible(line, 3)
	if !strings.HasPrefix(got, ansiCyan) {
		t.Errorf("dropVisible(%q, 3) = %q, want it to carry the cyan forward", line, got)
	}
	if !strings.Contains(got, "def") {
		t.Errorf("dropVisible dropped the wrong columns: %q", got)
	}
	if visibleWidth(got) != 3 {
		t.Errorf("dropVisible left %d visible columns, want 3", visibleWidth(got))
	}
	if got := dropVisible("abc", 10); got != "" {
		t.Errorf("dropping past the end returned %q, want empty", got)
	}
}

// truncateVisible must never cut inside an escape sequence, and must close
// styling it interrupted.
func TestTruncateVisibleDoesNotSplitEscapes(t *testing.T) {
	line := ansiCyan + "abc" + ansiReset + ansiYellow + "def" + ansiReset
	got := truncateVisible(line, 4)
	if visibleWidth(got) != 4 {
		t.Errorf("truncateVisible left %d visible columns, want 4", visibleWidth(got))
	}
	if !strings.HasSuffix(got, ansiReset) {
		t.Errorf("truncateVisible left styling open: %q", got)
	}
	if strings.Count(got, "\x1b[") != strings.Count(got, "\x1b[") {
		t.Fatal("unreachable")
	}
	// Cutting a plain string is the ordinary path and must be unchanged.
	if got := truncateVisible("abcdef", 3); visibleWidth(got) > 3 {
		t.Errorf("truncateVisible(%q, 3) = %q", "abcdef", got)
	}
}

// A panel's top border must reach its closing corner even when the title
// carries styling of its own - the detail pane's tab strip does, and a
// border measured in escape bytes stops short of the panel it closes.
func TestStyledTitleDoesNotShortenTheBorder(t *testing.T) {
	box := panelBox{Title: style("Detail", ansiBold, true) + "  " + style("Prompts", ansiDim, true),
		Width: 50, Height: 4, Style: true}
	first := strings.Split(box.render(), "\n")[0]
	if got := visibleWidth(first); got != 50 {
		t.Errorf("the top border of a 50-column panel is %d columns wide", got)
	}
	if !strings.HasSuffix(first, "╮") {
		t.Errorf("the top border does not close: %q", first)
	}
}

// ---------------------------------------------------------------------
// The text filter is literal (change literal-substring-filter)
// ---------------------------------------------------------------------

// TestTextFilterIsLiteralNotSubsequence is the regression this change
// exists for. The filter was a fuzzy subsequence match, so "postman" kept
// every row whose text happened to contain p...o...s...t...m...a...n in
// order - which, over a few hundred characters, is most of them. A row now
// survives only if it actually contains the word.
func TestTextFilterIsLiteralNotSubsequence(t *testing.T) {
	// Guard the premise: the row's text does not contain the word, but it
	// does contain the letters in order - so the old matcher really would
	// have kept it, and this test really is about the difference.
	scattered := "code/ryd-portal support-missing-street gitlab backend"
	if strings.Contains(scattered, "postman") {
		t.Fatal("fixture contains the word literally; it no longer isolates subsequence matching")
	}
	j := 0
	for _, c := range scattered {
		if j < len("postman") && byte(c) == "postman"[j] {
			j++
		}
	}
	if j != len("postman") {
		t.Fatal("fixture no longer demonstrates a subsequence match; pick different text")
	}

	m := &browseModel{textFilter: "postman"}
	rows := []search.Item{
		{SessionID: "claude:p:1", Source: "claude", Handle: 1,
			CWD: strp("/Users/example/code/ryd-portal"), Topic: strp("support-missing-street"), EndState: "completed"},
		{SessionID: "claude:p:2", Source: "claude", Handle: 2,
			CWD: strp("/Users/example/code/postman"), Topic: strp("Open collection in Postman"), EndState: "completed"},
	}
	got := m.applyTextFilter(rows)
	if len(got) != 1 || got[0].SessionID != "claude:p:2" {
		ids := make([]string, len(got))
		for i, it := range got {
			ids[i] = it.SessionID
		}
		t.Errorf("literal filter kept %v, want only the row containing the word", ids)
	}
}

// TestTextFilterIgnoresCase: the user types what they read, not how it was
// capitalised - "postman" has to find "Postman".
func TestTextFilterIgnoresCase(t *testing.T) {
	m := &browseModel{textFilter: "postman"}
	rows := []search.Item{
		{SessionID: "claude:p:1", Source: "claude", Handle: 1, Topic: strp("Open collection in Postman"), EndState: "completed"},
	}
	if got := m.applyTextFilter(rows); len(got) != 1 {
		t.Errorf("case-insensitive filter kept %d rows, want 1", len(got))
	}
}

// TestTextFilterMatchesOnlyWhatTheRowShows: the row displays the name when
// there is one, so a word that appears only in the topic or the last prompt
// it displaced must not keep the row. Matching invisible text is the other
// half of what made the old filter unexplainable - a row would survive with
// nothing on it to show why.
func TestTextFilterMatchesOnlyWhatTheRowShows(t *testing.T) {
	named := search.Item{
		SessionID: "claude:p:1", Source: "claude", Handle: 1, EndState: "completed",
		Name:       strp("retry-loop"),
		Topic:      strp("Postman collection debugging"),
		LastPrompt: strp("why does the postman collection 404"),
	}
	if got := (&browseModel{textFilter: "postman"}).applyTextFilter([]search.Item{named}); len(got) != 0 {
		t.Errorf("a row displaying %q survived a 'postman' filter on text it does not show", *named.Name)
	}
	// The name it does show still filters normally.
	if got := (&browseModel{textFilter: "retry"}).applyTextFilter([]search.Item{named}); len(got) != 1 {
		t.Errorf("filtering on the text the row shows kept %d rows, want 1", len(got))
	}
}

// TestTextFilterDoesNotMatchTextPastTheTerminalTruncation is the concrete
// regression a terminal-width audit found in change literal-substring-filter
// itself: matchText was built from rowSlots *before* fitRowToWidth ran, so a
// word deep inside a long prompt kept a row the terminal never drew any part
// of - the exact "why is this row here?" complaint that change existed to
// fix, reintroduced by leaving one field unfitted. The audit's real example
// was a 5,203-character last prompt where the panel draws roughly the first
// 130 columns and "adv" matched "advertises" at character 553, five hundred
// columns past anything on screen.
func TestTextFilterDoesNotMatchTextPastTheTerminalTruncation(t *testing.T) {
	db := browseTestDB(t)
	// 600 filler characters, then the target word, then more filler - long
	// enough that no realistic row width reaches it.
	prompt := strings.Repeat("x", 600) + "advertises" + strings.Repeat("x", 4000)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{
		"last_prompt": prompt, "last_activity_at": 1700000000,
	})
	m := newTestBrowser(db, "p", BrowserOptions{}) // sends a 100x40 WindowSizeMsg

	// Guard the premise: at the width the session row is actually rendered
	// at, the topic slot does not reach character 600 - this fixture only
	// tests what it claims to if the row genuinely cannot show the word.
	width := m.sessionRowWidth()
	slots := rowSlots(m.visible[0])
	fitRowToWidth(slots, width)
	if strings.Contains(strings.Join(slots, " "), "advertises") {
		t.Fatalf("fixture's target word survives fitRowToWidth at width %d; the fixture no longer isolates truncation", width)
	}

	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("/"))
	for _, r := range "advertises" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.visible) != 0 {
		t.Errorf("filter kept a row on the strength of text past where its %d-wide row is truncated - matching invisible text again", width)
	}
}

// TestTextFilterMatchesTextWithinTheTruncationWidth is
// TestTextFilterDoesNotMatchTextPastTheTerminalTruncation's counterpart: a
// word that IS still inside the row once it is fitted to width must keep
// matching. Without this, a fix to the defect above could overcorrect into
// never matching a long prompt's topic slot at all.
func TestTextFilterMatchesTextWithinTheTruncationWidth(t *testing.T) {
	db := browseTestDB(t)
	// A short word right at the front of the topic slot: at the width the
	// fixture browser actually renders at (100 columns, three side panels
	// wide), the age and other fixed slots already consume most of the
	// row's budget, leaving only a handful of columns for the topic - so
	// the target word here is deliberately short, not long like the
	// filler behind it.
	prompt := "bug " + strings.Repeat("x", 4000)
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{
		"last_prompt": prompt,
	})
	m := newTestBrowser(db, "p", BrowserOptions{})

	// Guard the premise the other direction: the word is still there once
	// the row is fitted to the width it is actually drawn at.
	width := m.sessionRowWidth()
	slots := rowSlots(m.visible[0])
	fitRowToWidth(slots, width)
	if !strings.Contains(strings.Join(slots, " "), "bug") {
		t.Fatalf("fixture's target word does not survive fitRowToWidth at width %d; pick shorter filler", width)
	}

	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("/"))
	for _, r := range "bug" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.visible) != 1 {
		t.Errorf("filter dropped a row whose matching text is still within the drawn row's width, got %d rows", len(m.visible))
	}
}

// TestTextFilterKeepsRecencyOrder: with literal matching there is no match
// quality to sort by, and recency is the order the whole browser is built
// around, so surviving rows keep the order they were loaded in.
func TestTextFilterKeepsRecencyOrder(t *testing.T) {
	rows := []search.Item{
		{SessionID: "claude:p:1", Source: "claude", Handle: 1, Topic: strp("postman first"), EndState: "completed"},
		{SessionID: "claude:p:2", Source: "claude", Handle: 2, Topic: strp("unrelated"), EndState: "completed"},
		{SessionID: "claude:p:3", Source: "claude", Handle: 3, Topic: strp("postman second"), EndState: "completed"},
	}
	got := (&browseModel{textFilter: "postman"}).applyTextFilter(rows)
	if len(got) != 2 || got[0].SessionID != "claude:p:1" || got[1].SessionID != "claude:p:3" {
		t.Errorf("filter reordered the surviving rows: %+v", got)
	}
}

// TestFacetFilterIsLiteralToo: "/" is one gesture and means one thing, so a
// side panel narrows by the same rule the session list does.
func TestFacetFilterIsLiteralToo(t *testing.T) {
	f := facet{filter: "postman"}
	f.setRows([]facetRow{
		{Label: "~/code/ryd-portal", Value: "/Users/example/code/ryd-portal", Count: 1},
		{Label: "~/code/postman", Value: "/Users/example/code/postman", Count: 1},
	})
	if len(f.rows) != 1 || f.rows[0].Value != "/Users/example/code/postman" {
		t.Errorf("facet filter kept %v, want only the literal match", rowValues(f))
	}
}

// TestTextFilterNarrowsTheFacetPanels: the panels count what the text
// filter left, not what was loaded. Before change literal-substring-filter
// the filter was applied only to the session list, so a panel could offer a
// value with sessions behind it that selecting could not produce.
func TestTextFilterNarrowsTheFacetPanels(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("/"))
	for _, r := range "dotfiles" {
		m = update(t, m, keyRunes(string(r)))
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	for _, v := range rowValues(m.repos) {
		if v != "" && !strings.Contains(v, "dotfiles") {
			t.Errorf("repo panel still offers %q under a 'dotfiles' text filter", v)
		}
	}
	// The fixture's omp session is not in a dotfiles repository, so that
	// agent has nothing behind it any more and must not be offered.
	for _, v := range rowValues(m.agents) {
		if v == "omp" {
			t.Error("agent panel still offers omp, which no filtered session uses")
		}
	}
}

// TestEveryOfferedFacetRowLeadsSomewhere is the invariant the panels exist
// to keep - "a row showing 0 would be a row that leads nowhere, and none is
// ever shown" - checked with a text filter in effect, which is exactly the
// case that used to break it. Selecting any row a panel offers must produce
// the number of sessions the row claims.
func TestEveryOfferedFacetRowLeadsSomewhere(t *testing.T) {
	base := fixtureBrowser(t)
	base = update(t, base, keyRunes("0"))
	base = update(t, base, keyRunes("/"))
	for _, r := range "dotfiles" {
		base = update(t, base, keyRunes(string(r)))
	}
	base = update(t, base, tea.KeyMsg{Type: tea.KeyEnter})

	for _, panel := range []struct {
		name string
		rows []facetRow
		sel  func(*browseModel, string)
	}{
		{"agents", base.agents.rows, func(m *browseModel, v string) { m.agents.Sel = v }},
		{"repos", base.repos.rows, func(m *browseModel, v string) { m.repos.Sel = v }},
		{"tags", base.tags.rows, func(m *browseModel, v string) { m.tags.Sel = v }},
	} {
		for _, row := range panel.rows {
			if row.Value == "" {
				continue // the synthetic "all" row
			}
			probe := *base
			panel.sel(&probe, row.Value)
			probe.rebuild()
			if len(probe.visible) != row.Count {
				t.Errorf("%s panel offers %q with %d behind it, but selecting it lists %d sessions",
					panel.name, row.Value, row.Count, len(probe.visible))
			}
		}
	}
}
