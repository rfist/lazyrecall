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

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rfist/lazyrecall/internal/annotate"
	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/schema"
	"github.com/rfist/lazyrecall/internal/search"
	"github.com/rfist/lazyrecall/internal/sqlitex"
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

// seedLineageOverride sets a lineage's manual group and archive state
// directly, after seedBrowseSession has already created the row (its own
// lineage insert only carries handle/orphaned) - the group/archive tests
// below need lineages.group_name and lineages.archived_at, which no other
// helper here writes. Values are Go string literals the test itself
// chooses, never external input, so building the statement by hand rather
// than through sqlitex's param mechanism carries no injection risk.
func seedLineageOverride(t *testing.T, db *sqlitex.Runner, lineageID, groupName string, archived bool) {
	t.Helper()
	groupSQL, archivedSQL := "NULL", "NULL"
	if groupName != "" {
		groupSQL = "'" + groupName + "'"
	}
	if archived {
		archivedSQL = "1700000000"
	}
	if err := db.Exec(fmt.Sprintf("UPDATE lineages SET group_name = %s, archived_at = %s WHERE id = '%s';", groupSQL, archivedSQL, lineageID)); err != nil {
		t.Fatal(err)
	}
}

// newTestBrowser builds a browser over db. name is kept only for call-site
// compatibility with every existing test here - profile switching (and so
// the active profile a browser opened "as") is gone (change
// group-sessions-in-one-index: one browsing session covers every install's
// data at once).
func newTestBrowser(db *sqlitex.Runner, name string, opts BrowserOptions) *browseModel {
	_ = name
	opts.DB = db
	m := newBrowseModel(opts)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return nm.(*browseModel)
}

// testInstalls builds a BrowserOptions.Installs lister over a fixed,
// synthetic set of install names, so the Transcript tab's install lookup
// and the refresh action never depend on this machine's real config roots.
func testInstalls(names ...string) func() []profile.Profile {
	return func() []profile.Profile {
		out := make([]profile.Profile, len(names))
		for i, n := range names {
			out[i] = profile.Profile{Name: n}
		}
		return out
	}
}

// noInstallInfo is the installInfo stand-in for renderItemDetail tests that
// don't care about the install line: no discovered install, so the line
// falls back to (or omits) exactly what a real lookup miss would produce.
func noInstallInfo(search.Item) (label, root string, ok bool) { return "", "", false }

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
		Installs: testInstalls("claude-personal", "claude-work"),
	})
}

// groupFixtureBrowser builds a browser over a synthetic corpus exercising
// every branch of "which group is this session in" (change
// group-sessions-in-one-index): two path-derived sessions each in "work"
// and one in "personal" (g0/g1/g2), one with no cwd any group path claims
// (g3, Unknown), one archived without ever having a manual group (g4,
// counted under Archive rather than its path-derived "work"), and one whose
// cwd alone would be Unknown but that carries a manual override filing it
// under "work" regardless (g5). All content is hand-written, never real
// session data.
func groupFixtureBrowser(t *testing.T) *browseModel {
	t.Helper()
	db := browseTestDB(t)
	seedBrowseSession(t, db, "G0", "claude:p:g0", "p", 1, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 600})
	seedBrowseSession(t, db, "G1", "claude:p:g1", "p", 2, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 500})
	seedBrowseSession(t, db, "G2", "claude:p:g2", "p", 3, map[string]any{"cwd": "/Users/x/personal/proj", "last_activity_at": 400})
	seedBrowseSession(t, db, "G3", "claude:p:g3", "p", 4, map[string]any{"cwd": "/tmp", "last_activity_at": 300})
	seedBrowseSession(t, db, "G4", "claude:p:g4", "p", 5, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 200})
	seedBrowseSession(t, db, "G5", "claude:p:g5", "p", 6, map[string]any{"cwd": "/tmp", "last_activity_at": 100})
	seedLineageOverride(t, db, "G4", "", true)      // archived, no manual group
	seedLineageOverride(t, db, "G5", "work", false) // manual override, path alone would be Unknown

	groups := []config.Group{
		{Name: "work", Paths: []string{"/Users/x/work"}},
		{Name: "personal", Paths: []string{"/Users/x/personal"}},
	}
	return newTestBrowser(db, "p", BrowserOptions{
		Groups:   groups,
		Installs: testInstalls("claude"),
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
	sizes := [][2]int{
		{100, 40}, {100, 26}, {120, 30}, {80, 24}, {76, 20}, {70, 20}, {60, 14}, {40, 10},
		{100, 4}, {100, 5}, {100, 6}, {100, 7}, {100, 8}, {100, 9}, {100, 10}, {100, 11}, {100, 12},
		{60, 4}, {60, 5}, {60, 6}, {60, 7}, {60, 8}, {60, 9}, {60, 10}, {60, 11}, {60, 12},
	}
	for _, styled := range []bool{false, true} {
		for _, size := range sizes {
			w, h := size[0], size[1]
			db := browseTestDB(t)
			seedFixture(t, db)
			m := newTestBrowser(db, "claude-personal", BrowserOptions{
				Style: styled, Installs: testInstalls("claude-personal"),
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
// other. The narrow and wide cases together matter: the former stacks six
// panels while the latter joins two independently allocated columns.
func TestViewFitsWithPromptOpen(t *testing.T) {
	for _, width := range []int{60, 100} {
		for height := 4; height <= 12; height++ {
			m := fixtureBrowser(t)
			m = update(t, m, tea.WindowSizeMsg{Width: width, Height: height})
			m = update(t, m, keyRunes("/"))
			if m.mode != modeFilter {
				t.Fatalf("%dx%d: expected the filter prompt to be open, got mode %v", width, height, m.mode)
			}
			if lines := strings.Split(m.View(), "\n"); len(lines) > height {
				t.Errorf("%dx%d: frame with a prompt open is %d lines, taller than the terminal", width, height, len(lines))
			}
		}
	}
}

// ---------------------------------------------------------------------
// Focus
// ---------------------------------------------------------------------

func TestDigitsJumpToPanels(t *testing.T) {
	m := fixtureBrowser(t)
	for key, want := range map[string]panelID{
		"1": panelGroups, "2": panelAgents, "3": panelRepos, "4": panelTags, "0": panelSessions,
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
	want := []panelID{panelGroups, panelAgents, panelRepos, panelTags, panelSessions, panelDetail}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("tab visited %v, want %v", seen, want)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != panelGroups {
		t.Errorf("tab past the last panel went to %v, want it to wrap to Groups", m.focus)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.focus != panelDetail {
		t.Errorf("shift-tab from the first panel went to %v, want it to wrap to Detail", m.focus)
	}
}

// A terminal too narrow for two columns stacks every panel into one column
// rather than dropping four of them (change stack-panels-when-narrow), so
// every panel stays reachable by its digit and by Tab. The rule this test
// guards is unchanged and is the one that matters - focus never lands on
// something not drawn - but at this width that is now satisfied by drawing
// everything instead of by refusing to move.
func TestNarrowTerminalStacksPanelsAndKeepsThemReachable(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	if m.geometry().sidebar {
		t.Fatal("fixture at 60x20 still draws two columns; pick a narrower width for this test to mean anything")
	}
	m = update(t, m, keyRunes("3"))
	if m.focus != panelRepos {
		t.Errorf("3 at 60 columns focused %v, want Repos: the panel is stacked, not dropped", m.focus)
	}
	for i := 0; i < 8; i++ {
		m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
		if !m.panelDrawn(m.focus) {
			t.Fatalf("tab reached %v, which is not drawn at this width", m.focus)
		}
	}
}

// The narrow stack pays Sessions' three-row floor before it hands out the
// other headers, so 60x8 leaves only four rows for five collapsibles. The
// panel receiving focus must keep its one-row minimum anyway: the missing
// row belongs to the tail of draw order, Detail, not to the panel the user
// just selected. Opening "/" costs another body row without changing focus,
// so the same rule must hold while input is open as well.
func TestFocusedNarrowPanelKeepsAHeaderAtMinimumBodyHeight(t *testing.T) {
	for _, openPrompt := range []bool{false, true} {
		m := fixtureBrowser(t)
		m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 8})
		m = update(t, m, keyRunes("1"))
		if openPrompt {
			m = update(t, m, keyRunes("/"))
		}
		if m.focus != panelGroups {
			t.Fatalf("prompt=%v: 1 focused %v, want Groups", openPrompt, m.focus)
		}
		if h := m.geometry().groupsH; h < 1 {
			t.Errorf("prompt=%v: focused Groups has height %d at 60x8", openPrompt, h)
		}
	}
}

// When there are fewer rows than stacked panels, a candidate that is absent
// from the current focus's layout can still receive its header after focus
// moves to it. J from Sessions to Detail at 60x8 is that shape: Groups'
// focus gave Detail the tail row up, then Sessions did; Detail must be
// judged against Detail's destination layout, not the Sessions layout that
// is about to cease to exist.
func TestSpatialFocusChecksTheDestinationLayout(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 8})
	m = update(t, m, keyRunes("1"))
	m = update(t, m, keyRunes("L"))
	if m.focus != panelSessions {
		t.Fatalf("L from Groups focused %v, want Sessions", m.focus)
	}
	if h := m.geometry().detailH; h != 0 {
		t.Fatalf("fixture leaves Detail at height %d before J; destination-layout check is not exercised", h)
	}
	m = update(t, m, keyRunes("J"))
	if m.focus != panelDetail {
		t.Errorf("J from Sessions focused %v, want Detail when its destination layout draws it", m.focus)
	}
	if h := m.geometry().detailH; h < 1 {
		t.Errorf("focused Detail has height %d after the move", h)
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
		{"H from Sessions goes to Groups", panelSessions, "H", panelGroups},
		{"H from Detail goes to Groups", panelDetail, "H", panelGroups},
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

// H always resolves to Groups specifically, never to whichever left panel
// most recently had focus - the user asked for exactly that, rejecting the
// cleverer "nearest panel" rule. This is the case that would tell the two
// apart: Agents was the last left panel visited, so a "nearest" rule would
// send H there instead of to Groups.
func TestHAlwaysReturnsToGroupsNotTheLastLeftPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m.focus = panelAgents
	m = update(t, m, keyRunes("L")) // Agents -> Sessions
	if m.focus != panelSessions {
		t.Fatalf("L from Agents landed on %v, want Sessions", m.focus)
	}
	m = update(t, m, keyRunes("H"))
	if m.focus != panelGroups {
		t.Errorf("H from Sessions landed on %v, want Groups, not the last left panel visited", m.focus)
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
		{"K on Groups", panelGroups, "K"},
		{"J on Tags", panelTags, "J"},
		{"L on Sessions", panelSessions, "L"},
		{"L on Detail", panelDetail, "L"},
		{"H on Groups", panelGroups, "H"},
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

// A terminal too narrow for two columns stacks the panels into one instead
// of dropping the side panels (change stack-panels-when-narrow), so H from
// Sessions reaches Groups at 60 columns exactly as it does at 100. This
// test used to assert the opposite - that H found nothing drawn and left
// focus alone - which was true only while a narrow terminal amputated four
// of the six panels.
func TestSpatialMoveReachesTheStackedPanelsWhenNarrow(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	if m.geometry().sidebar {
		t.Fatal("fixture at 60x20 still draws two columns; pick a narrower width for this test to mean anything")
	}
	for _, p := range []panelID{panelGroups, panelAgents, panelRepos, panelTags} {
		if !m.panelDrawn(p) {
			t.Fatalf("%v is not drawn at 60x20; the stacked layout must keep every panel on screen", p)
		}
	}
	m.focus = panelSessions
	m = update(t, m, keyRunes("H"))
	if m.focus != panelGroups {
		t.Errorf("H at 60 columns landed on %v, want Groups: the panels are stacked, not dropped", m.focus)
	}
}

// The accordion layout keeps every left panel on screen at every height:
// a collapsed panel costs one header line, so the "rest < 8" branch that
// used to drop Tags (tagsH == 0) on a short terminal is gone. At the old
// vanishing size, J from Repos now moves onto Tags instead of finding
// nothing drawn and doing nothing. This test used to assert the drop - it
// was called TestSpatialMoveSkipsDroppedTagsPanel - and was rewritten the
// other way around when the accordion made a dropped Tags impossible.
func TestSpatialMoveReachesTagsOnAShortTerminal(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if g := m.geometry(); !g.sidebar || g.tagsH < 1 {
		t.Fatalf("fixture at 100x18 has sidebar=%v tagsH=%d, want a drawn Tags panel for this test to mean anything", g.sidebar, g.tagsH)
	}
	if !m.panelDrawn(panelTags) {
		t.Fatal("Tags is not drawn at 100x18; the accordion was supposed to keep every panel visible")
	}
	m.focus = panelRepos
	m = update(t, m, keyRunes("J"))
	if m.focus != panelTags {
		t.Errorf("J from Repos landed on %v, want Tags now that every left panel is drawn", m.focus)
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

	// Narrowing no longer removes Tags - it stacks the panels into one
	// column - so focus legitimately stays put. What must still hold is the
	// invariant the resize handler exists for: focus is never left on
	// something that is not on screen.
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	if m.geometry().sidebar {
		t.Fatal("fixture at 60x20 still draws two columns; pick a narrower width for this test to mean anything")
	}
	if !m.panelDrawn(m.focus) {
		t.Errorf("resize left focus on %v, which is not currently drawn", m.focus)
	}
	if m.focus != panelTags {
		t.Errorf("resize moved focus to %v; Tags is still drawn when narrow, so focus had no reason to move", m.focus)
	}

	// The invariant still has teeth at a size that genuinely cannot afford
	// every panel a line: there focus must leave and land on Sessions.
	m = update(t, m, tea.WindowSizeMsg{Width: 40, Height: 9})
	if m.panelDrawn(panelTags) {
		t.Skip("40x9 still affords Tags a line; no size in this build exercises the fallback")
	}
	if !m.panelDrawn(m.focus) {
		t.Errorf("resize left focus on %v, which is not currently drawn", m.focus)
	}
	if m.focus != panelSessions {
		t.Errorf("resize moved focus to %v, want it to fall back to Sessions (always drawn)", m.focus)
	}
}

// Tab used to have to skip a zero-height Tags panel on a short terminal
// (moveFocus checked the sidebar bool, not per-panel drawnness, and
// geometry's "rest < 8" branch set tagsH to 0 - see
// TestSpatialMoveReachesTagsOnAShortTerminal for the same fixture and the
// same reversal). There is no zero-height Tags any more: every left panel
// is at least a header whenever the sidebar is drawn, so Tab from Repos
// cycles straight onto Tags, which is focused, expanded, and drawn. This
// test was called TestTabDoesNotLandOnZeroHeightTagsPanel and was rewritten
// with the behavior it used to forbid.
func TestTabLandsOnTagsOnAShortTerminal(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if g := m.geometry(); !g.sidebar || g.tagsH < 1 {
		t.Fatalf("fixture at 100x18 has sidebar=%v tagsH=%d, want a drawn Tags panel for this test to mean anything", g.sidebar, g.tagsH)
	}
	m.focus = panelRepos
	m = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != panelTags {
		t.Errorf("Tab from Repos landed on %v, want Tags now that every left panel is drawn", m.focus)
	}
	if !m.panelDrawn(m.focus) {
		t.Errorf("Tab left focus on %v, which is not currently drawn", m.focus)
	}
}

// ---------------------------------------------------------------------
// Accordion side panels
// ---------------------------------------------------------------------

// Every left panel is drawn at every height the sidebar exists at. The old
// layout's floor was a three-line box, so four panels stopped fitting below
// a certain body height and Tags was dropped (the "rest < 8" branch); a
// collapsed panel's floor is one header line, so 100x18 - the size that
// used to drop Tags - now shows all four.
func TestAllSidePanelsDrawnAtShortHeight(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if g := m.geometry(); !g.sidebar {
		t.Fatal("fixture at 100x18 has no sidebar; pick a wider terminal for this test to mean anything")
	}
	g := m.geometry()
	if g.groupsH < 1 || g.agentsH < 1 || g.reposH < 1 || g.tagsH < 1 {
		t.Errorf("a side panel has zero height at 100x18: %+v", g)
	}
	for _, p := range []panelID{panelGroups, panelAgents, panelRepos, panelTags} {
		if !m.panelDrawn(p) {
			t.Errorf("%s is not drawn at 100x18", p.title())
		}
	}
}

// The focused panel takes the space the others give up: the three unfocused
// panels are one-line headers, the focused one gets everything that is left,
// and the four still tile the column exactly - the invariant that keeps the
// frame from outgrowing the terminal.
func TestFocusedPanelIsTallerThanTheUnfocusedOnes(t *testing.T) {
	m := fixtureBrowser(t)
	// A short column: with a single "All" row (change
	// group-sessions-in-one-index) the fixture's Groups and Agents take 9
	// rows between them, so anything under a body of 17 leaves Repos and
	// Tags too little to share and the accordion takes over. At a taller
	// size the roomy sizing applies instead and every panel keeps its
	// content, which is what this test would otherwise be asserting
	// against.
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 17})
	m = update(t, m, keyRunes("2")) // Agents
	g := m.geometry()
	if g.agentsH <= g.groupsH || g.agentsH <= g.reposH || g.agentsH <= g.tagsH {
		t.Errorf("focused Agents (%d) is not taller than the unfocused panels (P=%d R=%d T=%d)", g.agentsH, g.groupsH, g.reposH, g.tagsH)
	}
	if g.groupsH != 1 || g.reposH != 1 || g.tagsH != 1 {
		t.Errorf("unfocused panels are not one-line headers: P=%d R=%d T=%d", g.groupsH, g.reposH, g.tagsH)
	}
	if g.groupsH+g.agentsH+g.reposH+g.tagsH != g.bodyHeight {
		t.Errorf("left column %d != body %d", g.groupsH+g.agentsH+g.reposH+g.tagsH, g.bodyHeight)
	}
}

// The expansion follows focus: the panel a Tab or a digit key lands on is
// the one that gets the space, so moving from Groups to Repos and back
// swaps which of the two is tall and which is a header.
func TestMovingFocusMovesTheExpansion(t *testing.T) {
	m := fixtureBrowser(t)
	// A short column: with a single "All" row (change
	// group-sessions-in-one-index) the fixture's Groups and Agents take 9
	// rows between them, so anything under a body of 17 leaves Repos and
	// Tags too little to share and the accordion takes over. At a taller
	// size the roomy sizing applies instead and every panel keeps its
	// content, which is what this test would otherwise be asserting
	// against.
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 17})
	m = update(t, m, keyRunes("1")) // Groups
	g := m.geometry()
	if g.groupsH != g.bodyHeight-3 || g.agentsH != 1 || g.reposH != 1 || g.tagsH != 1 {
		t.Fatalf("with Groups focused: P=%d A=%d R=%d T=%d, want P=body-3 and the rest 1", g.groupsH, g.agentsH, g.reposH, g.tagsH)
	}
	m = update(t, m, keyRunes("3")) // Repos
	g = m.geometry()
	if g.reposH != g.bodyHeight-3 || g.groupsH != 1 || g.agentsH != 1 || g.tagsH != 1 {
		t.Fatalf("with Repos focused: P=%d A=%d R=%d T=%d, want R=body-3 and the rest 1", g.groupsH, g.agentsH, g.reposH, g.tagsH)
	}
}

// With focus on the right column no left panel is focused, so the expansion
// has to be decided by state rather than by focus: the panel carrying a
// filter gets the space (it is the one whose state the user is relying on),
// and Repos gets it when none does - it is the longest list on a real
// machine, so the most likely to be worth looking at.
func TestRightColumnFocusExpandsThePanelWithAFilterOrRepos(t *testing.T) {
	m := fixtureBrowser(t)
	// A short column: with a single "All" row (change
	// group-sessions-in-one-index) the fixture's Groups and Agents take 9
	// rows between them, so anything under a body of 17 leaves Repos and
	// Tags too little to share and the accordion takes over. At a taller
	// size the roomy sizing applies instead and every panel keeps its
	// content, which is what this test would otherwise be asserting
	// against.
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 17})
	m = update(t, m, keyRunes("0")) // Sessions
	if g := m.geometry(); g.reposH != g.bodyHeight-3 {
		t.Fatalf("with no filter applied the expansion should go to Repos, got R=%d want %d", g.reposH, g.bodyHeight-3)
	}
	m.agents.Sel = "pi"
	m.rebuild()
	if g := m.geometry(); g.agentsH != g.bodyHeight-3 {
		t.Fatalf("with an agent filter the expansion should go to Agents, got A=%d want %d", g.agentsH, g.bodyHeight-3)
	}
}

// A collapsed panel is a header line that still says what it filters by:
// with Repos collapsed and a repository applied, the header carries that
// repository instead of a bare title, so the filter is never invisible just
// because the panel lost its rows.
func TestCollapsedPanelShowsItsAppliedValue(t *testing.T) {
	m := fixtureBrowser(t)
	// A short column: the fixture's Groups and Agents take 10 rows
	// between them, so anything under a body of 18 leaves Repos and Tags
	// too little to share and the accordion takes over. At a taller size
	// the roomy sizing applies instead and every panel keeps its content,
	// which is what this test would otherwise be asserting against.
	// Shorter than the other accordion tests on purpose: applying a
	// repository filter shrinks the Agents list, which gives the column
	// back rows and tips it into the roomy sizing at 18. 14 keeps it short
	// however the facets narrow.
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 14})
	m.repos.Sel = "/Users/x/work/api"
	m.rebuild()
	m = update(t, m, keyRunes("2")) // Agents expands, Repos collapses
	g := m.geometry()
	box := m.facetPanel(panelRepos, &m.repos, g.leftWidth, g.reposH, m.repos.Sel)
	if !box.Collapsed {
		t.Fatal("Repos is not collapsed while Agents is focused")
	}
	line := box.render()
	if !strings.Contains(line, "work/api") {
		t.Errorf("collapsed Repos header %q does not show the applied filter", line)
	}
	if len(strings.Split(strings.TrimRight(line, "\n"), "\n")) != 1 {
		t.Errorf("collapsed Repos renders %q, which is not a single line", line)
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

// TestClearAllFiltersAlsoResetsTheGroup is the P1 regression test for a
// review finding against group-sessions-in-one-index: X cleared every other
// facet but left the group filter (m.group and m.groups_.Sel) untouched, so
// a session list narrowed to one group stayed narrowed after "clear all
// filters" claimed to have cleared everything.
func TestClearAllFiltersAlsoResetsTheGroup(t *testing.T) {
	m := groupFixtureBrowser(t)
	m.setGroupFilter("work")
	m.agents.Sel = "claude" // the "another facet" alongside the group selection
	m.rebuild()
	if m.group != "work" || len(m.visible) == 0 {
		t.Fatalf("setup: group=%q visible=%d, want group=work with some sessions before clearing", m.group, len(m.visible))
	}

	m = update(t, m, keyRunes("X"))

	if m.group != "" {
		t.Errorf(`X left group=%q applied, want "" (All)`, m.group)
	}
	if m.groups_.Sel != "" {
		t.Errorf(`X left groups_.Sel=%q applied, want ""`, m.groups_.Sel)
	}
	if m.agents.Sel != "" {
		t.Errorf("X left agents.Sel=%q applied, want \"\"", m.agents.Sel)
	}
	// groupFixtureBrowser has 6 sessions, one archived (G4) and so hidden by
	// default (not showAll) - the same 5 a plain, unfiltered All view shows.
	if len(m.visible) != 5 {
		t.Errorf("%d sessions listed after X, want 5 (every non-archived session, group cleared)", len(m.visible))
	}
}

// Command-line filters and panel selections have to be the same state, or
// `--agent=pi` and walking to "pi" would put the browser in two different
// places.
// BrowserOptions.Agent is a bare source name (a --agent value resolveAgentFilter
// decided spans every install of a source, not one specific install), so it
// seeds agentSource - the source-level dimension - not agents.Sel, which is
// reserved for an install a panel row (or a label/install seed) names
// specifically (change group-sessions-in-one-index; see matchesFacets).
func TestCommandLineFiltersOpenAsPanelSelections(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{
		Agent: "pi", Tag: "wip",
		Installs: testInstalls("claude-personal"),
	})
	if m.agentSource != "pi" || m.agents.Sel != "" || m.tags.Sel != "wip" {
		t.Fatalf("opened with agentSource=%q agents.Sel=%q tag=%q, want pi/\"\"/wip",
			m.agentSource, m.agents.Sel, m.tags.Sel)
	}
	if len(m.visible) != 1 {
		t.Errorf("%d sessions listed under agent=pi tag=wip, want 1", len(m.visible))
	}
}

// TestCommandLineInstallSeedOpensAsAgentsPanelSelection is
// TestCommandLineFiltersOpenAsPanelSelections' install-seed counterpart: a
// --agent value that resolved to one specific install (BrowserOptions.
// Install, from a configured label or an install's own name) seeds
// agents.Sel exactly like walking to that row and pressing Enter would.
func TestCommandLineInstallSeedOpensAsAgentsPanelSelection(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "I0", "claude:cc:1", "cc", 1, map[string]any{"install": "cc", "cwd": "/x"})
	seedBrowseSession(t, db, "I1", "claude:ccp:1", "ccp", 2, map[string]any{"install": "ccp", "cwd": "/y"})
	m := newTestBrowser(db, "p", BrowserOptions{
		Install:  "cc",
		Installs: testInstalls("cc", "ccp"),
	})
	if m.agents.Sel != "cc" || m.agentSource != "" {
		t.Fatalf("opened with agents.Sel=%q agentSource=%q, want cc/\"\"", m.agents.Sel, m.agentSource)
	}
	if len(m.visible) != 1 || m.visible[0].SessionID != "claude:cc:1" {
		t.Errorf("%+v visible under install=cc, want only claude:cc:1", m.visible)
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
	// Counted from the tab set rather than written out, so adding a tab
	// does not silently turn this into a test of the first three.
	for i := 1; i < int(numDetailTabs); i++ {
		m = update(t, m, keyRunes("]"))
	}
	if m.tab != tabDetail {
		t.Errorf("] %d times landed on %v, want it to wrap to Detail", numDetailTabs, m.tab)
	}
	m = update(t, m, keyRunes("["))
	if m.tab != numDetailTabs-1 {
		t.Errorf("[ from Detail went to %v, want it to wrap to the last tab", m.tab)
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
		Installs: testInstalls("claude-personal"),
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

// TestDetailShowsNoCommentsAsNone covers the tags line's "(none)"
// convention carried over to the new comments section on the Detail tab: a
// session with no comments shows "(none)", not a blank section.
func TestDetailShowsNoCommentsAsNone(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	m := newTestBrowser(db, "claude-personal", BrowserOptions{Installs: testInstalls("claude-personal")})
	it := m.visible[0]
	got := renderItemDetail(db, it, nil, noInstallInfo, RenderOptions{Width: 80})
	if !strings.Contains(got, "comments: (none)") {
		t.Errorf("expected the comments section to say (none), got:\n%s", got)
	}
}

// TestDetailShowsUpToThreeRecentCommentsWithOverflowNote covers the Detail
// tab's comments preview: at most the 3 most recent comments, in the same
// date + wrapped-body format renderItemComments uses, and a note of how
// many more exist when there are more than 3 - the Comments tab itself is
// unaffected and still shows every one.
func TestDetailShowsUpToThreeRecentCommentsWithOverflowNote(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	for _, body := range []string{"first note", "second note", "third note", "fourth note"} {
		if err := annotate.AddComment(db, "L0", body); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestBrowser(db, "claude-personal", BrowserOptions{Installs: testInstalls("claude-personal")})
	var it search.Item
	for _, x := range m.all {
		if x.LineageID == "L0" {
			it = x
		}
	}
	got := renderItemDetail(db, it, nil, noInstallInfo, RenderOptions{Width: 80})

	if strings.Contains(got, "first note") {
		t.Errorf("expected only the 3 most recent comments, but the oldest one is shown:\n%s", got)
	}
	for _, want := range []string{"second note", "third note", "fourth note"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected recent comment %q on the Detail tab:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "(+1 more on the Comments tab)") {
		t.Errorf("expected an overflow note for the 1 comment beyond the preview:\n%s", got)
	}

	// The Comments tab itself still shows every comment, unaffected.
	full := renderItemComments(db, it, RenderOptions{Width: 80})
	if !strings.Contains(full, "first note") {
		t.Errorf("the Comments tab should still show every comment:\n%s", full)
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
		t.Errorf("the Sessions menu offers a Groups action: %s", joined)
	}
}

// TestActionMenuLettersDoNotNarrowByDefault covers the menu's default
// navigation (change menu-jk-navigation): a letter typed with no preceding
// "/" is not fed to the menu as narrowing text, and does not so much as
// enter narrowing mode - only "/" does that (TestActionMenuSlashNarrows).
func TestActionMenuLettersDoNotNarrowByDefault(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	before := len(m.menuFiltered)
	for _, r := range "comment" {
		m = update(t, m, keyRunes(string(r)))
	}
	if len(m.menuFiltered) != before {
		t.Fatalf("typing narrowed the menu from %d to %d entries; letters should not narrow by default", before, len(m.menuFiltered))
	}
	if m.menuNarrowing {
		t.Error("typing letters (none of them /) put the menu into narrowing mode")
	}
}

// TestActionMenuJKMoveTheCursor covers j/k as the menu's default navigation
// (change menu-jk-navigation) - the same keys that move every other panel,
// now moving the highlighted menu entry too instead of being stolen for
// narrowing.
func TestActionMenuJKMoveTheCursor(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	start := m.menuCursor
	m = update(t, m, keyRunes("j"))
	if m.menuCursor != start+1 {
		t.Errorf("j left the cursor at %d, want %d", m.menuCursor, start+1)
	}
	m = update(t, m, keyRunes("k"))
	if m.menuCursor != start {
		t.Errorf("k left the cursor at %d, want %d", m.menuCursor, start)
	}
}

// TestActionMenuSlashNarrows covers the "/" quick-filter sub-state (change
// menu-jk-navigation): "/" switches the menu into the old typing-narrows
// behaviour, and Esc leaves narrowing - clearing the filter but leaving the
// menu open - rather than closing the menu outright; a second Esc does
// that.
func TestActionMenuSlashNarrows(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	before := len(m.menuFiltered)

	m = update(t, m, keyRunes("/"))
	if !m.menuNarrowing {
		t.Fatal("/ did not enter narrowing mode")
	}
	for _, r := range "comment" {
		m = update(t, m, keyRunes(string(r)))
	}
	if len(m.menuFiltered) == 0 || len(m.menuFiltered) >= before {
		t.Fatalf("typing after / narrowed the menu from %d to %d entries", before, len(m.menuFiltered))
	}
	for _, a := range m.menuFiltered {
		if !strings.Contains(a.label, "comment") {
			t.Errorf("narrowed menu still offers %q", a.label)
		}
	}

	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.menuNarrowing {
		t.Error("esc did not leave narrowing mode")
	}
	if m.mode != modeMenu {
		t.Fatalf("esc while narrowing closed the menu (mode %v), want it to stay open", m.mode)
	}
	if len(m.menuFiltered) != before {
		t.Errorf("esc left the menu narrowed to %d of %d entries, want the filter cleared", len(m.menuFiltered), before)
	}

	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNone {
		t.Errorf("a second esc left mode %v, want the menu closed", m.mode)
	}
}

// TestActionMenuRunsTheHighlightedAction drives the menu with Down (the
// same movement j/k perform - see TestActionMenuJKMoveTheCursor) to reach
// "clear all filters" and applies it with Enter, since typing the label no
// longer narrows to it by default (change menu-jk-navigation).
func TestActionMenuRunsTheHighlightedAction(t *testing.T) {
	m := fixtureBrowser(t)
	m.agents.Sel = "pi"
	m.rebuild()
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	target := -1
	for i, a := range m.menuFiltered {
		if a.label == "clear all filters" {
			target = i
		}
	}
	if target < 0 {
		t.Fatalf("menu has no 'clear all filters' entry: %+v", m.menuFiltered)
	}
	for m.menuCursor < target {
		m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
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
// Groups
// ---------------------------------------------------------------------

// TestAllRowIsAlwaysMarkedInTheGroupsPanel covers the zero-config case
// (change group-sessions-in-one-index, "Zero-config and public users"):
// with no groups configured the Groups panel shows exactly one row, "All",
// and it is marked applied since it is in fact the active view - not
// "Profiles", the panel this replaced, whose one row was always marked for
// a different reason (it wasn't a filter at all). The accordion layout
// gives a side panel its rows only while it is the one expanded, so the
// panel has to be focused first for this assertion to have rows to look at.
func TestAllRowIsAlwaysMarkedInTheGroupsPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("1"))
	g := m.geometry()
	box := m.facetPanel(panelGroups, &m.groups_, g.leftWidth, g.groupsH, "")
	if len(box.Lines) != 1 {
		t.Fatalf("expected exactly the All row, got %+v", box.Lines)
	}
	if !strings.Contains(box.Lines[0], "All") || !strings.Contains(box.Lines[0], "●") {
		t.Errorf("the All row is not marked, got %q", box.Lines[0])
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
		ShowAll: true, Installs: testInstalls("claude-personal"),
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
		Hide:     config.Hide{MinMessages: 44},
		Installs: testInstalls("claude-personal"),
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
		Hide:     config.Hide{MinMessages: 44},
		Installs: testInstalls("claude-personal"),
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
		Installs: testInstalls("claude-personal"),
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
					Installs: testInstalls("claude-personal"),
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
		Installs: testInstalls("claude-personal"),
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
		Style: false, Installs: testInstalls("claude-personal"),
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

// TestRefreshThroughRealProgramEventLoop drives the real tea.Program - the
// same one RunBrowser constructs - over a piped input, pressing keys as the
// actual bytes a terminal would send, with no test code calling Update or a
// returned command itself. Every other test here proves the *model's*
// logic; only this one proves that bubbletea delivers a real key press
// through Update and feeds the tea.Cmd it returns back in as a message -
// which used to be exercised via a profile switch (now gone, change
// group-sessions-in-one-index) and is exercised here via the refresh
// action instead, the one other async command the browser has.
func TestRefreshThroughRealProgramEventLoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LAZYRECALL_HOME", home)

	// refreshCmd opens the real index at profile.DBPath(), not whatever
	// *sqlitex.Runner the test injects as opts.DB - so for its effect to be
	// observable here, the two must be the same file.
	db := browseTestDBAt(t, profile.DBPath())
	seedBrowseSession(t, db, "l1", "claude:p:1", "p", 1, map[string]any{"topic": "before refresh"})

	opts := BrowserOptions{
		DB: db, Style: false,
		Installs: testInstalls(), // no real installs: the refresh pass is a fast no-op
	}
	m := newBrowseModel(opts)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = *nm.(*browseModel)

	in := &chunkReader{chunks: make(chan []byte, 8)}
	var out syncBuffer
	p := tea.NewProgram(&m, tea.WithInput(in), tea.WithOutput(&out))

	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = p.Run()
		close(done)
	}()

	in.chunks <- []byte("R") // trigger the async refresh action

	// refreshCmd is itself asynchronous - bubbletea runs it in its own
	// goroutine and only feeds its result back in as a refreshDoneMsg once
	// that goroutine returns. Sending "q" without waiting for that to land
	// races the real quit against the real refresh completing, which looks
	// exactly like "the action had no effect" but is a race in this test's
	// own timing.
	deadline := time.Now().Add(5 * time.Second)
	for !out.Contains("index refreshed") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !out.Contains("index refreshed") {
		t.Fatal("the refresh action's result never appeared in the rendered output within 5s")
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

// TestRenameSetsNameAndUpdatesImmediately covers the `r` action end to end:
// it opens the rename prompt blank (no custom name yet), and submitting a
// name updates the row and the Detail tab without dismissing the browser -
// the same "annotation edit reflects immediately" contract tag and comment
// edits already have (TestTagAndCommentEditingUpdateImmediately).
func TestRenameSetsNameAndUpdatesImmediately(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	target := m.visible[0].LineageID

	m = update(t, m, keyRunes("r"))
	if m.mode != modeRename {
		t.Fatalf("r did not open the rename prompt (mode %v)", m.mode)
	}
	if got := m.input.Value(); got != "" {
		t.Errorf("rename prompt should open blank with no custom name yet, got %q", got)
	}
	m = update(t, m, keyRunes("retry-loop"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if got := m.visible[0].CustomName; got == nil || *got != "retry-loop" {
		t.Fatalf("the new name is not on the row: %v", got)
	}
	got, err := renderCustomNameFor(t, m.db, target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != "retry-loop" {
		t.Errorf("SetName was not applied to the lineage: %v", got)
	}
	m.tab = tabDetail
	if got := m.detailContent(RenderOptions{Width: 60}); !strings.Contains(got, "name:   retry-loop") {
		t.Errorf("the Detail tab does not show the new name:\n%s", got)
	}
}

// TestRenamePrefillsCurrentName covers the browser's own conflict with the
// non-interactive command: `r` opens the input bar with the session's
// current custom name already filled in, not blank - an edit of what is
// there, not a fresh prompt (change adding-annotation-shaped-naming).
func TestRenamePrefillsCurrentName(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	if err := annotate.SetName(m.db, m.visible[0].LineageID, "already named"); err != nil {
		t.Fatal(err)
	}
	m.loadAll()

	m = update(t, m, keyRunes("r"))
	if got := m.input.Value(); got != "already named" {
		t.Errorf("rename prompt value = %q, want it pre-filled with the current name", got)
	}
}

// TestRenameEmptyClears covers submitting a blank rename prompt: unlike
// modeAddTag/modeAddComment, where an empty submission is a no-op, an empty
// rename clears a name set earlier (annotate.SetName's own contract).
func TestRenameEmptyClears(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	if err := annotate.SetName(m.db, m.visible[0].LineageID, "already named"); err != nil {
		t.Fatal(err)
	}
	m.loadAll()

	m = update(t, m, keyRunes("r"))
	m.input.SetValue("")
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if got := m.visible[0].CustomName; got != nil {
		t.Errorf("expected the name to be cleared, got %v", got)
	}
}

// TestRenameEscCancelsWithoutChanging covers Esc leaving the rename prompt
// with no change applied, the same contract every other prompt has.
func TestRenameEscCancelsWithoutChanging(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	if err := annotate.SetName(m.db, m.visible[0].LineageID, "already named"); err != nil {
		t.Fatal(err)
	}
	m.loadAll()

	m = update(t, m, keyRunes("r"))
	m = update(t, m, keyRunes("something else entirely"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeNone {
		t.Errorf("esc did not leave the rename prompt (mode %v)", m.mode)
	}
	if got := m.visible[0].CustomName; got == nil || *got != "already named" {
		t.Errorf("esc should not have changed the name, got %v", got)
	}
}

// TestRenameOfferedFromActionMenuAndHelp covers the discoverability
// requirements: `x` on the Sessions panel offers "rename this session", and
// `?` lists the `r` binding, so the footer and help stay accurate.
func TestRenameOfferedFromActionMenuAndHelp(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("x"))
	labels := make([]string, len(m.menuFiltered))
	for i, a := range m.menuFiltered {
		labels[i] = a.label
	}
	if !contains(labels, "rename this session") {
		t.Errorf("the Sessions menu does not offer rename; it offers %v", labels)
	}

	found := false
	for _, a := range browseActions {
		if a.key == "r" {
			found = true
		}
	}
	if !found {
		t.Error("browseActions does not document the r binding, so ? would not list it")
	}
}

// renderCustomNameFor reads the raw custom_name a lineage has stored, for
// tests asserting SetName's effect independent of what the browser's own
// query happens to have cached.
func renderCustomNameFor(t *testing.T, db *sqlitex.Runner, lineageID string) (*string, error) {
	t.Helper()
	var rows []struct {
		CustomName *string `json:"custom_name"`
	}
	if err := db.Query("SELECT custom_name FROM lineages WHERE id = '"+lineageID+"';", &rows); err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one lineage row for %q, got %d", lineageID, len(rows))
	}
	return rows[0].CustomName, nil
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
	m := newTestBrowser(db, "p", BrowserOptions{Installs: testInstalls("p")})
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
		Style: true, Installs: testInstalls("claude-personal"),
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
	got := renderItemDetail(db, it, nil, noInstallInfo, RenderOptions{Width: 80})
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
// TestRenderItemDetailPrefersCustomNameAndShowsAgentNameSeparately covers
// the effective-name line: lazyrecall's own name (CustomName) is shown as
// "name:", and when the source also recorded a different name, that name
// gets its own "agent name:" line so what the session was renamed away
// from stays visible on both sides.
func TestRenderItemDetailPrefersCustomNameAndShowsAgentNameSeparately(t *testing.T) {
	db := browseTestDB(t)
	customName, sourceName, topic := "my own name", "agent-set name", "derived topic"
	it := search.Item{SessionID: "claude:p:1", Source: "claude", CustomName: &customName, Name: &sourceName, Topic: &topic}
	got := renderItemDetail(db, it, nil, noInstallInfo, RenderOptions{Width: 80})
	if !strings.Contains(got, "name:   "+customName) {
		t.Errorf("the detail pane does not show the custom name as the effective name:\n%s", got)
	}
	if !strings.Contains(got, "agent name: "+sourceName) {
		t.Errorf("the detail pane does not show the source-recorded name on its own line:\n%s", got)
	}
	if !strings.Contains(got, "topic:  "+topic) {
		t.Errorf("the detail pane does not still show the derived topic:\n%s", got)
	}

	// With no custom name set, the source name alone is the effective name
	// and there is no separate "agent name:" line - exactly the pre-existing
	// behaviour (TestRenderItemDetailShowsNameAndTopicSeparately).
	noCustom := search.Item{SessionID: "claude:p:2", Source: "claude", Name: &sourceName, Topic: &topic}
	got = renderItemDetail(db, noCustom, nil, noInstallInfo, RenderOptions{Width: 80})
	if !strings.Contains(got, "name:   "+sourceName) {
		t.Errorf("the detail pane does not fall back to the source name:\n%s", got)
	}
	if strings.Contains(got, "agent name:") {
		t.Errorf("the detail pane should not show a separate agent name line with no custom name set:\n%s", got)
	}

	// A custom name identical to the source name is shown once, not twice.
	same := search.Item{SessionID: "claude:p:3", Source: "claude", CustomName: &sourceName, Name: &sourceName}
	got = renderItemDetail(db, same, nil, noInstallInfo, RenderOptions{Width: 80})
	if strings.Contains(got, "agent name:") {
		t.Errorf("an identical custom name and source name should not duplicate the line:\n%s", got)
	}
}

func TestRenderItemDetailShowsClientWithItsRawValue(t *testing.T) {
	db := browseTestDB(t)
	client := "sdk-ts"
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Client: &client}
	got := renderItemDetail(db, it, nil, noInstallInfo, RenderOptions{Width: 80})
	if !strings.Contains(got, "client: acp (sdk-ts)") {
		t.Errorf("the detail pane does not show the client and what it was read from:\n%s", got)
	}

	// The terminal has no label - the row leaves it out - but the detail
	// pane still states it, because "driven from the terminal" and "the
	// source never said" are different facts and this is the one place
	// with room to tell them apart.
	terminal := "cli"
	got = renderItemDetail(db, search.Item{SessionID: "claude:p:2", Source: "claude", Client: &terminal}, nil, noInstallInfo, RenderOptions{Width: 80})
	if !strings.Contains(got, "client: cli") {
		t.Errorf("the detail pane should still state a terminal session's client:\n%s", got)
	}

	// A source that records no client at all carries no line.
	got = renderItemDetail(db, search.Item{SessionID: "pi:p:3", Source: "pi"}, nil, noInstallInfo, RenderOptions{Width: 80})
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
	for _, want := range []string{"Groups", "Agents", "Repos"} {
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
		Style: true, Installs: testInstalls("claude-personal"),
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

// TestTextFilterMatchesCustomName covers the `/` row filter's coverage of
// lazyrecall's own name (CustomName): since it wins the row's text slot
// over both the source-recorded name and the topic (rowText), it must be
// what the filter matches too, following the same "if it's on the line, it
// filters" rule TestTextFilterMatchesOnlyWhatTheRowShows already covers for
// Name.
func TestTextFilterMatchesCustomName(t *testing.T) {
	renamed := search.Item{
		SessionID: "claude:p:1", Source: "claude", Handle: 1, EndState: "completed",
		CustomName: strp("my own retry-loop notes"),
		Name:       strp("agent-set name"),
		Topic:      strp("Postman collection debugging"),
	}
	if got := (&browseModel{textFilter: "retry-loop"}).applyTextFilter([]search.Item{renamed}); len(got) != 1 {
		t.Errorf("filtering on the custom name kept %d rows, want 1", len(got))
	}
	// The source name and topic are not shown on this row once a custom
	// name is set (rowText precedence), so they must not match either.
	if got := (&browseModel{textFilter: "agent-set"}).applyTextFilter([]search.Item{renamed}); len(got) != 0 {
		t.Errorf("a row displaying the custom name survived a filter on the source name it does not show")
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
	slots := rowSlots(m.visible[0], m.installLabels)
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

// An archived row draws its marker inside the Sessions panel's width budget,
// leaving eleven fewer columns for row text than an ordinary row. The filter
// must use that per-row budget too: a word fitted away to make room for the
// marker cannot explain why the archived row survived a query.
func TestTextFilterDoesNotMatchPastAnArchivedRowsMarkerBudget(t *testing.T) {
	it := search.Item{
		SessionID: "claude:p:1",
		Source:    "claude",
		Handle:    1,
		CWD:       strp("/x"),
		Topic:     strp(strings.Repeat("x", 15) + "marker-only"),
		EndState:  "completed",
		Archived:  true,
	}
	m := &browseModel{width: 60, showAll: true, textFilter: "marker-only"}
	baseWidth := m.sessionRowWidth()
	drawnWidth := maxInt(baseWidth-visibleWidth(" "+archivedMarker), 1)
	if !strings.Contains(matchText(it, baseWidth, nil), m.textFilter) {
		t.Fatalf("fixture's target word does not fit the ordinary %d-column row", baseWidth)
	}
	if strings.Contains(matchText(it, drawnWidth, nil), m.textFilter) {
		t.Fatalf("fixture's target word still fits the archived %d-column row", drawnWidth)
	}
	if got := m.applyTextFilter([]search.Item{it}); len(got) != 0 {
		t.Errorf("archived row survived on text outside its %d-column display budget", drawnWidth)
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
	slots := rowSlots(m.visible[0], m.installLabels)
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

// ---------------------------------------------------------------------
// Half-page scrolling in the detail pane
// ---------------------------------------------------------------------

// Ctrl-D/Ctrl-U used to move the detail pane a single line: the pane is not
// a facet, so the height lookup reported zero for it and the half-page
// distance fell through to its one-line floor. A long transcript is what
// made it obvious.
func TestHalfScreenUsesTheDetailPaneHeight(t *testing.T) {
	m := fixtureBrowser(t)
	m.focus = panelDetail
	inner := m.geometry().detailInner
	if inner < 4 {
		t.Fatalf("detail pane is %d rows at the fixture size; too small for this test to mean anything", inner)
	}
	if got, want := m.halfScreen(), inner/2; got != want {
		t.Errorf("halfScreen on the detail pane = %d, want half of its %d inner rows (%d)", got, inner, want)
	}
}

// ---------------------------------------------------------------------
// J/K in the stacked layout
// ---------------------------------------------------------------------

// When the terminal is too narrow for two columns every panel is stacked
// into one, so J and K have to walk the whole stack. They used to follow a
// table written for the two-column layout, which left Sessions and Detail
// unreachable by J even though they are drawn directly below Tags.
func TestSpatialDownWalksTheWholeStackWhenNarrow(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 24})
	if m.geometry().sidebar {
		t.Fatal("fixture at 60x24 still draws two columns; pick a narrower width")
	}

	m.focus = panelGroups
	want := []panelID{panelAgents, panelRepos, panelTags, panelSessions, panelDetail}
	for _, w := range want {
		m = update(t, m, keyRunes("J"))
		if m.focus != w {
			t.Fatalf("J landed on %v, want %v (walking the stack from Groups)", m.focus, w)
		}
	}
	// And back up again.
	for i := len(want) - 2; i >= 0; i-- {
		m = update(t, m, keyRunes("K"))
		if m.focus != want[i] {
			t.Fatalf("K landed on %v, want %v (walking back up the stack)", m.focus, want[i])
		}
	}
}

// The two-column layout must be unchanged by that: J from Tags stays put,
// because Sessions is in the other column there, not below it.
func TestSpatialDownStopsAtTheColumnEndWhenWide(t *testing.T) {
	m := fixtureBrowser(t)
	if !m.geometry().sidebar {
		t.Fatal("fixture is not in the two-column layout")
	}
	m.focus = panelTags
	m = update(t, m, keyRunes("J"))
	if m.focus != panelTags {
		t.Errorf("J from Tags landed on %v; with two columns Tags is the bottom of its own", m.focus)
	}
}

// ---------------------------------------------------------------------
// d: remove the tag under the cursor
// ---------------------------------------------------------------------

func TestRemoveTagUnderCursor(t *testing.T) {
	m := fixtureBrowser(t)
	// The fixture tags L0 and L2 "wip"; L0 is the newest, so it is the
	// selected row.
	if it := m.current(); it == nil || !hasTag(*it, "wip") {
		t.Fatalf("expected the selected session to carry the fixture tag, got %+v", m.current())
	}
	m = update(t, m, keyRunes("4")) // Tags panel
	if m.focus != panelTags {
		t.Fatalf("focus is %v, want Tags", m.focus)
	}
	m.tags.cursor = 0
	tag := m.tags.rows[0].Value

	m = update(t, m, keyRunes("d"))
	if it := m.current(); it != nil && hasTag(*it, tag) {
		t.Errorf("session still carries #%s after d", tag)
	}
	if !strings.Contains(m.notice, tag) {
		t.Errorf("notice = %q, want it to name the tag that was removed", m.notice)
	}
}

// d elsewhere must not guess which tag was meant - it says where the key
// works instead of removing something the user never pointed at.
func TestRemoveTagUnderCursorOnlyActsInTheTagsPanel(t *testing.T) {
	m := fixtureBrowser(t)
	m.focus = panelSessions
	before := m.current().Tags

	m = update(t, m, keyRunes("d"))
	if got := m.current().Tags; len(got) != len(before) {
		t.Errorf("d outside the Tags panel changed the tags from %v to %v", before, got)
	}
	if !strings.Contains(m.notice, "Tags panel") {
		t.Errorf("notice = %q, want it to say where d works", m.notice)
	}
}

// The Tags panel lists every tag in the profile, so the cursor can easily
// be on one the selected session does not carry. Removing it would report
// success and change nothing, which reads as the key having failed.
func TestRemoveTagUnderCursorSaysWhenTheSessionLacksTheTag(t *testing.T) {
	m := fixtureBrowser(t)
	if err := annotate.AddTag(m.db, "L1", "other-tag"); err != nil {
		t.Fatal(err)
	}
	m.loadAll()
	m = update(t, m, keyRunes("4"))
	for i, r := range m.tags.rows {
		if r.Value == "other-tag" {
			m.tags.cursor = i
		}
	}
	m = update(t, m, keyRunes("d"))
	if !strings.Contains(m.notice, "not tagged") {
		t.Errorf("notice = %q, want it to say the selected session does not carry the tag", m.notice)
	}
}

// ---------------------------------------------------------------------
// The Groups panel (change group-sessions-in-one-index)
// ---------------------------------------------------------------------

func TestGroupsPanelRowsAndCounts(t *testing.T) {
	m := groupFixtureBrowser(t)
	m = update(t, m, keyRunes("1"))
	wantOrder := []string{"", "work", "personal", "archive", "unknown"}
	if len(m.groups_.rows) != len(wantOrder) {
		t.Fatalf("Groups panel has %d rows, want %d (All, work, personal, Archive, Unknown): %+v",
			len(m.groups_.rows), len(wantOrder), m.groups_.rows)
	}
	for i, v := range wantOrder {
		if m.groups_.rows[i].Value != v {
			t.Errorf("row %d = %q, want %q", i, m.groups_.rows[i].Value, v)
		}
	}
	wantCounts := map[string]int{"": 5, "work": 3, "personal": 1, "archive": 1, "unknown": 1}
	for _, r := range m.groups_.rows {
		if r.Count != wantCounts[r.Value] {
			t.Errorf("row %q count = %d, want %d", r.Value, r.Count, wantCounts[r.Value])
		}
	}
}

// groupCountsFixtureBrowser builds a browser over a corpus that exercises
// filter-aware Groups panel counts (change group-sessions-in-one-index, P1
// review fix #4): two agents (claude, pi) and three groups (work, personal,
// sandbox), with sandbox occupied by pi alone so an agent filter can drive
// its count to zero. Each session's topic doubles as a word an "/" text
// narrow can select on: "alpha" for the claude-only pair plus one archived
// claude session, "beta"/"gamma" for everything else. All content is
// hand-written, never real session data.
//
//	work:     W1 claude alpha, W2 pi beta
//	personal: P1 claude alpha, P2 pi beta
//	sandbox:  S1 pi gamma
//	archived: A1 claude alpha, A2 pi beta
func groupCountsFixtureBrowser(t *testing.T) *browseModel {
	t.Helper()
	db := browseTestDB(t)
	seedBrowseSession(t, db, "W1", "claude:p:w1", "p", 1, map[string]any{"source": "claude", "cwd": "/Users/x/work/api", "topic": "alpha", "last_activity_at": 700})
	seedBrowseSession(t, db, "W2", "claude:p:w2", "p", 2, map[string]any{"source": "pi", "cwd": "/Users/x/work/api", "topic": "beta", "last_activity_at": 600})
	seedBrowseSession(t, db, "P1", "claude:p:p1", "p", 3, map[string]any{"source": "claude", "cwd": "/Users/x/personal/proj", "topic": "alpha", "last_activity_at": 500})
	seedBrowseSession(t, db, "P2", "claude:p:p2", "p", 4, map[string]any{"source": "pi", "cwd": "/Users/x/personal/proj", "topic": "beta", "last_activity_at": 400})
	seedBrowseSession(t, db, "S1", "claude:p:s1", "p", 5, map[string]any{"source": "pi", "cwd": "/Users/x/sandbox/proj", "topic": "gamma", "last_activity_at": 300})
	seedBrowseSession(t, db, "A1", "claude:p:a1", "p", 6, map[string]any{"source": "claude", "cwd": "/Users/x/work/api", "topic": "alpha", "last_activity_at": 200})
	seedBrowseSession(t, db, "A2", "claude:p:a2", "p", 7, map[string]any{"source": "pi", "cwd": "/Users/x/personal/proj", "topic": "beta", "last_activity_at": 100})
	seedLineageOverride(t, db, "A1", "", true)
	seedLineageOverride(t, db, "A2", "", true)

	groups := []config.Group{
		{Name: "work", Paths: []string{"/Users/x/work"}},
		{Name: "personal", Paths: []string{"/Users/x/personal"}},
		{Name: "sandbox", Paths: []string{"/Users/x/sandbox"}},
	}
	return newTestBrowser(db, "p", BrowserOptions{
		Groups:   groups,
		Installs: testInstalls("claude", "pi"),
	})
}

// TestGroupsPanelCountsRespectAgentFilter covers the core of review finding
// #4: the Groups panel used to count over the whole index (search.Counts),
// ignoring the active Agent/Repo/Tag/text selections that already narrow
// every other panel on screen. With agent=claude selected, every group and
// Archive row must drop to claude's own sessions, not the fixture's true
// (agent-blind) totals - this also covers "Archive counts respect the
// other facets", since claude occupies only one of the two archived
// sessions.
func TestGroupsPanelCountsRespectAgentFilter(t *testing.T) {
	m := groupCountsFixtureBrowser(t)
	m.agents.Sel = "claude"
	m.rebuild()

	wantCounts := map[string]int{"": 2, "work": 1, "personal": 1, "archive": 1}
	for value, want := range wantCounts {
		if got := rowCount(t, m.groups_, value); got != want {
			t.Errorf("row %q count = %d under agent=claude, want %d", value, got, want)
		}
	}
	// sandbox is pi-only, so under agent=claude it has zero matching
	// sessions and (not being the selected row) must not be advertised at
	// all - the zero-count convention every other facet panel follows.
	for _, r := range m.groups_.rows {
		if r.Value == "sandbox" {
			t.Fatalf("sandbox row shown with a zero count under agent=claude: %+v", m.groups_.rows)
		}
	}
}

// TestGroupsPanelZeroCountRowStaysWhenSelected covers "a group with no
// matching sessions under the filter is not advertised, unless it's the
// selected row": TestGroupsPanelCountsRespectAgentFilter already shows
// sandbox is hidden under agent=claude when nothing has it selected: this
// test selects sandbox itself first and confirms the row survives the same
// narrowing that would otherwise hide it, at its true zero count, so the
// selection stays reachable to clear.
func TestGroupsPanelZeroCountRowStaysWhenSelected(t *testing.T) {
	m := groupCountsFixtureBrowser(t)
	m.setGroupFilter("sandbox")
	m.agents.Sel = "claude"
	m.rebuild()

	found := false
	for _, r := range m.groups_.rows {
		if r.Value == "sandbox" {
			found = true
			if r.Count != 0 {
				t.Errorf("sandbox row count = %d under agent=claude, want 0", r.Count)
			}
		}
	}
	if !found {
		t.Fatalf("sandbox row missing even though it is the selected group: %+v", m.groups_.rows)
	}
}

// TestGroupsPanelCountsRespectTextFilter covers "with a text narrow active,
// counts follow it": the "/" filter (m.textFilter) narrows Groups panel
// counts the same way it already narrows Agents/Repos/Tags.
func TestGroupsPanelCountsRespectTextFilter(t *testing.T) {
	m := groupCountsFixtureBrowser(t)
	m.textFilter = "alpha" // only W1, P1 and the archived A1 carry this topic
	m.rebuild()

	wantCounts := map[string]int{"": 2, "work": 1, "personal": 1, "archive": 1}
	for value, want := range wantCounts {
		if got := rowCount(t, m.groups_, value); got != want {
			t.Errorf("row %q count = %d under text filter %q, want %d", value, got, m.textFilter, want)
		}
	}
	for _, r := range m.groups_.rows {
		if r.Value == "sandbox" {
			t.Fatalf("sandbox row shown with a zero count under text filter %q: %+v", m.textFilter, m.groups_.rows)
		}
	}
}

// TestSelectingAGroupStillFiltersTheList is the sanity check behind moving
// the Groups panel's counts onto a client-side superset (m.groupItems):
// the Sessions list itself must still narrow to exactly the selected
// group's sessions, unaffected by however its count is now computed.
func TestSelectingAGroupStillFiltersTheList(t *testing.T) {
	m := groupCountsFixtureBrowser(t)
	m.setGroupFilter("sandbox")

	if len(m.visible) != 1 || m.visible[0].SessionID != "claude:p:s1" {
		t.Fatalf("visible = %+v, want exactly the one sandbox session", m.visible)
	}
}

// TestGroupsPanelHidesUnknownRowWhenEmpty covers the other half of the
// panel's row-selection rule (TestGroupsPanelRowsAndCounts covers the case
// where Unknown is non-empty and shown): a row that leads nowhere is never
// offered, the same rule every other facet's rows follow.
func TestGroupsPanelHidesUnknownRowWhenEmpty(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "U0", "claude:p:u0", "p", 1, map[string]any{"cwd": "/Users/x/work/api"})
	groups := []config.Group{{Name: "work", Paths: []string{"/Users/x/work"}}}
	m := newTestBrowser(db, "p", BrowserOptions{Groups: groups, Installs: testInstalls("claude")})
	m = update(t, m, keyRunes("1"))
	for _, r := range m.groups_.rows {
		if r.Value == "unknown" {
			t.Fatalf("Unknown row shown with a zero count: %+v", m.groups_.rows)
		}
	}
}

// TestGroupsPanelZeroConfigShowsAllPlusArchiveWhenArchived is
// TestAllRowIsAlwaysMarkedInTheGroupsPanel's sibling, checking row content
// rather than the marker. With no groups configured and nothing archived,
// the browser looks exactly as it did before groups existed (plan,
// "Zero-config and public users"): just All. But archived sessions still
// need a row to reach them even with zero groups configured, since Archive
// is not a group - so as soon as something is archived, the panel also
// offers Archive (with the right count) alongside All, and Enter on it
// filters to only the archived sessions. Unknown stays hidden either way:
// with no groups configured, every non-archived session is Unknown, so an
// Unknown row would only duplicate All.
func TestGroupsPanelZeroConfigShowsAllPlusArchiveWhenArchived(t *testing.T) {
	t.Run("nothing archived", func(t *testing.T) {
		m := fixtureBrowser(t)
		m = update(t, m, keyRunes("1"))
		if len(m.groups_.rows) != 1 || m.groups_.rows[0].Value != "" {
			t.Errorf("zero-config Groups panel rows = %+v, want exactly one All row", m.groups_.rows)
		}
	})

	t.Run("something archived", func(t *testing.T) {
		db := browseTestDB(t)
		seedFixture(t, db)
		seedLineageOverride(t, db, "L0", "", true)
		seedLineageOverride(t, db, "L1", "", true)
		m := newTestBrowser(db, "p", BrowserOptions{
			Installs: testInstalls("claude", "pi", "omp"),
		})
		m = update(t, m, keyRunes("1"))

		if len(m.groups_.rows) != 2 {
			t.Fatalf("zero-config Groups panel rows = %+v, want All and Archive only", m.groups_.rows)
		}
		if m.groups_.rows[0].Value != "" || m.groups_.rows[0].Label != "All" {
			t.Errorf("first row = %+v, want All", m.groups_.rows[0])
		}
		if m.groups_.rows[1].Value != "archive" || m.groups_.rows[1].Label != "Archive" || m.groups_.rows[1].Count != 2 {
			t.Errorf("second row = %+v, want Archive with count 2", m.groups_.rows[1])
		}

		for i, r := range m.groups_.rows {
			if r.Value == "archive" {
				m.groups_.cursor = i
			}
		}
		m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if len(m.visible) != 2 {
			t.Fatalf("Archive row shows %+v, want only the 2 archived sessions", m.visible)
		}
		for _, it := range m.visible {
			if it.SessionID != "claude:p:s0" && it.SessionID != "claude:p:s1" {
				t.Errorf("Archive row shows unarchived session %s", it.SessionID)
			}
		}
	})
}

func TestEnterOnGroupsPanelFiltersSessionList(t *testing.T) {
	m := groupFixtureBrowser(t)
	m = update(t, m, keyRunes("1"))
	for i, r := range m.groups_.rows {
		if r.Value == "work" {
			m.groups_.cursor = i
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.group != "work" {
		t.Fatalf("group filter is %q, want work", m.group)
	}
	if len(m.visible) != 3 {
		t.Fatalf("%d sessions visible under group=work, want 3", len(m.visible))
	}
	for _, it := range m.visible {
		if it.Group != "work" {
			t.Errorf("session %s has group %q, want work", it.SessionID, it.Group)
		}
	}
}

func TestEscOnGroupsPanelReturnsToAll(t *testing.T) {
	m := groupFixtureBrowser(t)
	m = update(t, m, keyRunes("1"))
	for i, r := range m.groups_.rows {
		if r.Value == "personal" {
			m.groups_.cursor = i
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.group != "personal" {
		t.Fatalf("group filter is %q, want personal", m.group)
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.group != "" {
		t.Errorf("esc left group filter at %q, want cleared", m.group)
	}
	if len(m.visible) != 5 {
		t.Errorf("%d sessions visible after esc, want all 5 non-archived sessions", len(m.visible))
	}
}

func TestArchiveGroupRowListsOnlyArchivedSessions(t *testing.T) {
	m := groupFixtureBrowser(t)
	m = update(t, m, keyRunes("1"))
	for i, r := range m.groups_.rows {
		if r.Value == "archive" {
			m.groups_.cursor = i
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 1 || m.visible[0].SessionID != "claude:p:g4" {
		t.Fatalf("Archive row shows %+v, want only the archived session", m.visible)
	}
}

// ---------------------------------------------------------------------
// The `p` popup (change group-sessions-in-one-index)
// ---------------------------------------------------------------------

// TestGroupPopupMarksTheCurrentChoice covers the three states
// groupMenuActions distinguishes: a manual override, an archived session
// with no override (GroupManual wins when both are somehow true, but
// neither of these fixtures is), and neither (automatic).
func TestGroupPopupMarksTheCurrentChoice(t *testing.T) {
	m := groupFixtureBrowser(t)
	items, err := search.List(m.db, search.Filter{Groups: m.groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]search.Item{}
	for _, it := range items {
		byID[it.SessionID] = it
	}

	cases := []struct {
		name, sessionID, want string
	}{
		{"manual override", "claude:p:g5", "work"},
		{"archived, no override", "claude:p:g4", "Archive"},
		{"automatic", "claude:p:g0", "Automatic (work)"},
	}
	for _, c := range cases {
		it, ok := byID[c.sessionID]
		if !ok {
			t.Fatalf("%s: fixture is missing session %s", c.name, c.sessionID)
		}
		actions := m.groupMenuActions(it)
		idx := currentGroupMenuIndex(actions)
		if !strings.HasPrefix(actions[idx].label, "● ") {
			t.Errorf("%s: marked entry %q has no applied marker", c.name, actions[idx].label)
		}
		if !strings.Contains(actions[idx].label, c.want) {
			t.Errorf("%s: marked entry is %q, want it to name %q", c.name, actions[idx].label, c.want)
		}
	}
}

// TestGroupPopupZeroConfigOffersOnlyArchiveAndAutomatic covers the other
// zero-config requirement: with no groups configured, `p` offers only the
// two entries every session always has, never a phantom configured group.
func TestGroupPopupZeroConfigOffersOnlyArchiveAndAutomatic(t *testing.T) {
	m := fixtureBrowser(t)
	m = update(t, m, keyRunes("0"))
	m = update(t, m, keyRunes("p"))
	if m.mode != modeMenu {
		t.Fatalf("p did not open the popup (mode %v)", m.mode)
	}
	if len(m.menuFiltered) != 2 {
		t.Fatalf("zero-config popup has %d entries, want 2 (Archive, Automatic): %+v", len(m.menuFiltered), m.menuFiltered)
	}
	joined := m.menuFiltered[0].label + "|" + m.menuFiltered[1].label
	if !strings.Contains(joined, "Archive") || !strings.Contains(joined, "Automatic") {
		t.Errorf("zero-config popup entries are %q, want Archive and Automatic", joined)
	}
}

// TestGroupPopupAppliesChoiceAndMovesTheSessionBetweenGroups drives the
// popup through the real key-event loop end to end: open with `p`, move to
// an entry (Up/Down - typing in this popup narrows by substring, as it
// does in the `x` menu it reuses), apply with Enter, then checks the
// persisted database state and that the session moved between group
// filters.
func TestGroupPopupAppliesChoiceAndMovesTheSessionBetweenGroups(t *testing.T) {
	m := groupFixtureBrowser(t)
	idx := -1
	for i, it := range m.visible {
		if it.SessionID == "claude:p:g0" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("fixture session g0 not found in %+v", m.visible)
	}
	m.cursor = idx

	m = update(t, m, keyRunes("p"))
	if m.mode != modeMenu {
		t.Fatalf("p did not open the group popup (mode %v)", m.mode)
	}
	if m.menuTitle != "Session group" {
		t.Errorf("popup title = %q, want it to name the group popup, not the focused panel", m.menuTitle)
	}
	if !strings.Contains(m.menuFiltered[m.menuCursor].label, "Automatic (work)") {
		t.Errorf("popup opened on %q, want the Automatic entry marked and pre-selected", m.menuFiltered[m.menuCursor].label)
	}

	personalIdx := -1
	for i, a := range m.menuFiltered {
		if strings.Contains(a.label, "personal") {
			personalIdx = i
		}
	}
	if personalIdx < 0 {
		t.Fatalf("popup has no personal entry: %+v", m.menuFiltered)
	}
	for m.menuCursor < personalIdx {
		m = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	for m.menuCursor > personalIdx {
		m = update(t, m, tea.KeyMsg{Type: tea.KeyUp})
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNone {
		t.Errorf("applying a group choice left mode %v", m.mode)
	}
	if !strings.Contains(m.notice, "personal") {
		t.Errorf("notice = %q, want it to mention personal", m.notice)
	}

	items, err := search.List(m.db, search.Filter{Groups: m.groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	var got search.Item
	for _, it := range items {
		if it.SessionID == "claude:p:g0" {
			got = it
		}
	}
	if got.Group != "personal" || !got.GroupManual {
		t.Errorf("g0 is group=%q manual=%v after the popup, want personal/manual", got.Group, got.GroupManual)
	}

	m.setGroupFilter("personal")
	found := false
	for _, it := range m.visible {
		if it.SessionID == "claude:p:g0" {
			found = true
		}
	}
	if !found {
		t.Error("g0 does not appear under group=personal after being filed there")
	}
	m.setGroupFilter("work")
	for _, it := range m.visible {
		if it.SessionID == "claude:p:g0" {
			t.Error("g0 still appears under group=work after being filed under personal")
		}
	}
}

func TestGroupPopupEscChangesNothing(t *testing.T) {
	m := groupFixtureBrowser(t)
	idx := -1
	for i, it := range m.visible {
		if it.SessionID == "claude:p:g0" {
			idx = i
		}
	}
	m.cursor = idx
	before, err := search.List(m.db, search.Filter{Groups: m.groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}

	m = update(t, m, keyRunes("p"))
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNone {
		t.Errorf("esc left the popup open (mode %v)", m.mode)
	}

	after, err := search.List(m.db, search.Filter{Groups: m.groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	var b, a search.Item
	for _, it := range before {
		if it.SessionID == "claude:p:g0" {
			b = it
		}
	}
	for _, it := range after {
		if it.SessionID == "claude:p:g0" {
			a = it
		}
	}
	if a.Group != b.Group || a.GroupManual != b.GroupManual || a.Archived != b.Archived {
		t.Errorf("esc changed g0's group state: before %+v after %+v", b, a)
	}
}

// ---------------------------------------------------------------------
// The Agents panel facets by install (change group-sessions-in-one-index)
// ---------------------------------------------------------------------

func TestAgentsPanelListsInstallsByLabelAndFiltersByInstall(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "A0", "claude:cc:1", "cc", 1, map[string]any{"install": "cc", "cwd": "/x"})
	seedBrowseSession(t, db, "A1", "claude:ccp:1", "ccp", 2, map[string]any{"install": "ccp", "cwd": "/y"})
	m := newTestBrowser(db, "p", BrowserOptions{
		Installs:      testInstalls("cc", "ccp"),
		InstallLabels: map[string]string{"cc": "cc-label", "ccp": "ccp-label"},
	})
	m = update(t, m, keyRunes("2"))

	var got []string
	for _, r := range m.agents.rows {
		got = append(got, r.Label)
	}
	joined := strings.Join(got, "|")
	for _, want := range []string{"cc-label", "ccp-label"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Agents panel rows are %v, want the label %q present", got, want)
		}
	}

	for i, r := range m.agents.rows {
		if r.Value == "cc" {
			m.agents.cursor = i
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 1 || m.visible[0].SessionID != "claude:cc:1" {
		t.Errorf("filtering by install cc gave %+v, want only claude:cc:1", m.visible)
	}
}

// ---------------------------------------------------------------------
// Detail: install and group lines (change group-sessions-in-one-index)
// ---------------------------------------------------------------------

func TestDetailShowsInstallAndGroupLines(t *testing.T) {
	db := browseTestDB(t)
	groups := []config.Group{{Name: "work", Paths: []string{"/Users/x/work"}}}
	seedBrowseSession(t, db, "D0", "claude:acct:1", "acct", 1, map[string]any{"install": "acct", "cwd": "/Users/x/work/api"})
	m := newTestBrowser(db, "acct", BrowserOptions{
		Groups: groups,
		Installs: func() []profile.Profile {
			return []profile.Profile{{Name: "acct", Roots: map[string]string{"claude": "/home/me/.claude-acct"}}}
		},
		InstallLabels: map[string]string{"acct": "cc"},
	})
	it := m.visible[0]
	got := renderItemDetail(m.db, it, m.groups, m.installInfo, RenderOptions{Width: 100})
	if !strings.Contains(got, "install: cc (") || !strings.Contains(got, ".claude-acct)") {
		t.Errorf("detail pane does not show the install label and its root:\n%s", got)
	}
	if !strings.Contains(got, "group:  work (path") {
		t.Errorf("detail pane does not show the group and its path provenance:\n%s", got)
	}
}

func TestDetailGroupLineVariants(t *testing.T) {
	m := groupFixtureBrowser(t)
	items, err := search.List(m.db, search.Filter{Groups: m.groups, ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]search.Item{}
	for _, it := range items {
		byID[it.SessionID] = it
	}

	manual := renderItemDetail(m.db, byID["claude:p:g5"], m.groups, m.installInfo, RenderOptions{Width: 100})
	if !strings.Contains(manual, "group:  work (set manually)") {
		t.Errorf("manual group line wrong:\n%s", manual)
	}
	unknown := renderItemDetail(m.db, byID["claude:p:g3"], m.groups, m.installInfo, RenderOptions{Width: 100})
	if !strings.Contains(unknown, "group:  unknown") {
		t.Errorf("unknown group line wrong:\n%s", unknown)
	}
}

func TestDetailNoGroupLineWithZeroConfig(t *testing.T) {
	m := fixtureBrowser(t)
	it := m.visible[0]
	got := renderItemDetail(m.db, it, m.groups, m.installInfo, RenderOptions{Width: 100})
	if strings.Contains(got, "group:") {
		t.Errorf("detail pane shows a group line with no groups configured:\n%s", got)
	}
}

// ---------------------------------------------------------------------
// Transcript tab: install resolution (change group-sessions-in-one-index)
// ---------------------------------------------------------------------

// TestInstallForResolvesTheSessionsOwnInstall covers what the Transcript
// tab (and now the Detail tab's install line) depends on for a
// database-backed source like hermes: the install embedded in the
// session's own composite id, never "the profile being browsed" - a notion
// that no longer exists once one browsing session covers every install's
// data together.
func TestInstallForResolvesTheSessionsOwnInstall(t *testing.T) {
	m := newTestBrowser(browseTestDB(t), "unused", BrowserOptions{
		Installs: testInstalls("claude-personal", "hermes-work"),
	})
	it := search.Item{SessionID: "hermes:hermes-work:abc123", Source: "hermes"}
	p, err := m.installFor(it)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "hermes-work" {
		t.Errorf("installFor resolved %q, want hermes-work (the session's own install)", p.Name)
	}
}

// TestAgentsPanelInstallSelectionDoesNotLeakSiblingInstallSessions is the
// regression test for a real bug: matching an Agents panel selection
// against "it.Install == v OR it.Source == v" is wrong whenever one
// install is literally named the same as its own multi-install source -
// internal/profile.installName's ordinary result for a primary claude root -
// because the source half of that OR
// matches every install of the source, silently widening "this one
// install" back out to "every install of it". Two installs both sourced
// from "claude" - one of them named "claude" itself - is exactly that
// shape.
func TestAgentsPanelInstallSelectionDoesNotLeakSiblingInstallSessions(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "W0", "claude:claude:1", "claude", 1, map[string]any{"install": "claude", "cwd": "/x/work"})
	seedBrowseSession(t, db, "W1", "claude:claude:2", "claude", 2, map[string]any{"install": "claude", "cwd": "/x/work"})
	seedBrowseSession(t, db, "W2", "claude:claude-personal:1", "claude-personal", 3, map[string]any{"install": "claude-personal", "cwd": "/x/work"})
	labels := map[string]string{"claude": "cc", "claude-personal": "ccp"}

	m := newTestBrowser(db, "p", BrowserOptions{
		Installs:      testInstalls("claude", "claude-personal"),
		InstallLabels: labels,
	})
	m = update(t, m, keyRunes("2"))
	for i, r := range m.agents.rows {
		if r.Value == "claude" {
			m.agents.cursor = i
		}
	}
	m = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.visible) != 2 {
		t.Fatalf("selecting the claude install shows %d sessions, want 2 (claude-personal must not leak in): %+v", len(m.visible), m.visible)
	}
	for _, it := range m.visible {
		if it.Install != "claude" {
			t.Errorf("session %s has install %q, want claude", it.SessionID, it.Install)
		}
	}

	// A bare source seed (--agent=claude -> resolveAgentFilter's Agent
	// return, spanning every install of the source) is the one case that
	// IS supposed to show every claude session, via the separate
	// agentSource dimension.
	src := newTestBrowser(db, "p", BrowserOptions{
		Agent: "claude", Installs: testInstalls("claude", "claude-personal"), InstallLabels: labels,
	})
	if len(src.visible) != 3 {
		t.Errorf("--agent=claude shows %d sessions, want all 3", len(src.visible))
	}

	// A label seed (--agent=cc / --agent=ccp -> resolveAgentFilter's
	// Install return) narrows to the one install it names.
	cc := newTestBrowser(db, "p", BrowserOptions{
		Install: "claude", Installs: testInstalls("claude", "claude-personal"), InstallLabels: labels,
	})
	if len(cc.visible) != 2 {
		t.Errorf("--agent=cc shows %d sessions, want 2", len(cc.visible))
	}
	ccp := newTestBrowser(db, "p", BrowserOptions{
		Install: "claude-personal", Installs: testInstalls("claude", "claude-personal"), InstallLabels: labels,
	})
	if len(ccp.visible) != 1 {
		t.Errorf("--agent=ccp shows %d sessions, want 1", len(ccp.visible))
	}
}

// TestSessionsPanelUsesInstallLabelsInBadges is the regression test for a
// real bug: RenderRow itself honours RenderOptions.InstallLabels (row_test.go
// covers that directly), but the Sessions panel built its own RenderOptions
// per row without copying installLabels onto it, so every row kept showing
// the bare source ("[claude]") no matter what BrowserOptions.InstallLabels
// carried. This renders through sessionsPanel - the actual draw path - not
// RenderRow in isolation, so it would have caught the gap.
func TestSessionsPanelUsesInstallLabelsInBadges(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "B0", "claude:claude:1", "claude", 1, map[string]any{"install": "claude", "cwd": "/x", "client": "sdk-ts"})
	seedBrowseSession(t, db, "B1", "claude:claude-personal:1", "claude-personal", 2, map[string]any{"install": "claude-personal", "cwd": "/y"})
	m := newTestBrowser(db, "p", BrowserOptions{
		Installs:      testInstalls("claude", "claude-personal"),
		InstallLabels: map[string]string{"claude": "cc", "claude-personal": "ccp"},
	})
	g := m.geometry()
	box := m.sessionsPanel(g)
	view := strings.Join(box.Lines, "\n")
	if !strings.Contains(view, "[cc·acp]") {
		t.Errorf("Sessions panel does not show the labelled+client badge:\n%s", view)
	}
	if !strings.Contains(view, "[ccp]") {
		t.Errorf("Sessions panel does not show the labelled badge:\n%s", view)
	}
	if strings.Contains(view, "[claude]") || strings.Contains(view, "[claude·acp]") {
		t.Errorf("Sessions panel still shows the bare source instead of the install label:\n%s", view)
	}
}

// TestSessionsPanelColorsHandleByEffectiveGroup covers change
// per-group-colors: a session's handle is drawn in its effective group's
// configured color, and the selected row - drawn in reverse video - still
// carries that color, since highlightLine re-applies reverse after every
// field's own reset (TestHighlightLineReappliesReverseAfterFieldResets) the
// same way it always has for the default bold-cyan handle.
func TestSessionsPanelColorsHandleByEffectiveGroup(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "W0", "claude:p:w0", "p", 1, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 600})
	seedBrowseSession(t, db, "U0", "claude:p:u0", "p", 2, map[string]any{"cwd": "/tmp", "last_activity_at": 500})

	groups := []config.Group{{Name: "work", Paths: []string{"/Users/x/work"}, Color: "blue"}}
	m := newTestBrowser(db, "p", BrowserOptions{
		Groups:   groups,
		Installs: testInstalls("claude"),
		Style:    true,
	})
	if len(m.visible) < 2 || m.visible[0].SessionID != "claude:p:w0" {
		t.Fatalf("fixture ordering assumption broken, m.visible = %+v", m.visible)
	}
	m.cursor = 0 // the work-group session, most recently active
	g := m.geometry()
	box := m.sessionsPanel(g)

	wantCode := "\x1b[34m" // ansi blue
	selectedLine := box.Lines[0]
	if !strings.Contains(selectedLine, ansiReverse) {
		t.Errorf("selected row is not drawn in reverse video: %q", selectedLine)
	}
	if !strings.Contains(selectedLine, wantCode) {
		t.Errorf("selected row lost its group color: %q", selectedLine)
	}

	otherLine := box.Lines[1]
	if strings.Contains(otherLine, wantCode) {
		t.Errorf("the Unknown-group session's row carries work's color: %q", otherLine)
	}
}

// TestGroupsPanelShowsAllRowsWhenTheColumnHasRoom is the regression test
// for a real bug: the roomy left-column layout capped the Groups panel at
// 4 content rows (boxHeight's old maxRows), so a config with 2 groups
// (All, work, personal, Archive, Unknown - 5 rows) always scrolled Unknown
// out of view even at a generous terminal size, while Tags below it had
// empty lines to spare. maxGroupsRows is meant to be large enough that a
// small, config-bounded row count never gets capped this way.
func TestGroupsPanelShowsAllRowsWhenTheColumnHasRoom(t *testing.T) {
	m := groupFixtureBrowser(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 170, Height: 42})
	if len(m.groups_.rows) != 5 {
		t.Fatalf("fixture has %d Groups rows, want 5 (All, work, personal, Archive, Unknown)", len(m.groups_.rows))
	}
	g := m.geometry()
	box := m.facetPanel(panelGroups, &m.groups_, g.leftWidth, g.groupsH, m.group)
	if len(box.Lines) != 5 {
		t.Errorf("Groups panel draws %d of 5 rows at 170x42 (groupsH=%d): %+v", len(box.Lines), g.groupsH, box.Lines)
	}
	if !strings.Contains(strings.Join(box.Lines, "\n"), "Unknown") {
		t.Errorf("Unknown row is not drawn even with room: %+v", box.Lines)
	}
}

// TestGroupsPanelColorsConfiguredGroupNames covers change per-group-colors:
// a configured group's own name in the Groups panel is drawn in its color,
// while a group with no configured color and the All/Archive/Unknown rows
// all keep today's default styling.
func TestGroupsPanelColorsConfiguredGroupNames(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "C0", "claude:p:c0", "p", 1, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 600})
	seedBrowseSession(t, db, "C1", "claude:p:c1", "p", 2, map[string]any{"cwd": "/Users/x/personal/proj", "last_activity_at": 500})

	groups := []config.Group{
		{Name: "work", Paths: []string{"/Users/x/work"}, Color: "blue"},
		{Name: "personal", Paths: []string{"/Users/x/personal"}}, // no color configured
	}
	m := newTestBrowser(db, "p", BrowserOptions{
		Groups:   groups,
		Installs: testInstalls("claude"),
		Style:    true,
	})
	m = update(t, m, tea.WindowSizeMsg{Width: 170, Height: 42})
	g := m.geometry()
	box := m.facetPanel(panelGroups, &m.groups_, g.leftWidth, g.groupsH, m.group)

	var workLine, personalLine, allLine string
	for _, l := range box.Lines {
		switch {
		case strings.Contains(l, "work"):
			workLine = l
		case strings.Contains(l, "personal"):
			personalLine = l
		case strings.Contains(l, "All"):
			allLine = l
		}
	}
	if workLine == "" || personalLine == "" || allLine == "" {
		t.Fatalf("Groups panel missing an expected row: %+v", box.Lines)
	}
	wantCode := "\x1b[34m" // ansi blue
	if !strings.Contains(workLine, wantCode) {
		t.Errorf("work row does not carry its configured color %q: %q", wantCode, workLine)
	}
	if strings.Contains(personalLine, wantCode) {
		t.Errorf("personal row (no configured color) carries work's color: %q", personalLine)
	}
	if strings.Contains(allLine, wantCode) {
		t.Errorf("All row carries a group's color: %q", allLine)
	}
}

// TestGroupsPanelColorsArchiveAndUnknownRows covers change
// archive-unknown-colors: the Archive and Unknown rows in the Groups panel
// are drawn in their own configured colors exactly like a real group's own
// name already is (TestGroupsPanelColorsConfiguredGroupNames), and default
// (no escape code) when neither is configured.
func TestGroupsPanelColorsArchiveAndUnknownRows(t *testing.T) {
	db := browseTestDB(t)
	seedBrowseSession(t, db, "G0", "claude:p:g0", "p", 1, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 600})
	seedBrowseSession(t, db, "G1", "claude:p:g1", "p", 2, map[string]any{"cwd": "/tmp", "last_activity_at": 500}) // Unknown
	seedBrowseSession(t, db, "G2", "claude:p:g2", "p", 3, map[string]any{"cwd": "/Users/x/work/api", "last_activity_at": 400})
	seedLineageOverride(t, db, "G2", "", true) // archived, counted under Archive

	groups := []config.Group{{Name: "work", Paths: []string{"/Users/x/work"}}}
	archiveCode, unknownCode := "\x1b[31m", "\x1b[32m" // ansi red, ansi green

	rowsByLabel := func(m *browseModel) map[string]string {
		m = update(t, m, tea.WindowSizeMsg{Width: 170, Height: 42})
		g := m.geometry()
		box := m.facetPanel(panelGroups, &m.groups_, g.leftWidth, g.groupsH, m.group)
		out := map[string]string{}
		for _, l := range box.Lines {
			switch {
			case strings.Contains(l, "Archive"):
				out["archive"] = l
			case strings.Contains(l, "Unknown"):
				out["unknown"] = l
			case strings.Contains(l, "All"):
				out["all"] = l
			}
		}
		return out
	}

	colored := newTestBrowser(db, "p", BrowserOptions{
		Groups:       groups,
		ArchiveColor: "red",
		UnknownColor: "green",
		Installs:     testInstalls("claude"),
		Style:        true,
	})
	lines := rowsByLabel(colored)
	if lines["archive"] == "" || lines["unknown"] == "" || lines["all"] == "" {
		t.Fatalf("Groups panel missing an expected row: %+v", lines)
	}
	if !strings.Contains(lines["archive"], archiveCode) {
		t.Errorf("Archive row does not carry its configured color %q: %q", archiveCode, lines["archive"])
	}
	if !strings.Contains(lines["unknown"], unknownCode) {
		t.Errorf("Unknown row does not carry its configured color %q: %q", unknownCode, lines["unknown"])
	}
	if strings.Contains(lines["all"], archiveCode) || strings.Contains(lines["all"], unknownCode) {
		t.Errorf("All row carries Archive/Unknown's color: %q", lines["all"])
	}

	uncolored := newTestBrowser(db, "p", BrowserOptions{
		Groups:   groups,
		Installs: testInstalls("claude"),
		Style:    true,
	})
	lines = rowsByLabel(uncolored)
	// The row still carries the count's own dim styling (like any other
	// row); what must be absent with nothing configured is specifically the
	// archive/unknown color codes above.
	if strings.Contains(lines["archive"], archiveCode) {
		t.Errorf("Archive row carries a color with none configured: %q", lines["archive"])
	}
	if strings.Contains(lines["unknown"], unknownCode) {
		t.Errorf("Unknown row carries a color with none configured: %q", lines["unknown"])
	}
}

// TestGroupsPanelAllCountWithZeroConfig is the regression test for a real
// bug: with no groups configured, the All row's Count was left at its zero
// value ("All  0") because groupRows' zero-groups branch returned before
// ever touching the panel's own counts, while the very same fixture's
// Sessions list and the Agents panel's own "all agents" row both showed the
// true count. All's count must equal the number of sessions the All view
// actually lists, computed from the same source (m.groupItems, via loadAll)
// whether or not any group is configured. The fixture also archives one
// session, so this doubles as coverage that the Archive row's own count is
// unaffected by whatever fixed the All row (change group-sessions-in-one-
// index's zero-config-plus-archive rule, TestGroupsPanelZeroConfigShows-
// AllPlusArchiveWhenArchived covers that row's presence and behavior
// directly).
func TestGroupsPanelAllCountWithZeroConfig(t *testing.T) {
	db := browseTestDB(t)
	for i := 0; i < 4; i++ {
		seedBrowseSession(t, db, fmt.Sprintf("Z%d", i), fmt.Sprintf("claude:p:z%d", i), "p", i+1,
			map[string]any{"cwd": "/x", "last_activity_at": int64(1000 - i)})
	}
	seedBrowseSession(t, db, "Z4", "claude:p:z4", "p", 5, map[string]any{"cwd": "/x", "last_activity_at": 500})
	seedLineageOverride(t, db, "Z4", "", true) // archived, excluded from All

	m := newTestBrowser(db, "p", BrowserOptions{Installs: testInstalls("claude")})
	m = update(t, m, keyRunes("1"))
	if len(m.groups_.rows) != 2 || m.groups_.rows[0].Value != "" || m.groups_.rows[1].Value != "archive" {
		t.Fatalf("zero-config Groups panel rows = %+v, want All then Archive", m.groups_.rows)
	}
	if m.groups_.rows[0].Count != 4 {
		t.Errorf("All row count = %d, want 4 (5 sessions minus 1 archived)", m.groups_.rows[0].Count)
	}
	if m.groups_.rows[0].Count != len(m.visible) {
		t.Errorf("All row count (%d) does not match the Sessions list length (%d)", m.groups_.rows[0].Count, len(m.visible))
	}
	if m.groups_.rows[1].Count != 1 {
		t.Errorf("Archive row count = %d, want 1", m.groups_.rows[1].Count)
	}
}

// ---------------------------------------------------------------------
// Date separator rows in the Sessions panel (change date-separator-rows)
// ---------------------------------------------------------------------

// timep is dateHeaderItem's pointer helper, the *time.Time counterpart of
// pick_test.go's strp.
func timep(t time.Time) *time.Time { return &t }

// dateHeaderItem builds a minimal search.Item for these tests: only the
// fields sessionDisplayRows and RenderRow actually read. EndState is filled
// in because RenderRow always draws a state slot regardless of what the test
// cares about.
func dateHeaderItem(handle int, at *time.Time) search.Item {
	return search.Item{
		SessionID:      fmt.Sprintf("claude:p:%d", handle),
		Source:         "claude",
		Handle:         handle,
		EndState:       "completed",
		LastActivityAt: at,
	}
}

// buildBucketedItems returns perBucket sessions in each of the six buckets -
// Today, Yesterday, Last 7 days, Last 30 days, Older, then perBucket more
// with no LastActivityAt at all - already in the newest-first order every
// real query returns (recencyOrdered(result) is always true), which is what
// lets these tests build a model by hand (no DB) and still exercise the same
// display-row math a real browsing session would.
func buildBucketedItems(now time.Time, perBucket int) []search.Item {
	daysAgo := []int{0, 1, 3, 10, 40}
	var items []search.Item
	handle := 1
	for _, days := range daysAgo {
		for i := 0; i < perBucket; i++ {
			at := now.AddDate(0, 0, -days).Add(-time.Duration(i) * time.Minute)
			items = append(items, dateHeaderItem(handle, timep(at)))
			handle++
		}
	}
	for i := 0; i < perBucket; i++ {
		items = append(items, dateHeaderItem(handle, nil))
		handle++
	}
	return items
}

// dateHeaderModel builds a browseModel directly from items, with dateHeaders
// and a pinned clock set, and applies size - the same no-DB construction
// TestTextFilterKeepsRecencyOrder already uses for pure display-logic tests,
// extended with a WindowSizeMsg so geometry, rebuild and keepCursorVisible
// all run exactly as they would in the real program. focus and detail are
// set by hand because this bypasses newBrowseModel (which needs a real *DB*
// to run its own loadAll) - both are things a WindowSizeMsg touches
// regardless of which panel is focused (Update's own WindowSizeMsg case, and
// panelDrawn's fallback to Sessions).
func dateHeaderModel(t *testing.T, items []search.Item, headers bool, now time.Time, width, height int) *browseModel {
	t.Helper()
	m := &browseModel{
		all:         items,
		dateHeaders: headers,
		now:         func() time.Time { return now },
		focus:       panelSessions,
		detail:      &viewport.Model{},
	}
	return update(t, m, tea.WindowSizeMsg{Width: width, Height: height})
}

// TestBucketForTimeUsesLocalMidnightBoundaries pins down the exact boundary
// rule (spec: "based on each session's LastActivityAt in local time, with
// boundaries at local midnight") rather than "24 hours ago": a session from
// late last night is Yesterday even though fewer than 24 hours have passed,
// and a session from just after midnight today is Today even though nearly
// 24 hours have not yet passed either.
func TestBucketForTimeUsesLocalMidnightBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 30, 0, 0, time.Local)
	cases := []struct {
		name string
		at   time.Time
		want dateBucket
	}{
		{"40 minutes ago but the previous calendar day is Yesterday", time.Date(2026, 9, 24, 23, 50, 0, 0, time.Local), bucketYesterday},
		{"same calendar day, hours apart, is still Today", time.Date(2026, 9, 25, 0, 1, 0, 0, time.Local), bucketToday},
		{"exactly 7 calendar days back is still Last 7 days", now.AddDate(0, 0, -7), bucketLast7Days},
		{"8 calendar days back rolls into Last 30 days", now.AddDate(0, 0, -8), bucketLast30Days},
		{"exactly 30 calendar days back is still Last 30 days", now.AddDate(0, 0, -30), bucketLast30Days},
		{"31 calendar days back is Older", now.AddDate(0, 0, -31), bucketOlder},
		{"a timestamp after now (clock skew) is still Today", now.Add(time.Hour), bucketToday},
	}
	for _, c := range cases {
		if got := bucketForTime(c.at, now); got != c.want {
			t.Errorf("%s: bucketForTime = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRecencyOrderedDetectsAnOutOfOrderList covers the self-check
// recencyOrdered's own doc comment describes: newest-first with a trailing
// run of no-timestamp sessions is "ordered"; anything where an earlier item
// is older than a later one is not.
func TestRecencyOrderedDetectsAnOutOfOrderList(t *testing.T) {
	newer := time.Date(2026, 9, 25, 10, 0, 0, 0, time.Local)
	older := newer.AddDate(0, 0, -5)
	ordered := []search.Item{
		dateHeaderItem(1, timep(newer)),
		dateHeaderItem(2, timep(older)),
		dateHeaderItem(3, nil),
	}
	if !recencyOrdered(ordered) {
		t.Error("a newest-first list with a trailing no-timestamp session was reported out of order")
	}
	outOfOrder := []search.Item{
		dateHeaderItem(1, timep(older)),
		dateHeaderItem(2, timep(newer)),
	}
	if recencyOrdered(outOfOrder) {
		t.Error("an out-of-order list was reported as recency-ordered")
	}
}

// TestDateHeadersBucketAndOrderCorrectly is the end-to-end render test: one
// session per bucket, and the Sessions panel must draw exactly one header per
// bucket, in bucket order, immediately before that bucket's session.
func TestDateHeadersBucketAndOrderCorrectly(t *testing.T) {
	now := time.Date(2026, 9, 25, 15, 0, 0, 0, time.Local)
	items := []search.Item{
		dateHeaderItem(1, timep(now.Add(-time.Hour))),    // Today
		dateHeaderItem(2, timep(now.AddDate(0, 0, -1))),  // Yesterday
		dateHeaderItem(3, timep(now.AddDate(0, 0, -3))),  // Last 7 days
		dateHeaderItem(4, timep(now.AddDate(0, 0, -10))), // Last 30 days
		dateHeaderItem(5, timep(now.AddDate(0, 0, -40))), // Older
		dateHeaderItem(6, nil),                           // Unknown date
	}
	m := dateHeaderModel(t, items, true, now, 100, 40)

	box := m.sessionsPanel(m.geometry())
	wantLabels := []string{"Today", "Yesterday", "Last 7 days", "Last 30 days", "Older", "Unknown date"}
	var gotLabels []string
	for _, l := range box.Lines {
		for _, lab := range wantLabels {
			if strings.Contains(l, lab) {
				gotLabels = append(gotLabels, lab)
			}
		}
	}
	if !reflect.DeepEqual(gotLabels, wantLabels) {
		t.Errorf("header labels in order = %v, want %v\nfull render:\n%s", gotLabels, wantLabels, strings.Join(box.Lines, "\n"))
	}
	if len(box.Lines) != len(items)+len(wantLabels) {
		t.Errorf("drew %d lines, want %d (one per session plus one header per bucket)", len(box.Lines), len(items)+len(wantLabels))
	}
}

// TestDateHeadersOnlyPopulatedBucketsShowAHeader: with sessions in only two
// of the six buckets, only those two headers may appear - a header for an
// empty bucket would say "these are the sessions in this bucket" about
// nothing.
func TestDateHeadersOnlyPopulatedBucketsShowAHeader(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	items := []search.Item{
		dateHeaderItem(1, timep(now)),
		dateHeaderItem(2, timep(now.AddDate(0, 0, -40))),
	}
	m := dateHeaderModel(t, items, true, now, 100, 40)
	box := m.sessionsPanel(m.geometry())
	joined := strings.Join(box.Lines, "\n")
	for _, absent := range []string{"Yesterday", "Last 7 days", "Last 30 days", "Unknown date"} {
		if strings.Contains(joined, absent) {
			t.Errorf("rendered a header for an empty bucket %q:\n%s", absent, joined)
		}
	}
	if !strings.Contains(joined, "Today") || !strings.Contains(joined, "Older") {
		t.Errorf("missing an expected header:\n%s", joined)
	}
	if len(box.Lines) != 4 {
		t.Errorf("drew %d lines, want 4 (2 headers + 2 sessions)", len(box.Lines))
	}
}

// TestDateHeadersOffShowsPlainList covers browse.date_headers = false
// (BrowserOptions.DateHeaders unset, the default off in this package - see
// its doc comment): the exact pre-feature output, one line per session, no
// separators at all, even though the sessions span every bucket.
func TestDateHeadersOffShowsPlainList(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	items := buildBucketedItems(now, 2)
	m := dateHeaderModel(t, items, false, now, 100, 40)
	box := m.sessionsPanel(m.geometry())
	if len(box.Lines) != len(items) {
		t.Errorf("dateHeaders=false drew %d lines, want exactly %d (one per session)", len(box.Lines), len(items))
	}
	joined := strings.Join(box.Lines, "\n")
	for _, lab := range []string{"Today", "Yesterday", "Last 7 days", "Last 30 days", "Older", "Unknown date"} {
		if strings.Contains(joined, lab) {
			t.Errorf("dateHeaders=false still drew a %q header", lab)
		}
	}
}

// TestBrowserOptionsDateHeadersWiresThrough covers the option itself: unset
// leaves the model's headers off (see BrowserOptions.DateHeaders' doc
// comment on why that default lives here rather than matching the config
// key's true default), and DateHeaders: true turns them on.
func TestBrowserOptionsDateHeadersWiresThrough(t *testing.T) {
	db := browseTestDB(t)
	seedFixture(t, db)
	off := newTestBrowser(db, "p", BrowserOptions{Installs: testInstalls("claude")})
	if off.dateHeaders {
		t.Error("BrowserOptions{} (DateHeaders unset) produced dateHeaders=true, want false")
	}
	on := newTestBrowser(db, "p", BrowserOptions{DateHeaders: true, Installs: testInstalls("claude")})
	if !on.dateHeaders {
		t.Error("BrowserOptions{DateHeaders: true} did not set browseModel.dateHeaders")
	}
}

// TestDateHeadersHiddenWhenListIsNotRecencyOrdered exercises recencyOrdered's
// self-check directly: no code path in this browser produces an out-of-order
// m.visible today (list, "/", and "s" are all recency-ordered - see
// recencyOrdered's doc comment), so this hand-builds the one condition that
// would make it happen anyway, rather than trusting that invariant to hold
// forever without anything rechecking it.
func TestDateHeadersHiddenWhenListIsNotRecencyOrdered(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	older := now.AddDate(0, 0, -40)
	items := []search.Item{
		dateHeaderItem(1, timep(older)), // deliberately older-before-newer
		dateHeaderItem(2, timep(now)),
	}
	m := dateHeaderModel(t, items, true, now, 100, 40)
	if recencyOrdered(m.visible) {
		t.Fatal("test fixture is not actually out of order - fix the fixture")
	}
	box := m.sessionsPanel(m.geometry())
	joined := strings.Join(box.Lines, "\n")
	if len(box.Lines) != len(items) {
		t.Errorf("out-of-order list drew %d lines, want %d (no headers)", len(box.Lines), len(items))
	}
	for _, lab := range []string{"Today", "Older"} {
		if strings.Contains(joined, lab) {
			t.Errorf("drew a header (%q) for a list that is not recency-ordered", lab)
		}
	}
}

// TestDateHeadersCursorIndexesSessionsNotDisplayRows: m.cursor must walk
// exactly one session per "j", regardless of how many header rows sit
// between them - the cursor indexes m.visible, never the mixed display-row
// list.
func TestDateHeadersCursorIndexesSessionsNotDisplayRows(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	items := buildBucketedItems(now, 2)
	m := dateHeaderModel(t, items, true, now, 100, 40)
	for i := 0; i < len(items)-1; i++ {
		m = update(t, m, keyRunes("j"))
		if m.cursor != i+1 {
			t.Fatalf("after %d 'j' presses cursor = %d, want %d", i+1, m.cursor, i+1)
		}
		got := m.current()
		if got == nil || got.SessionID != items[i+1].SessionID {
			t.Fatalf("cursor %d does not point at session %d", m.cursor, i+1)
		}
	}
	// One more 'j' past the end must not move past the last session.
	m = update(t, m, keyRunes("j"))
	if m.cursor != len(items)-1 {
		t.Errorf("cursor moved past the last session: %d", m.cursor)
	}
}

// TestDateHeadersGAndCtrlDUMoveByHeaderAwareRows covers g/G and Ctrl-D/U with
// headers on and a panel too short to show the whole list at once: every one
// of them must still land on a real session and never overflow the panel's
// row budget.
func TestDateHeadersGAndCtrlDUMoveByHeaderAwareRows(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	items := buildBucketedItems(now, 4) // 24 sessions across 6 buckets, plus their headers
	m := dateHeaderModel(t, items, true, now, 100, 20)
	g := m.geometry()
	if g.sessionsInner <= 0 || g.sessionsInner >= len(items) {
		t.Fatalf("need a panel shorter than the full list for this test to mean anything (sessionsInner=%d, sessions=%d)", g.sessionsInner, len(items))
	}

	m = update(t, m, keyRunes("G"))
	if m.cursor != len(items)-1 {
		t.Errorf("G put the cursor at %d, want %d", m.cursor, len(items)-1)
	}
	assertCursorDrawnWithinBudget(t, m, g, "G")

	m = update(t, m, keyRunes("g"))
	if m.cursor != 0 || m.listTop != 0 {
		t.Errorf("g left cursor=%d top=%d, want 0/0", m.cursor, m.listTop)
	}
	assertCursorDrawnWithinBudget(t, m, g, "g")

	before := m.cursor
	m = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.cursor <= before {
		t.Errorf("Ctrl-D did not move the cursor forward: %d -> %d", before, m.cursor)
	}
	assertCursorDrawnWithinBudget(t, m, g, "Ctrl-D")

	before = m.cursor
	m = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.cursor >= before {
		t.Errorf("Ctrl-U did not move the cursor backward: %d -> %d", before, m.cursor)
	}
	assertCursorDrawnWithinBudget(t, m, g, "Ctrl-U")
}

// assertCursorDrawnWithinBudget checks the two invariants every cursor move
// must keep, with or without headers: the panel never draws more rows than
// its budget, and the selected session is one of the rows it drew.
func assertCursorDrawnWithinBudget(t *testing.T, m *browseModel, g geometry, action string) {
	t.Helper()
	box := m.sessionsPanel(g)
	if len(box.Lines) > g.sessionsInner {
		t.Errorf("after %s: drew %d lines for a %d-row panel", action, len(box.Lines), g.sessionsInner)
	}
	if !strings.Contains(strings.Join(box.Lines, "\n"), "▸ ") {
		t.Errorf("after %s: the selected session (cursor=%d) is not drawn:\n%s", action, m.cursor, strings.Join(box.Lines, "\n"))
	}
}

// TestKeepCursorVisibleNeverOverflowsWithHeaders walks the cursor through an
// entire header-bearing list at several panel heights, including ones far
// shorter than the number of buckets - the small-terminal case the task
// singles out - and checks at every step that the panel never draws more
// rows than it was given and the selected session is always among the rows
// it drew, even when its own header cannot be (h==1).
func TestKeepCursorVisibleNeverOverflowsWithHeaders(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	items := buildBucketedItems(now, 5) // 30 sessions across 6 buckets, plus their headers
	for _, size := range [][2]int{{100, 10}, {100, 6}, {60, 5}, {60, 4}} {
		m := dateHeaderModel(t, items, true, now, size[0], size[1])
		g := m.geometry()
		for step := 0; step < len(items); step++ {
			box := m.sessionsPanel(g)
			if len(box.Lines) > g.sessionsInner {
				t.Fatalf("%v step %d: drew %d lines for a %d-row panel", size, step, len(box.Lines), g.sessionsInner)
			}
			if g.sessionsInner > 0 && !strings.Contains(strings.Join(box.Lines, "\n"), "▸ ") {
				t.Fatalf("%v step %d: the selected session (cursor=%d) is not drawn:\n%s", size, step, m.cursor, strings.Join(box.Lines, "\n"))
			}
			m = update(t, m, keyRunes("j"))
		}
	}
}

// TestViewFitsTerminalWithDateHeaders is TestViewFitsTerminal's sibling with
// headers turned on, over the same size matrix the frame-fitting regression
// tests (e0f3b03, d781915, 57a783f, 0371e5f) use: those never set
// BrowserOptions.DateHeaders, so without this the whole header code path
// would go unexercised by the tests that specifically guard against the
// frame overflowing its terminal.
func TestViewFitsTerminalWithDateHeaders(t *testing.T) {
	sizes := [][2]int{
		{100, 40}, {100, 26}, {120, 30}, {80, 24}, {76, 20}, {70, 20}, {60, 14}, {40, 10},
		{100, 4}, {100, 5}, {100, 6}, {100, 7}, {100, 8}, {100, 9}, {100, 10}, {100, 11}, {100, 12},
		{60, 4}, {60, 5}, {60, 6}, {60, 7}, {60, 8}, {60, 9}, {60, 10}, {60, 11}, {60, 12},
	}
	for _, size := range sizes {
		w, h := size[0], size[1]
		// seedFixture's sessions are all from 2023 (browseTestDB's fixture,
		// far in the past relative to any real clock), which lands them all
		// in the same Older bucket against time.Now() - no injected clock
		// here on purpose, so this also covers the real fallback in clock().
		db := browseTestDB(t)
		seedFixture(t, db)
		m := newTestBrowser(db, "claude-personal", BrowserOptions{
			DateHeaders: true, Installs: testInstalls("claude-personal"),
		})
		m = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
		lines := strings.Split(m.View(), "\n")
		if len(lines) > h {
			t.Errorf("%dx%d: frame is %d lines, taller than the terminal", w, h, len(lines))
		}
		for i, l := range lines {
			if got := visibleWidth(l); got > w {
				t.Errorf("%dx%d: line %d is %d columns wide: %q", w, h, i, got, l)
			}
		}

		g := m.geometry()
		box := m.sessionsPanel(g)
		if len(box.Lines) > g.sessionsInner {
			t.Errorf("%dx%d: sessions panel drew %d lines for a %d-row budget", w, h, len(box.Lines), g.sessionsInner)
		}
		if !box.Collapsed && g.sessionsInner > 0 && !strings.Contains(strings.Join(box.Lines, "\n"), "▸ ") {
			t.Errorf("%dx%d: the selected session is not drawn:\n%s", w, h, strings.Join(box.Lines, "\n"))
		}
	}
}
