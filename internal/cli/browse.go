// The in-process interactive browser: a bubbletea program that holds every
// piece of browsing state - active filters, active profile, selection,
// scroll position - in the running process. There is no external fuzzy
// finder, no coordination file, and no re-invocation of the binary; the
// terminal is owned and restored by the interface itself on every exit
// path (change replace-fzf-browser-with-tui, design.md decision 1).
//
// Change lazy-style-browser reshapes it into the layout the lazy* family
// established: the dimensions you slice sessions by - profile, agent,
// repository, tag - are persistent panels down the left with live counts,
// not transient prompts you open with a letter and dismiss. Two
// consequences follow, and they are the point of the change:
//
//   - The filters in effect are visible because they *are* the interface.
//     Previously they survived only as a line of header text, and the way
//     to discover what you could filter by was to remember which letter
//     opened which prompt.
//   - Navigation is "move to a panel, move within it, press Enter",
//     uniformly, instead of a different modal prompt per dimension.
package cli

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rfist/lazyrecall/internal/annotate"
	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/refresh"
	"github.com/rfist/lazyrecall/internal/search"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
	"github.com/rfist/lazyrecall/internal/transcript"
)

// BrowserOptions carries the initial state for one browsing session.
type BrowserOptions struct {
	DB          *sqlitex.Runner // the active profile's database, already refreshed
	ProfileName string
	Repo        string // initial filter values (from command-line flags)
	Agent       string
	Tag         string
	// Client narrows every listing to one client for the whole session
	// (change show-editor-clients). Unlike agent/repo/tag it has no panel
	// of its own - it is a qualifier on the agent, not a dimension worth a
	// quarter of the side column - so it stays as it was passed in and is
	// cleared by restarting the browser, not from inside it.
	Client string
	Query  string
	Style  bool // NO_COLOR-aware: suppress all styling when the environment asks

	// ShowAll opens the browser with the hide rules and the archive flag
	// disabled - "show me everything". The caller seeds it from the config
	// file's browse.show_archived, so a user who asked to keep archived
	// sessions visible has that preference from the first frame.
	ShowAll bool
	// Hide carries the standing hide rules so the browser suppresses the
	// same sessions the non-interactive commands do, rather than keeping
	// two notions of "noise".
	Hide config.Hide

	// Resolve resolves a profile name for the browser's in-process refresh
	// and profile-switch actions. Defaults to the same resolution the
	// command line uses; injectable so tests can run against synthetic
	// profiles without touching this machine's real config roots.
	Resolve func(name string) (profile.Profile, error)

	// Profiles lists every profile that can be switched to (change
	// choose-from-known-values, task 2.1: "supply profile candidates from
	// profile resolution"). Defaults to profile.Discover(); injectable for
	// the same reason as Resolve - tests must never depend on this
	// machine's real config roots.
	Profiles func() []profile.Profile
}

// resolve uses the injected resolver, defaulting to the standard profile
// resolution when none was provided.
func (o BrowserOptions) resolve(name string) (profile.Profile, error) {
	if o.Resolve != nil {
		return o.Resolve(name)
	}
	profiles, err := profile.Discover()
	if err != nil {
		return profile.Profile{}, err
	}
	return profile.Resolve(profiles, name)
}

// discoverProfiles uses the injected lister, defaulting to the standard
// profile discovery when none was provided. The lister type has no error
// slot (the browser's profile-switch panel wants a plain slice), so a
// discovery failure yields an empty list; command paths surface the same
// failure through the resolver instead.
func (o BrowserOptions) discoverProfiles() []profile.Profile {
	if o.Profiles != nil {
		return o.Profiles()
	}
	profiles, _ := profile.Discover()
	return profiles
}

// RunBrowser runs the in-process browser to completion and returns the
// session the user chose (ok=true) or reports cancellation (ok=false). The
// terminal is restored on every exit path - normal quit, cancelling an
// input prompt, cancelling the browser, and termination by signal
// (spec session-search, "The terminal is always restored").
func RunBrowser(opts BrowserOptions) (search.Item, bool, error) {
	m := newBrowseModel(opts)
	p := tea.NewProgram(&m, tea.WithAltScreen())

	// bubbletea handles SIGINT and SIGTERM itself (as keys or as a
	// graceful quit). SIGHUP - the terminal being closed - is not one of
	// them, so forward it as a graceful quit: the program's shutdown then
	// restores the terminal before the process exits, instead of dying in
	// raw mode.
	stop := make(chan struct{})
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		defer signal.Stop(ch)
		select {
		case <-ch:
			p.Send(tea.QuitMsg{})
		case <-stop:
		}
	}()

	final, err := p.Run()
	close(stop)
	if err != nil {
		return search.Item{}, false, err
	}
	fm := final.(*browseModel)
	return fm.selected, fm.selectedOK, nil
}

// ---------------------------------------------------------------------
// Panels
// ---------------------------------------------------------------------

// panelID names one focusable region. Focus is the browser's primary mode:
// which panel has it determines what the movement keys move, what Enter
// applies, and which actions the footer and the action menu offer.
type panelID int

const (
	panelProfiles panelID = iota
	panelAgents
	panelRepos
	panelTags
	panelSessions
	panelDetail
	numPanels
)

// jumpKey is the digit that focuses a panel directly, and whether it has
// one at all - the detail pane does not: it is reached with Tab from
// Sessions, which is where you already are when you want to read it.
//
// Sessions was renumbered from 5 to 0 (change spatial-panel-navigation),
// which makes 0 a real jump key. Before that, "no jump key" was signalled by
// returning the int 0 - a value no panel actually used, so it was safe as a
// sentinel. It no longer is: Sessions' real jump key and Detail's "none"
// would be the same int, and panelBox.Number's own zero value already means
// "don't draw a digit" (panel.go), which would have swallowed Sessions'
// digit at the border too. The ok return is what keeps "none" a fact of its
// own, distinguishable from every real digit including 0 - see facetPanel
// and sessionsPanel below, which set panelBox.HasNumber from it rather than
// inferring "none" from Number alone.
func (p panelID) jumpKey() (int, bool) {
	switch p {
	case panelDetail:
		return 0, false
	case panelSessions:
		return 0, true
	}
	return int(p) + 1, true
}

func (p panelID) title() string {
	switch p {
	case panelProfiles:
		return "Profiles"
	case panelAgents:
		return "Agents"
	case panelRepos:
		return "Repos"
	case panelTags:
		return "Tags"
	case panelSessions:
		return "Sessions"
	}
	return "Detail"
}

// isFacet reports whether p is one of the three panels that filter the
// session list by a value. Profiles looks the same but does not filter -
// it replaces the whole view - and Sessions/Detail are not filters at all.
func (p panelID) isFacet() bool {
	return p == panelAgents || p == panelRepos || p == panelTags
}

// isLeftColumn reports whether p is one of the four panels stacked in the
// left column - Profiles, Agents, Repos, Tags. Unlike isFacet it includes
// Profiles: this is a question about where a panel sits on screen, not
// about what it does, and Profiles sits in that column even though
// selecting a row there replaces the view rather than filtering it.
func (p panelID) isLeftColumn() bool {
	return p == panelProfiles || p == panelAgents || p == panelRepos || p == panelTags
}

// detailTab is which page of the right-hand pane is showing.
type detailTab int

// The order is the order you need them in: what the session is, what you
// asked it, what actually happened, and what you wrote down about it.
// Transcript sits next to Prompts because they answer the same question at
// two depths - Prompts to recognise the session, Transcript to see where it
// was left.
const (
	tabDetail detailTab = iota
	tabPrompts
	tabTranscript
	tabComments
	numDetailTabs
)

func (t detailTab) title() string {
	switch t {
	case tabPrompts:
		return "Prompts"
	case tabTranscript:
		return "Transcript"
	case tabComments:
		return "Comments"
	}
	return "Detail"
}

// ---------------------------------------------------------------------
// Input modes
// ---------------------------------------------------------------------

// inputMode is what the browser is currently prompting for. Prompts are
// input modes owned by the interface - cancelling one (Esc) returns to
// browsing with no change applied, and submitting empty applies nothing
// except on a filter prompt, where it clears that one filter (spec
// session-search, "Leaving input mode").
//
// The three prompts that used to ask for a profile, an agent, or a tag are
// gone: those values are panels now, chosen by moving to them. What is
// left is the input that genuinely is free text - a search phrase, a text
// narrowing, a new tag, a comment - plus the action menu.
type inputMode int

const (
	modeNone   inputMode = iota
	modeFilter           // narrows the focused panel's rows
	modeSearchPhrase
	modeAddTag
	modeRemoveTag
	modeAddComment
	modeRemoveComment
	modeMenu // the action menu (x)
)

func (m inputMode) prompt() string {
	switch m {
	case modeFilter:
		return "filter (blank to clear): "
	case modeSearchPhrase:
		return "search phrase (blank to clear): "
	case modeAddTag:
		return "tag to add: "
	case modeRemoveTag:
		return "tag to remove: "
	case modeAddComment:
		return "comment to add: "
	case modeRemoveComment:
		return "comment id to remove: "
	case modeMenu:
		return "action (type to narrow): "
	}
	return ""
}

// ---------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------

// browseModel is the running state of the browser. Everything here lives in
// the process; nothing is written to disk for coordination.
type browseModel struct {
	db          *sqlitex.Runner
	profileName string
	style       bool
	width       int
	height      int

	focus panelID

	// showAll disables the hide rules and the archive flag together, because
	// they are one question to the user: "show me everything". hide is the
	// standing config rules, passed through to the same search.Filter the
	// non-interactive commands build.
	showAll bool
	hide    config.Hide
	// client is the standing --client narrowing, applied to every query
	// this browser runs (see BrowserOptions.Client).
	client string
	// hidden is how many sessions the rules suppressed in the current
	// result set - the `N hidden` the border reports so hiding is never
	// silent, the same contract the command-line header keeps.
	hidden int

	// all is the profile's whole result set for the current search phrase,
	// queried once and sliced in process (see facet.go for why). It is
	// re-queried only when the corpus itself can have changed: a profile
	// switch, an index refresh, a new search phrase, or an annotation edit.
	all   []search.Item
	query string // the full-text search phrase, "" for none

	profiles_ facet // the Profiles panel: same shape, but Enter switches rather than filters
	agents    facet
	repos     facet
	tags      facet

	// textFilter narrows the loaded session rows in process as the user
	// types (spec session-search, "Narrowing the loaded rows by typing").
	textFilter string

	visible []search.Item // sessions after every facet and the text filter
	cursor  int
	listTop int

	tab      detailTab
	detail   *viewport.Model
	detailOf string // session id the viewport's content was built for

	// convo caches the transcript the Transcript tab is showing. The pane
	// is rebuilt on every frame, and a transcript is a file read rather
	// than a query against the already-loaded result set, so without this
	// every keystroke would re-read it. Behind a pointer for the same
	// reason the viewport is: the render path takes the model by value.
	convo *conversationCache

	mode       inputMode
	input      textinput.Model
	inputLabel string
	resolve    func(name string) (profile.Profile, error)
	profiles   func() []profile.Profile

	// Action-menu state. The menu is a list of concrete actions for the
	// focused panel, narrowable by typing - the same interaction the old
	// value-selection prompt had, now pointed at verbs instead of values.
	menuAll      []menuAction
	menuFiltered []menuAction
	menuCursor   int

	help   bool // the full action list is showing
	notice string

	selected   search.Item
	selectedOK bool
}

// Init satisfies tea.Model: the browser needs no startup command beyond
// the initial refresh already performed by the caller before the program
// was created (design.md decision 5: refresh once on open).
func (m browseModel) Init() tea.Cmd { return nil }

func newBrowseModel(opts BrowserOptions) browseModel {
	m := browseModel{
		db:          opts.DB,
		profileName: opts.ProfileName,
		style:       opts.Style,
		showAll:     opts.ShowAll,
		hide:        opts.Hide,
		resolve:     opts.resolve,
		profiles:    opts.discoverProfiles,
		width:       DefaultWidth,
		height:      24,
		focus:       panelSessions,
		query:       opts.Query,
		client:      opts.Client,
		input:       textinput.New(),
	}
	// Command-line filters open as the corresponding panels' selections, so
	// `lazyrecall browse --agent=pi` and walking to "pi" in the Agents panel
	// land in exactly the same state.
	m.agents.Sel = opts.Agent
	m.repos.Sel = opts.Repo
	m.tags.Sel = opts.Tag
	m.input.CharLimit = 1000
	detail := viewport.New(m.width, 12)
	m.detail = &detail
	m.convo = &conversationCache{}
	m.loadAll()
	return m
}

// ---------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------

type refreshDoneMsg struct{ err error }
type dbSwitchedMsg struct {
	name string
	db   *sqlitex.Runner
	err  error
}

// ---------------------------------------------------------------------
// Queries and derivation
// ---------------------------------------------------------------------

// loadAll re-queries the profile's whole result set and rebuilds everything
// derived from it. This is the only function that touches the session
// tables; every filter change below is a walk over what it loaded. The hide
// rules and the archive flag ride on the same search.Filter the
// non-interactive commands build, and the WithHidden variant reports how
// many sessions they suppressed, which the Sessions border then shows.
func (m *browseModel) loadAll() {
	f := search.Filter{Client: m.client, Hide: m.hide, ShowAll: m.showAll}
	var (
		rows   []search.Item
		hidden int
		err    error
	)
	if m.query != "" {
		rows, hidden, err = search.SearchWithHidden(m.db, m.query, f)
	} else {
		rows, hidden, err = search.ListWithHidden(m.db, f)
	}
	if err != nil {
		m.notice = "browse: " + err.Error()
		return
	}
	m.all = rows
	m.hidden = hidden
	m.rebuild()
}

// rebuild recomputes the facet panels and the visible session list from
// m.all. Each facet is counted over the corpus narrowed by the *other*
// facets, so a panel always answers "what would selecting this row give
// me, on top of what is already selected" - a row showing 0 would be a row
// that leads nowhere, and none is ever shown.
//
// The text filter is part of "what is already selected" for that purpose,
// which it was not until change literal-substring-filter. It used to be
// applied only to m.visible, on the last line here, after the three panels
// had been counted - so with "postman" typed, Repos could still offer a
// repository with three sessions behind it and selecting that row produced
// an empty list. The count was not stale, it was wrong about the single
// thing a facet count promises. That the search phrase ("s") never had the
// same defect is an accident of where the two filters act: a phrase is a
// database query and changes m.all itself, while the text filter is a walk
// over what m.all already holds, and so was invisible to everything
// upstream of it.
//
// corpus is what keeps that from happening again: every panel and the
// session list ask the same function for their rows, so a filter added to
// it applies to all four by construction rather than by four call sites
// remembering to.
func (m *browseModel) rebuild() {
	corpus := func(agent, repo, tag string) []search.Item {
		return m.applyTextFilter(narrow(m.all, agent, repo, tag))
	}
	m.agents.setRows(countBy(corpus("", m.repos.Sel, m.tags.Sel), "all agents", agentKey, nil))
	m.repos.setRows(countBy(corpus(m.agents.Sel, "", m.tags.Sel), "all repos", repoKey, abbreviateHome))
	m.tags.setRows(countBy(corpus(m.agents.Sel, m.repos.Sel, ""), "all tags", tagKeys, func(s string) string { return "#" + s }))
	m.profiles_.setRows(m.profileRows())

	m.visible = corpus(m.agents.Sel, m.repos.Sel, m.tags.Sel)
	if m.cursor >= len(m.visible) {
		m.cursor = len(m.visible) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.listTop > m.cursor {
		m.listTop = m.cursor
	}
}

// profileRows lists every profile on the machine, active one first. There
// are no counts: a count would mean opening every other profile's database,
// and profiles are isolated from each other by design - reading one to
// annotate another's panel is exactly the mixing that isolation exists to
// prevent.
func (m browseModel) profileRows() []facetRow {
	var out []facetRow
	for _, p := range m.profiles() {
		if p.Name == "" {
			continue
		}
		out = append(out, facetRow{Value: p.Name, Label: p.Name})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Value == m.profileName) != (out[j].Value == m.profileName) {
			return out[i].Value == m.profileName
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// matchText is the per-row haystack the text filter matches against: the
// row's own slots, shortened to width by fitRowToWidth exactly as the
// session list shortens them before drawing, then joined - which is to say
// exactly what the row puts on screen, and nothing else (change
// literal-substring-filter, corrected by fit-filter-to-drawn-width).
//
// It is built from rowSlots rather than from the Item's fields directly so
// that the two cannot drift apart. The rule a user can hold in their head
// is "if I can read it on the line, I can filter on it; if I can't, I
// can't" - and that rule is only true if display and matching are the same
// text by construction. The predecessor built its own list and quietly
// included the git branch, the topic AND the last prompt, while the row
// shows only the first non-empty of name/topic/last-prompt: rows matched
// on text that was not on them and could not be, which is most of what
// made the filter feel arbitrary.
//
// literal-substring-filter left one gap in that same rule: it matched
// rowSlots *before* fitRowToWidth shortens them, on the theory that being
// able to find a session by a word the width cut off was help, not
// surprise. It is not help when the row is a 5,203-character last prompt
// and the panel draws roughly the first 130 columns of it - "adv" matched
// "advertises" at character 553, nowhere near the screen, which is exactly
// the "why is this row here?" complaint the whole change existed to fix.
// width must be the number the row is actually rendered at (sessionRowWidth
// - not a guessed constant, not DefaultWidth for its own sake), or this
// goes back to matching a different string than the one on screen.
//
// A direct, and intended, consequence: filter results now depend on the
// terminal's width. A word visible in a wide terminal can straddle or fall
// past the truncation point in a narrower one and stop matching there.
// That is the honest meaning of "match what is displayed" - the row really
// is narrower at 80 columns than at 200 - not an oversight to be smoothed
// over by matching some width-independent superset of it.
func matchText(it search.Item, width int) string {
	slots := rowSlots(it)
	fitRowToWidth(slots, width)
	return strings.Join(slots, " ")
}

// sessionRowWidth is the unmarked content width a session row is rendered
// at: box.innerWidth() less rowDecorationWidth. It is computed once per
// caller, then sessionRowWidthFor deducts a marker only for the archived
// rows that draw one. Keeping the shared base here avoids recomputing
// geometry for every corpus item without pretending that every row has the
// same last eleven columns.
func (m browseModel) sessionRowWidth() int {
	if m.width <= 0 {
		// No terminal size is known yet - every applyTextFilter test in
		// this file builds a browseModel by hand and never sends a
		// tea.WindowSizeMsg, so m.width is Go's int zero value, not a real
		// geometry. RenderRow itself treats Width<=0 as "unset, use
		// DefaultWidth" (style.go) for exactly this reason: geometry()'s
		// own arithmetic has no such fallback, and driven by a zero width
		// it clamps down to the narrowest legal panel (1 column, from
		// panelBox.innerWidth's own floor) rather than reporting "unknown"
		// - which would make matching against an unsized model filter on
		// almost nothing, not on what a real terminal would show. Falling
		// back to the same sentinel RenderRow uses keeps the two
		// consistent about what "no width" means.
		return DefaultWidth
	}
	g := m.geometry()
	width := panelBox{Width: g.rightWidth}.innerWidth() - rowDecorationWidth
	if width < 1 {
		// Mirrors the clamp sessionsPanel itself applies to rowOpts.Width
		// before calling RenderRow (see the comment there): 0 or negative
		// would fall through to RenderRow's "unset" sentinel above and
		// match against DefaultWidth's worth of text in a terminal with
		// far less room than that to show it - the opposite of what a
		// too-narrow terminal should mean here.
		width = 1
	}
	return width
}

// sessionRowWidthFor returns the content width RenderRow receives for it.
// The archive marker is appended after RenderRow, but it is still inside the
// row budget, so an archived row has to surrender its visible width before
// both rendering and matching. `showAll && it.Archived` is deliberately the
// same condition sessionsPanel uses to draw the marker; separate predicates
// would silently recreate the mismatch this helper closes.
func (m browseModel) sessionRowWidthFor(it search.Item, width int) int {
	if m.showAll && it.Archived {
		width -= visibleWidth(" " + archivedMarker)
	}
	if width < 1 {
		// RenderRow's non-positive width is its "not supplied" sentinel,
		// not an instruction to make a too-narrow terminal wide again.
		return 1
	}
	return width
}

// applyTextFilter keeps the rows whose text contains m.textFilter, matched
// literally and without regard to case, in the order they were already in
// (most recently active first).
//
// Literal, not fuzzy: the filter used to be a subsequence match, which is
// the right algorithm for a fuzzy finder hunting a path and the wrong one
// here. "postman" would match p...o...s...t...m...a...n scattered across
// three hundred characters of a row, so a seven-letter query kept half the
// listing and no row explained why it was there. A session list is read,
// not hunted through: the query is a word the user saw, or expects to see,
// on the line.
//
// Order is left alone rather than ranked. There is no match quality to rank
// by once matching is literal - a row either contains the word or does not
// - and recency is the order the whole browser is built around. (The old
// code did ask its matcher for a rank and then never sorted by it, so the
// listing was in recency order anyway, minus the ranking work.)
//
// The base match width is computed once, outside the loop. Archived rows
// then deduct their own marker with sessionRowWidthFor, a constant-time
// branch that preserves the renderer's per-item budget without rebuilding
// geometry len(rows) times.
func (m browseModel) applyTextFilter(rows []search.Item) []search.Item {
	if m.textFilter == "" {
		return rows
	}
	baseWidth := m.sessionRowWidth()
	needle := strings.ToLower(m.textFilter)
	out := make([]search.Item, 0, len(rows))
	for _, it := range rows {
		if strings.Contains(strings.ToLower(matchText(it, m.sessionRowWidthFor(it, baseWidth))), needle) {
			out = append(out, it)
		}
	}
	return out
}

func (m *browseModel) current() *search.Item {
	if m.cursor >= 0 && m.cursor < len(m.visible) {
		return &m.visible[m.cursor]
	}
	return nil
}

// facetFor returns the panel state p navigates, or nil for panels that
// hold no row list of their own.
func (m *browseModel) facetFor(p panelID) *facet {
	switch p {
	case panelProfiles:
		return &m.profiles_
	case panelAgents:
		return &m.agents
	case panelRepos:
		return &m.repos
	case panelTags:
		return &m.tags
	}
	return nil
}

// keepCursorVisible scrolls the session list window so the cursor is shown.
func (m *browseModel) keepCursorVisible() {
	h := m.geometry().sessionsInner
	if h <= 0 {
		return
	}
	if m.cursor < m.listTop {
		m.listTop = m.cursor
	}
	if m.cursor >= m.listTop+h {
		m.listTop = m.cursor - h + 1
	}
	if m.listTop < 0 {
		m.listTop = 0
	}
}

// ---------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------

func (m *browseModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		g := m.geometry()
		m.detail.Width = g.rightWidth - 2
		m.detail.Height = g.detailInner

		// sessionRowWidth (and so matchText) is derived from m.width, so a
		// resize can change which rows a text filter in effect keeps, not
		// only how much of them is drawn - the whole point of matching
		// against the fitted row (defect 1 of the terminal-width audit).
		// Rebuilding here, rather than waiting for the next keystroke in
		// the filter prompt, is what keeps m.visible answering "what
		// matches at the width now on screen" rather than "what matched at
		// whatever width was current when the filter was last typed."
		m.rebuild()
		m.keepCursorVisible()

		// A resize can also drop the panel that currently has focus: the
		// whole left column collapses below minSidebarWidth. setFocus and
		// moveFocusSpatial already refuse to land focus on a panel panelDrawn
		// reports as not on screen; this is that same check on the one path
		// that changes what is drawn without the user pressing a focus key at
		// all - so j/k, /, Enter and the action menu can never keep acting on
		// something invisible until the user happens to press Tab or a digit
		// next. (A short terminal used to drop Tags on its own as well, via
		// geometry's "rest < 8" branch; the accordion layout abolished that,
		// so the width collapse is the only way a panel can disappear now.)
		// Sessions is always panelDrawn (defect 2 of the same audit), so
		// this reassignment can never itself need a further fallback.
		if !m.panelDrawn(m.focus) {
			m.focus = panelSessions
		}
		return m, nil

	case refreshDoneMsg:
		if msg.err != nil {
			m.notice = "refresh: " + msg.err.Error()
		} else {
			m.notice = "index refreshed."
		}
		m.loadAll()
		return m, nil

	case dbSwitchedMsg:
		if msg.err != nil {
			m.notice = "profile: " + msg.err.Error()
			return m, nil
		}
		m.db = msg.db
		m.profileName = msg.name
		// A profile is a different corpus, so nothing selected under the
		// old one carries over: its agents, repos, and tags are not this
		// profile's, and keeping them would silently show an empty list.
		m.agents.Sel, m.repos.Sel, m.tags.Sel = "", "", ""
		m.textFilter = ""
		m.cursor, m.listTop = 0, 0
		m.notice = "profile: " + msg.name
		m.loadAll()
		return m, nil

	case tea.KeyMsg:
		if m.mode != modeNone {
			return m.updateInputMode(msg)
		}
		if m.help {
			// Any key closes the help overlay; nothing else acts while it
			// is up, so a key pressed to dismiss it can never also fire an
			// action behind it.
			m.help = false
			return m, nil
		}
		return m.handleBrowseKey(msg)
	}
	return m, nil
}

// handleBrowseKey dispatches normal-mode keys: first the ones that mean the
// same thing everywhere, then the focused panel's own.
//
// Alt/Meta combinations are never bound to anything: the user's window
// manager reserves them, so they would never reach the program (spec
// session-search, "The browser binds no Alt combinations") - checked once,
// up front, rather than per-case, so it can never be missed by a future
// addition below.
func (m *browseModel) handleBrowseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Alt {
		return m, nil
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		m.selectedOK = false
		return m, tea.Quit
	case tea.KeyTab:
		m.moveFocus(1)
		return m, nil
	case tea.KeyShiftTab:
		m.moveFocus(-1)
		return m, nil
	case tea.KeyEnter:
		return m.activate()
	case tea.KeyEsc:
		m.clearFocused()
		return m, nil
	case tea.KeyUp:
		m.move(-1)
		return m, nil
	case tea.KeyDown:
		m.move(1)
		return m, nil
	case tea.KeyCtrlU:
		m.move(-m.halfScreen())
		return m, nil
	case tea.KeyCtrlD:
		m.move(m.halfScreen())
		return m, nil
	case tea.KeyRunes:
	default:
		return m, nil
	}

	switch string(msg.Runes) {
	case "j":
		m.move(1)
	case "k":
		m.move(-1)
	case "g":
		m.moveTo(0)
	case "G":
		m.moveTo(1 << 30)
	case "1", "2", "3", "4":
		n, _ := strconv.Atoi(string(msg.Runes))
		m.setFocus(panelID(n - 1))
	case "0":
		// Sessions' jump key (change spatial-panel-navigation, renumbered
		// from 5): not panelID(n-1) like the others above, since 0 is not
		// one more than panelSessions' position in the iota - it is a
		// digit chosen for the panel used most, not for where it sits in
		// the column.
		m.setFocus(panelSessions)
	case "H":
		m.moveFocusSpatial(dirLeft)
	case "J":
		m.moveFocusSpatial(dirDown)
	case "K":
		m.moveFocusSpatial(dirUp)
	case "L":
		m.moveFocusSpatial(dirRight)
	case "q":
		m.selectedOK = false
		return m, tea.Quit
	case "?":
		m.help = true
	case "x":
		return m.openMenu()
	case "/":
		return m.startInput(modeFilter)
	case "s":
		return m.startInput(modeSearchPhrase)
	case "X":
		m.clearAllFilters()
	case "R":
		m.notice = "refreshing index..."
		return m, m.refreshCmd()
	case "[":
		m.cycleTab(-1)
	case "]":
		m.cycleTab(1)
	case "n":
		m.jumpToHit(1)
	case "N":
		m.jumpToHit(-1)
	case "m":
		return m.startInput(modeAddTag)
	case "M":
		return m.startInput(modeRemoveTag)
	case "c":
		return m.startInput(modeAddComment)
	case "C":
		return m.startInput(modeRemoveComment)
	case "a":
		m.toggleArchive()
	case ".":
		m.toggleShowAll()
	}
	return m, nil
}

// panelDrawn reports whether p occupies space in the layout for the focus
// that is already active. Resize recovery asks exactly that question: after
// a new terminal size arrives, it needs to know whether the cursor it has
// kept is still represented on screen.
func (m *browseModel) panelDrawn(p panelID) bool {
	return m.panelDrawnWithFocus(p, m.focus)
}

// panelDrawnWhenFocused asks the different question focus movement needs:
// whether p will occupy space after it becomes focused. The two layouts can
// differ at a short stacked height, because the focused panel is the one
// narrowHeights protects and an unfocused tail panel may yield its header to
// it. Looking at the current geometry there would answer "is p visible
// before this move?" and then leave focus on a panel that only became
// invisible because the move succeeded.
func (m *browseModel) panelDrawnWhenFocused(p panelID) bool {
	return m.panelDrawnWithFocus(p, p)
}

func (m *browseModel) panelDrawnWithFocus(p, focus panelID) bool {
	if p < 0 || p >= numPanels || focus < 0 || focus >= numPanels {
		return false
	}
	// geometry derives the accordion from focus. A value copy changes only
	// that input and keeps this predicate observational: probing a candidate
	// must not briefly mutate the cursor or any panel state on the live
	// model, even though Bubble Tea currently serialises Update calls.
	candidate := *m
	candidate.focus = focus
	g := candidate.geometry()
	switch p {
	case panelProfiles:
		return g.profilesH > 0
	case panelAgents:
		return g.agentsH > 0
	case panelRepos:
		return g.reposH > 0
	case panelTags:
		return g.tagsH > 0
	case panelSessions:
		return g.sessionsH > 0
	case panelDetail:
		return g.detailH > 0
	}
	return false
}

// setFocus moves focus to p directly, refusing a panel absent from p's own
// destination layout - the movement keys must never point a cursor at
// something invisible after the move completes.
func (m *browseModel) setFocus(p panelID) {
	if !m.panelDrawnWhenFocused(p) {
		m.notice = "that panel isn't shown at this terminal size."
		return
	}
	m.focus = p
	m.notice = ""
}

// moveFocus cycles focus by delta over panels their own destination layouts
// draw. Tab therefore shares setFocus's rule instead of briefly selecting a
// panel whose current geometry happens to contain a header that its focused
// geometry cannot keep.
func (m *browseModel) moveFocus(delta int) {
	for range int(numPanels) {
		next := panelID((int(m.focus) + delta + int(numPanels)) % int(numPanels))
		if m.panelDrawnWhenFocused(next) {
			m.focus = next
			break
		}
	}
	m.notice = ""
}

// direction is a screen direction for the H/J/K/L spatial moves below -
// distinct from the plain int delta move/moveTo take, which shift a cursor
// *within* the focused panel rather than choose which panel is focused.
type direction int

const (
	dirLeft direction = iota
	dirRight
	dirUp
	dirDown
)

// moveFocusSpatial moves focus by screen geometry rather than by cycling -
// moveFocus (Tab) does the cyclic version. It never wraps: reaching an edge
// is the point of the feature, so K on Profiles, J on Tags, and L on
// Sessions or Detail simply do nothing, the same way move() does nothing
// past the first or last row of a list.
//
// H always resolves to Profiles specifically, never to whichever left panel
// last had focus or looks vertically nearest to Sessions' or Detail's
// cursor. That "nearest panel" rule would be one small variety of clever
// per keypress; one fixed destination is what a user can predict from
// muscle memory without checking the screen first, and the user who asked
// for this said as much directly.
//
// Each branch below is an ordered chain of candidates - normally one panel,
// but J/K's chains run all the way to the far end of the left column - and
// focus goes to the first candidate whose destination layout draws it. That
// is what lets a move continue past a panel the current frame has dropped
// when focusing it would restore its protected header, rather than deciding
// the move against geometry that ceases to exist the moment it succeeds.
func (m *browseModel) moveFocusSpatial(dir direction) {
	var candidates []panelID
	switch {
	case dir == dirLeft && (m.focus == panelSessions || m.focus == panelDetail):
		candidates = []panelID{panelProfiles}
	case dir == dirRight && m.focus.isLeftColumn():
		candidates = []panelID{panelSessions}
	case dir == dirDown:
		switch m.focus {
		case panelProfiles:
			candidates = []panelID{panelAgents, panelRepos, panelTags}
		case panelAgents:
			candidates = []panelID{panelRepos, panelTags}
		case panelRepos:
			candidates = []panelID{panelTags}
		case panelSessions:
			candidates = []panelID{panelDetail}
		}
	case dir == dirUp:
		switch m.focus {
		case panelAgents:
			candidates = []panelID{panelProfiles}
		case panelRepos:
			candidates = []panelID{panelAgents, panelProfiles}
		case panelTags:
			candidates = []panelID{panelRepos, panelAgents, panelProfiles}
		case panelDetail:
			candidates = []panelID{panelSessions}
		}
	}
	for _, p := range candidates {
		if m.panelDrawnWhenFocused(p) {
			m.focus = p
			m.notice = ""
			return
		}
	}
}

// move shifts the focused panel's cursor by delta.
func (m *browseModel) move(delta int) {
	switch m.focus {
	case panelSessions:
		m.cursor = clampIndex(m.cursor+delta, len(m.visible))
		m.keepCursorVisible()
	case panelDetail:
		if delta < 0 {
			m.detail.LineUp(-delta)
		} else {
			m.detail.LineDown(delta)
		}
	default:
		if f := m.facetFor(m.focus); f != nil {
			f.cursor = clampIndex(f.cursor+delta, len(f.rows))
			f.keepVisible(m.facetInnerHeight(m.focus))
		}
	}
}

// moveTo jumps the focused panel's cursor to an absolute index, clamped -
// what g and G do.
func (m *browseModel) moveTo(idx int) {
	switch m.focus {
	case panelSessions:
		m.cursor = clampIndex(idx, len(m.visible))
		m.listTop = 0
		m.keepCursorVisible()
	case panelDetail:
		if idx == 0 {
			m.detail.GotoTop()
		} else {
			m.detail.GotoBottom()
		}
	default:
		if f := m.facetFor(m.focus); f != nil {
			f.cursor = clampIndex(idx, len(f.rows))
			f.top = 0
			f.keepVisible(m.facetInnerHeight(m.focus))
		}
	}
}

// halfScreen is the number of rows Ctrl-D/Ctrl-U move by in the focused
// panel - half of that panel's height, by vim convention.
func (m *browseModel) halfScreen() int {
	h := m.geometry().sessionsInner
	if m.focus != panelSessions {
		h = m.facetInnerHeight(m.focus)
	}
	if h /= 2; h < 1 {
		h = 1
	}
	return h
}

// activate is Enter: what it does depends entirely on which panel has
// focus, which is the whole navigation model in one function.
func (m *browseModel) activate() (tea.Model, tea.Cmd) {
	switch m.focus {
	case panelSessions:
		if it := m.current(); it != nil {
			m.selected = *it
			m.selectedOK = true
			return m, tea.Quit
		}
	case panelProfiles:
		row := m.profiles_.index()
		if row == nil || row.Value == "" {
			return m, nil
		}
		if row.Value == m.profileName {
			m.notice = row.Value + " is already the active profile."
			return m, nil
		}
		return m, m.switchProfileCmd(row.Value)
	case panelAgents, panelRepos, panelTags:
		f := m.facetFor(m.focus)
		row := f.index()
		if row == nil {
			return m, nil
		}
		f.Sel = row.Value // the "all" row carries "", which is "no filter"
		m.cursor, m.listTop = 0, 0
		m.rebuild()
	}
	return m, nil
}

// clearFocused is Esc outside of a prompt: it undoes whatever the focused
// panel contributes to the current view, and nothing else. Esc on Sessions
// clears the text filter, which is the thing typed into that panel.
func (m *browseModel) clearFocused() {
	switch m.focus {
	case panelSessions:
		m.textFilter = ""
		m.rebuild()
	case panelAgents, panelRepos, panelTags:
		f := m.facetFor(m.focus)
		if f.Sel == "" {
			return
		}
		f.Sel = ""
		m.cursor, m.listTop = 0, 0
		m.rebuild()
	}
}

func (m *browseModel) clearAllFilters() {
	m.agents.Sel, m.repos.Sel, m.tags.Sel = "", "", ""
	m.textFilter = ""
	m.cursor, m.listTop = 0, 0
	if m.query != "" {
		m.query = ""
		m.loadAll()
		return
	}
	m.rebuild()
}

// toggleArchive archives the selected session if it is unarchived and
// unarchives it if it is archived, then reloads so the row leaves (or
// rejoins) the list and its archive state is drawn correctly. The archive
// flag is a decision the user made, stored on the durable lineage exactly
// like the `archive` command line stores it, so a full index rebuild
// cannot undo it. Without a session selected there is nothing to act on,
// and the key does nothing.
func (m *browseModel) toggleArchive() {
	it := m.current()
	if it == nil {
		return
	}
	archived, err := annotate.IsArchived(m.db, it.LineageID)
	if err != nil {
		m.notice = "archive: " + err.Error()
		return
	}
	verb := "archived"
	if archived {
		verb = "unarchived"
		err = annotate.Unarchive(m.db, it.LineageID)
	} else {
		err = annotate.Archive(m.db, it.LineageID)
	}
	if err != nil {
		m.notice = "archive: " + err.Error()
		return
	}
	// The row's archive state changed but the corpus is otherwise intact,
	// so the notice is what says what happened before the reload hides or
	// marks the row.
	m.notice = fmt.Sprintf("%s %s.", verb, it.SessionID)
	m.detailOf = ""
	m.loadAll()
}

// toggleShowAll flips the browser between applying and ignoring the hide
// rules and the archive flag - the "." key. The selection follows the same
// session across the reload when it survives it, so toggling never dumps
// the cursor onto an unrelated row.
func (m *browseModel) toggleShowAll() {
	keep := ""
	if it := m.current(); it != nil {
		keep = it.LineageID
	}
	m.showAll = !m.showAll
	m.loadAll()
	if keep == "" {
		return
	}
	for i, it := range m.visible {
		if it.LineageID == keep {
			m.cursor = i
			m.keepCursorVisible()
			return
		}
	}
}

func (m *browseModel) cycleTab(delta int) {
	m.tab = detailTab((int(m.tab) + delta + int(numDetailTabs)) % int(numDetailTabs))
	m.detailOf = "" // force a re-render of the pane's content
	m.detail.GotoTop()
}

// jumpToHit scrolls the detail pane to the next (delta > 0) or previous
// match of the active search phrase in the transcript - n and N.
//
// The hit offsets come from the render that produced the lines currently on
// screen (see conversationCache), so this reads them rather than
// recomputing: recomputing would need the pane's width, which only the
// layout knows, and a jump measured against the wrong width lands in the
// wrong place.
func (m *browseModel) jumpToHit(delta int) {
	if m.tab != tabTranscript {
		m.notice = "n and N step through matches on the Transcript tab."
		return
	}
	phrase := m.transcriptPhrase()
	if phrase == "" {
		m.notice = "No search phrase to step through - press s to search, or / to narrow."
		return
	}
	hits := m.convo.hits
	if len(hits) == 0 {
		m.notice = fmt.Sprintf("No match for %q in the part of the transcript that was read.", phrase)
		return
	}

	// The pane is scrolled to a line, not to a hit, so "next" is the first
	// hit strictly below the top line and "previous" the last one strictly
	// above it. Stepping past either end wraps, which is what makes n
	// alone enough to walk every match.
	target := -1
	if delta > 0 {
		for _, h := range hits {
			if h > m.detail.YOffset {
				target = h
				break
			}
		}
		if target < 0 {
			target = hits[0]
		}
	} else {
		for i := len(hits) - 1; i >= 0; i-- {
			if hits[i] < m.detail.YOffset {
				target = hits[i]
				break
			}
		}
		if target < 0 {
			target = hits[len(hits)-1]
		}
	}
	m.detail.SetYOffset(target)
	m.notice = fmt.Sprintf("%d matches for %q.", len(hits), phrase)
}

func clampIndex(i, n int) int {
	if i >= n {
		i = n - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

// keepVisible scrolls a facet panel's window so its cursor row is shown.
func (f *facet) keepVisible(h int) {
	if h <= 0 {
		return
	}
	if f.cursor < f.top {
		f.top = f.cursor
	}
	if f.cursor >= f.top+h {
		f.top = f.cursor - h + 1
	}
	if f.top < 0 {
		f.top = 0
	}
}

// ---------------------------------------------------------------------
// Input modes and the action menu
// ---------------------------------------------------------------------

// startInput begins a free-text input mode: the prompt is drawn by the
// interface, and the text input owns the keyboard until submitted or
// cancelled. The returned command starts the input cursor blinking.
func (m *browseModel) startInput(mode inputMode) (tea.Model, tea.Cmd) {
	m.mode = mode
	m.inputLabel = mode.prompt()
	m.input.Prompt = ""
	m.input.Placeholder = ""
	m.input.SetValue("")
	m.input.Width = m.width - len(m.inputLabel) - 2
	if m.input.Width < 10 {
		m.input.Width = 10
	}
	m.notice = ""
	return m, m.input.Focus()
}

func (m *browseModel) updateInputMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.mode = modeNone
			m.input.Blur()
			m.notice = ""
			return m, nil
		case tea.KeyEnter:
			if m.mode == modeMenu {
				return m, m.runMenuSelection()
			}
			return m, m.submitInput(strings.TrimSpace(m.input.Value()))
		case tea.KeyUp:
			if m.mode == modeMenu {
				m.menuCursor = clampIndex(m.menuCursor-1, len(m.menuFiltered))
				return m, nil
			}
		case tea.KeyDown:
			if m.mode == modeMenu {
				m.menuCursor = clampIndex(m.menuCursor+1, len(m.menuFiltered))
				return m, nil
			}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	switch m.mode {
	case modeMenu:
		m.refilterMenu()
	case modeFilter:
		// The narrowing filter applies as it is typed, which is what makes
		// it feel like filtering rather than like filling in a form.
		m.applyFilterText(m.input.Value())
	}
	return m, cmd
}

// applyFilterText routes / to whatever the focused panel narrows by: the
// session list's text filter, or a facet panel's own row filter.
func (m *browseModel) applyFilterText(v string) {
	if f := m.facetFor(m.focus); f != nil {
		f.filter = v
		m.rebuild()
		return
	}
	m.textFilter = v
	m.cursor, m.listTop = 0, 0
	m.rebuild()
}

// submitInput applies the submitted value for the current input mode. An
// empty value applies nothing (spec session-search, "Submitting an empty
// value"): clearing a filter is done by submitting a blank line to that
// filter's prompt - an explicit action, not an accident.
func (m *browseModel) submitInput(value string) tea.Cmd {
	mode := m.mode
	m.mode = modeNone
	m.input.Blur()

	switch mode {
	case modeFilter:
		m.applyFilterText(value)
		return nil
	case modeSearchPhrase:
		m.query = value
		m.cursor, m.listTop = 0, 0
		m.loadAll()
		return nil
	case modeAddTag:
		if value != "" {
			if it := m.current(); it != nil {
				if err := annotate.AddTag(m.db, it.LineageID, value); err != nil {
					m.notice = "tag: " + err.Error()
					return nil
				}
			}
		}
	case modeRemoveTag:
		if value != "" {
			if it := m.current(); it != nil {
				if err := annotate.RemoveTag(m.db, it.LineageID, value); err != nil {
					m.notice = "tag: " + err.Error()
					return nil
				}
			}
		}
	case modeAddComment:
		if value != "" {
			if it := m.current(); it != nil {
				if err := annotate.AddComment(m.db, it.LineageID, value); err != nil {
					m.notice = "comment: " + err.Error()
					return nil
				}
			}
		}
	case modeRemoveComment:
		if value != "" {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				m.notice = fmt.Sprintf("%q is not a comment id", value)
				return nil
			}
			if err := annotate.RemoveComment(m.db, id); err != nil {
				m.notice = "comment: " + err.Error()
				return nil
			}
		}
	}
	// An annotation edit changes the tag facet and the rows themselves, so
	// the corpus is re-read rather than re-sliced.
	m.detailOf = ""
	m.loadAll()
	return nil
}

// menuAction is one entry in the action menu: a label, and what pressing
// Enter on it does.
type menuAction struct {
	label string
	run   func(*browseModel) tea.Cmd
}

// menuFor is the action menu's contents for the focused panel. Every entry
// here is also a key binding; the menu exists so the actions available
// right now can be *read* rather than recalled, which is the job `x` does
// in lazygit.
func (m *browseModel) menuFor(p panelID) []menuAction {
	var out []menuAction
	switch p {
	case panelSessions:
		out = append(out,
			menuAction{"resume this session", func(m *browseModel) tea.Cmd {
				mod, cmd := m.activate()
				*m = *mod.(*browseModel)
				return cmd
			}},
			menuAction{"add a tag", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeAddTag); return c }},
			menuAction{"remove a tag", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeRemoveTag); return c }},
			menuAction{"add a comment", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeAddComment); return c }},
			menuAction{"remove a comment", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeRemoveComment); return c }},
			menuAction{"filter these sessions", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeFilter); return c }},
		)
	case panelProfiles:
		out = append(out, menuAction{"switch to this profile", func(m *browseModel) tea.Cmd {
			mod, cmd := m.activate()
			*m = *mod.(*browseModel)
			return cmd
		}})
	case panelAgents, panelRepos, panelTags:
		out = append(out,
			menuAction{"filter by this " + strings.TrimSuffix(strings.ToLower(p.title()), "s"), func(m *browseModel) tea.Cmd {
				mod, cmd := m.activate()
				*m = *mod.(*browseModel)
				return cmd
			}},
			menuAction{"clear this filter", func(m *browseModel) tea.Cmd { m.clearFocused(); return nil }},
			menuAction{"narrow this panel", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeFilter); return c }},
		)
	}
	// Always available, listed last so the panel's own actions lead.
	return append(out,
		menuAction{"set the search phrase", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeSearchPhrase); return c }},
		menuAction{"clear all filters", func(m *browseModel) tea.Cmd { m.clearAllFilters(); return nil }},
		menuAction{"refresh the index", func(m *browseModel) tea.Cmd { m.notice = "refreshing index..."; return m.refreshCmd() }},
		menuAction{"show all key bindings", func(m *browseModel) tea.Cmd { m.help = true; return nil }},
	)
}

func (m *browseModel) openMenu() (tea.Model, tea.Cmd) {
	m.menuAll = m.menuFor(m.focus)
	m.menuFiltered = m.menuAll
	m.menuCursor = 0
	return m.startInput(modeMenu)
}

func (m *browseModel) refilterMenu() {
	q := m.input.Value()
	if q == "" {
		m.menuFiltered = m.menuAll
		m.menuCursor = clampIndex(m.menuCursor, len(m.menuFiltered))
		return
	}
	// Literal and case-insensitive, the same rule the session list and the
	// facet panels narrow by (change literal-substring-filter): one
	// "type to narrow" gesture that means one thing everywhere, rather than
	// a menu that answers to different matching than the list behind it.
	needle := strings.ToLower(q)
	out := make([]menuAction, 0, len(m.menuAll))
	for _, a := range m.menuAll {
		if strings.Contains(strings.ToLower(a.label), needle) {
			out = append(out, a)
		}
	}
	m.menuFiltered = out
	m.menuCursor = 0
}

// runMenuSelection closes the menu and runs the highlighted action. Unlike
// the value-selection prompt it replaced, the menu always has a highlighted
// entry: every entry is a verb the user asked for by opening the menu, so
// there is no "apply nothing" position to defend - Esc is that.
func (m *browseModel) runMenuSelection() tea.Cmd {
	m.mode = modeNone
	m.input.Blur()
	if m.menuCursor < 0 || m.menuCursor >= len(m.menuFiltered) {
		return nil
	}
	return m.menuFiltered[m.menuCursor].run(m)
}

// ---------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------

// switchProfileCmd resolves name to a profile and opens that profile's own
// database (one file per profile - sessions from two profiles can never
// share a result set), reporting the new connection back into the model.
// The new profile's index is refreshed only by the explicit refresh
// action, exactly like the initial profile (design.md decision 5: refresh
// once on open, never per interaction).
func (m browseModel) switchProfileCmd(name string) tea.Cmd {
	return func() tea.Msg {
		p, err := m.resolve(name)
		if err != nil {
			return dbSwitchedMsg{err: err}
		}
		r, err := refresh.New(p, "")
		if err != nil {
			return dbSwitchedMsg{err: err}
		}
		return dbSwitchedMsg{name: p.Name, db: r.DB}
	}
}

// refreshCmd is the explicit index refresh action (spec session-search,
// "Implement an explicit index refresh without leaving the browser"): it
// re-runs the refresh pass for the active profile and reports back into
// the model, which then reloads its rows.
func (m browseModel) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		p, err := m.resolve(m.profileName)
		if err != nil {
			return refreshDoneMsg{err: err}
		}
		r, err := refresh.New(p, "")
		if err != nil {
			return refreshDoneMsg{err: err}
		}
		_, err = r.Refresh(refresh.Options{})
		return refreshDoneMsg{err: err}
	}
}

// ---------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------

// minSidebarWidth is the terminal width below which the side panels are
// not drawn at all. Under it there is no width left for a session row
// after a usable sidebar, and a sidebar that squeezes the rows it exists
// to filter is worse than no sidebar: the Sessions and Detail panes take
// the whole screen instead, which is the pre-panel layout and still a
// complete interface.
const minSidebarWidth = 76

// geometry is the whole frame's arithmetic in one place. Every height here
// is an *outer* height, borders included, and the panels in each column
// sum to exactly bodyHeight - if they did not, the rendered frame would be
// taller than the terminal and the terminal would scroll it, silently
// pushing the top panel off the screen. That failure mode is invisible to
// a unit test comparing strings, which is why it is computed once here
// rather than per-panel at draw time. A collapsed side panel's height of 1
// is that panel's header line (panelBox's Collapsed rendering); it needs no
// border arithmetic of its own, which is what lets four panels always fit.
type geometry struct {
	sidebar       bool
	leftWidth     int
	rightWidth    int
	bodyHeight    int
	profilesH     int
	agentsH       int
	reposH        int
	tagsH         int
	sessionsH     int
	detailH       int
	sessionsInner int
	detailInner   int
}

func (m browseModel) geometry() geometry {
	g := geometry{}

	// One line for the footer, and one more for the prompt when a prompt
	// is open. A negative body is not a minimum layout: it is no layout at
	// all. Raising it to the old six-row floor made the frame visibly taller
	// than the terminal that could not pay for it, the precise scroll-off-
	// screen failure this function exists to prevent.
	g.bodyHeight = m.height - 1
	if m.mode != modeNone {
		g.bodyHeight--
	}
	if g.bodyHeight < 0 {
		g.bodyHeight = 0
	}

	g.sidebar = m.width >= minSidebarWidth
	if g.sidebar {
		g.leftWidth = m.width * 3 / 10
		if g.leftWidth < 22 {
			g.leftWidth = 22
		}
		if g.leftWidth > 34 {
			g.leftWidth = 34
		}
	}
	g.rightWidth = m.width - g.leftWidth

	if g.sidebar {
		g.wideHeights(m)
	} else {
		g.narrowHeights(m.focus)
	}
	g.sessionsInner = maxInt(g.sessionsH-2, 0)
	g.detailInner = maxInt(g.detailH-2, 0)
	return g
}

// wideHeights divides the two independently stacked columns without asking
// either to honour a minimum the terminal has not supplied. At ordinary
// heights the accordion and 3:2 split are unchanged; below their floors,
// panels disappear from the ends of their draw orders until each column
// totals bodyHeight exactly. A focused side panel is the exception: it keeps
// the one line that says where the cursor is, even when an unfocused earlier
// panel must give its line up instead.
func (g *geometry) wideHeights(m browseModel) {
	focus := m.focus
	if g.bodyHeight <= 0 {
		return
	}

	expanded := panelRepos
	switch {
	case focus.isLeftColumn():
		expanded = focus
	case m.agents.Sel != "":
		expanded = panelAgents
	case m.repos.Sel != "":
		expanded = panelRepos
	case m.tags.Sel != "":
		expanded = panelTags
	}

	// Accordion only when the column cannot afford four panels at once.
	// Profiles and Agents are short, known-length lists and need only what
	// they hold; if what is left over still gives Repos and Tags a usable
	// window each, every panel shows content and nothing collapses. That is
	// the sizing this browser had before the accordion, and it is the right
	// one whenever there is room: a wide, tall terminal has space for all
	// four, and collapsing three of them there hides dimensions the user
	// could otherwise read at a glance without pressing anything.
	//
	// The accordion below is for the case that sizing could not handle -
	// the old "rest < 8" branch, which dropped Tags outright. Collapsing a
	// panel to a header line is strictly better than deleting it, but it is
	// a concession to a short column, not an improvement on a roomy one.
	if room := g.bodyHeight - boxHeight(len(m.profiles_.rows), 1, 4) - boxHeight(len(m.agents.rows), 1, 5); room >= 8 {
		g.profilesH = boxHeight(len(m.profiles_.rows), 1, 4)
		g.agentsH = boxHeight(len(m.agents.rows), 1, 5)
		g.reposH = room * 3 / 5
		g.tagsH = room - g.reposH
	} else if g.bodyHeight >= 4 {
		g.profilesH, g.agentsH, g.reposH, g.tagsH = 1, 1, 1, 1
		switch expanded {
		case panelProfiles:
			g.profilesH = g.bodyHeight - 3
		case panelAgents:
			g.agentsH = g.bodyHeight - 3
		case panelRepos:
			g.reposH = g.bodyHeight - 3
		case panelTags:
			g.tagsH = g.bodyHeight - 3
		}
	} else {
		heights := [numPanels]int{
			panelProfiles: 1, panelAgents: 1, panelRepos: 1, panelTags: 1,
		}
		used := 4
		for _, p := range [...]panelID{panelTags, panelRepos, panelAgents, panelProfiles} {
			if used <= g.bodyHeight || (p == expanded && focus.isLeftColumn()) {
				continue
			}
			heights[p]--
			used--
		}
		g.profilesH = heights[panelProfiles]
		g.agentsH = heights[panelAgents]
		g.reposH = heights[panelRepos]
		g.tagsH = heights[panelTags]
	}

	// Right column: there is enough room for both panels once bodyHeight
	// reaches two. At one row the focused Detail keeps its tab header;
	// every other focus leaves the row with Sessions, the browser's primary
	// panel. Neither case hands a negative height to a renderer.
	if g.bodyHeight == 1 {
		if focus == panelDetail {
			g.detailH = 1
		} else {
			g.sessionsH = 1
		}
		return
	}
	g.sessionsH = g.bodyHeight * 3 / 5
	if g.sessionsH < 1 {
		g.sessionsH = 1
	}
	g.detailH = g.bodyHeight - g.sessionsH
	if g.detailH < 1 {
		g.detailH = 1
		g.sessionsH = g.bodyHeight - 1
	}
}

// narrowHeights lays the whole frame out as one column, for a terminal too
// narrow to put the side panels beside the session list (change
// stack-panels-when-narrow).
//
// The alternative it replaces was to draw no side panels at all below
// minSidebarWidth, which did not narrow the interface so much as amputate
// it: four of the six panels were unreachable, and the only thing the
// program would say about it was "that panel isn't shown at this terminal
// size". Width is the scarce resource in that situation, and a stack needs
// exactly as much width as its widest member - so stacking costs nothing
// that was actually short, and buys back every dimension the user had lost.
// This is what lazygit does at the same threshold, for the same reason.
//
// Rows start with Sessions' three-line floor and one header for every other
// panel. When the frame cannot fund all eight rows, it takes them back from
// the end of the draw order - Detail, Tags, Repos, Agents, Profiles, then
// Sessions' surplus - while never taking the focused panel's last line.
// That order is not cosmetic: losing the panel the user just chose while an
// unfocused Detail header remains is worse than losing the header, and it
// leaves movement keys operating on a cursor the frame does not show.
//
// Once those floors fit, the focused panel grows to a readable quarter of
// the frame and Sessions receives the rest. Sessions therefore remains the
// primary list at practical sizes without making an impossible floor spill
// the frame past a tiny terminal.
func (g *geometry) narrowHeights(focus panelID) {
	if g.bodyHeight <= 0 {
		return
	}

	// Draw order is Profiles, Agents, Repos, Tags, Sessions, Detail - the
	// digit order with Sessions' 0 last, which is also where lazygit puts
	// its own [0] panel. Detail collapses like the rest: it is content for
	// the selected row, so it earns its rows only when it is what the user
	// is reading.
	heights := [numPanels]int{
		panelProfiles: 1,
		panelAgents:   1,
		panelRepos:    1,
		panelTags:     1,
		panelSessions: 3,
		panelDetail:   1,
	}
	used := 8
	for _, p := range [...]panelID{panelDetail, panelTags, panelRepos, panelAgents, panelProfiles, panelSessions} {
		if p == focus {
			continue
		}
		for used > g.bodyHeight && heights[p] > 0 {
			heights[p]--
			used--
		}
	}
	if used > g.bodyHeight {
		// This is possible only when Sessions itself is focused at one or
		// two body rows. Its three-row preference is expendable; its last
		// line is not, because focus must remain represented whenever the
		// terminal can show any panel at all.
		take := minInt(used-g.bodyHeight, heights[focus]-1)
		heights[focus] -= take
		used -= take
	}

	remaining := g.bodyHeight - used
	if focus != panelSessions {
		want := g.bodyHeight / 4
		if want < 3 {
			want = 3
		}
		if extra := want - heights[focus]; extra > 0 {
			extra = minInt(extra, remaining)
			heights[focus] += extra
			remaining -= extra
		}
	}
	heights[panelSessions] += remaining

	g.profilesH = heights[panelProfiles]
	g.agentsH = heights[panelAgents]
	g.reposH = heights[panelRepos]
	g.tagsH = heights[panelTags]
	g.sessionsH = heights[panelSessions]
	g.detailH = heights[panelDetail]
}

// boxHeight is the outer height a panel needs to show n rows, clamped to
// between min and max content rows. Used by the roomy left-column sizing,
// where a panel takes only what its list actually holds.
func boxHeight(n, minRows, maxRows int) int {
	if n < minRows {
		n = minRows
	}
	if n > maxRows {
		n = maxRows
	}
	return n + 2
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// facetInnerHeight is how many rows the given side panel can show - what
// the movement keys page by.
func (m browseModel) facetInnerHeight(p panelID) int {
	g := m.geometry()
	switch p {
	case panelProfiles:
		return maxInt(g.profilesH-2, 0)
	case panelAgents:
		return maxInt(g.agentsH-2, 0)
	case panelRepos:
		return maxInt(g.reposH-2, 0)
	case panelTags:
		return maxInt(g.tagsH-2, 0)
	}
	return 0
}

// ---------------------------------------------------------------------
// View
// ---------------------------------------------------------------------

// rowDecorationWidth is the number of columns every list draws in front of
// a row's own content: the two-column selection marker on the selected row
// and equal padding on every other row. The width handed to row rendering
// must already exclude this rather than have it added on afterwards -
// trimming a styled row after the fact risks cutting inside an ANSI escape
// sequence (change fix-row-width-budget).
const rowDecorationWidth = 2

// facetDecorationWidth is the same budget for a side panel's rows, which
// carry one column more: the cursor mark, the "this value is applied" mark,
// and a separating space.
const facetDecorationWidth = 3

func (m browseModel) View() string {
	if m.help {
		return m.helpView()
	}
	g := m.geometry()

	var body string
	if g.sidebar {
		left := stackPanels(
			m.facetPanel(panelProfiles, &m.profiles_, g.leftWidth, g.profilesH, ""),
			m.facetPanel(panelAgents, &m.agents, g.leftWidth, g.agentsH, m.agents.Sel),
			m.facetPanel(panelRepos, &m.repos, g.leftWidth, g.reposH, m.repos.Sel),
			m.facetPanel(panelTags, &m.tags, g.leftWidth, g.tagsH, m.tags.Sel),
		)
		body = joinColumns(left, stackPanels(m.sessionsPanel(g), m.detailPanel(g)))
	} else {
		// Too narrow for two columns: the same panels, stacked into one,
		// at full width (change stack-panels-when-narrow). Nothing is
		// dropped - narrowHeights collapses the ones the user is not in.
		// Sessions sits after the facets and before Detail, which is the
		// digit order (1 2 3 4 then 0) and puts the list next to the
		// detail that describes its selected row.
		body = stackPanels(
			m.facetPanel(panelProfiles, &m.profiles_, g.rightWidth, g.profilesH, ""),
			m.facetPanel(panelAgents, &m.agents, g.rightWidth, g.agentsH, m.agents.Sel),
			m.facetPanel(panelRepos, &m.repos, g.rightWidth, g.reposH, m.repos.Sel),
			m.facetPanel(panelTags, &m.tags, g.rightWidth, g.tagsH, m.tags.Sel),
			m.sessionsPanel(g),
			m.detailPanel(g),
		)
	}

	// The action menu is a popup over the frame, not a pane that displaces
	// it: it is a momentary question about the panel you are already
	// looking at, and moving the layout out from under that question is
	// exactly the wrong answer to it.
	if m.mode == modeMenu {
		body = overlay(body, m.menuPopup())
	}

	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n")
	if m.mode != modeNone {
		fmt.Fprintf(&b, "%s%s\n", m.inputLabel, m.input.View())
	}
	b.WriteString(m.footer())
	return b.String()
}

// menuPopupWidth is the popup's width: wide enough for the longest action
// label the menu ever offers, narrow enough to leave the frame underneath
// recognisable.
const menuPopupWidth = 34

// menuPopup renders the action menu as a bordered box to be composited over
// the frame. It sizes itself to what it is offering, so a menu narrowed by
// typing shrinks to the matches rather than leaving the frame covered by
// blank rows.
func (m browseModel) menuPopup() string {
	width := minInt(menuPopupWidth, maxInt(m.width-4, 12))
	rows := minInt(maxInt(len(m.menuFiltered), 1), maxInt(m.height-6, 3))
	box := panelBox{
		Title:   m.focus.title() + " actions",
		Width:   width,
		Height:  rows + 2,
		Focused: true,
		Style:   m.style,
	}
	if len(m.menuFiltered) == 0 {
		box.Lines = append(box.Lines, style(" no matching action", ansiDim, m.style))
		return box.render()
	}
	top := 0
	if m.menuCursor >= rows {
		top = m.menuCursor - rows + 1
	}
	for i := top; i < minInt(top+rows, len(m.menuFiltered)); i++ {
		marker := "  "
		if i == m.menuCursor {
			marker = "▸ "
		}
		line := marker + truncateToWidth(m.menuFiltered[i].label, box.innerWidth()-rowDecorationWidth)
		if i == m.menuCursor && m.style {
			line = highlightLine(padToWidth(line, box.innerWidth()))
		}
		box.Lines = append(box.Lines, line)
	}
	return box.render()
}

// facetPanel draws one side panel. sel is the value currently applied by
// this panel, marked so the applied filter is distinguishable from wherever
// the cursor happens to be sitting - the two are independent, and conflating
// them is what makes a panel feel like it filters on hover.
func (m browseModel) facetPanel(id panelID, f *facet, width, height int, sel string) panelBox {
	box := panelBox{
		Title:   id.title(),
		Width:   width,
		Height:  height,
		Focused: m.focus == id,
		Style:   m.style,
	}
	box.Number, box.HasNumber = id.jumpKey()
	if height <= 0 {
		return box
	}
	if height == 1 {
		// A collapsed accordion header: one line, no rows, whose whole job
		// is to say what the panel is and what it currently applies. The
		// full box marks the applied value as a ● row; a header has no rows
		// to carry that mark, so the value moves into the border - without
		// it, a filtered panel and an empty one would look identical from a
		// collapsed header, which would be the panels' point turned against
		// them.
		box.Collapsed = true
		if id == panelProfiles {
			// Profiles filters nothing; its analogue of an applied value is
			// the active profile, exactly what its ● row marks when open.
			box.Value = sanitizeSingleLine(m.profileName)
		} else {
			box.Value = sanitizeSingleLine(sel)
		}
		return box
	}
	if f.filter != "" {
		box.Count = "/" + truncateToWidth(sanitizeSingleLine(f.filter), 8)
	} else if id == panelProfiles {
		box.Count = ""
	} else if sel != "" {
		box.Count = "filtered"
	}

	inner := box.innerWidth()
	h := box.innerHeight()
	f.keepVisible(h)
	end := minInt(f.top+h, len(f.rows))
	for i := f.top; i < end; i++ {
		r := f.rows[i]

		// Three columns of decoration, always: a cursor mark, an "applied"
		// mark, and a separating space. They are two independent facts -
		// where the cursor is, and which value is actually filtering - and
		// giving each its own column is what keeps them from being read as
		// one. Only the focused panel draws a pointer; an unfocused panel
		// marks its cursor row faintly, so five arrows are never on screen
		// at once competing to be the one that Enter acts on.
		cursorMark := " "
		if i == f.cursor {
			cursorMark = "·"
			if m.focus == id {
				cursorMark = "▸"
			}
		}

		// The count is right-aligned against the panel edge; the label
		// takes whatever is left. Both are sanitized: a repo path or a tag
		// is free text from a session, and a control character in it would
		// otherwise break the box open.
		count := ""
		if id != panelProfiles {
			count = strconv.Itoa(r.Count)
		}
		labelWidth := inner - facetDecorationWidth - len(count)
		if count != "" {
			labelWidth--
		}
		if labelWidth < 1 {
			labelWidth = 1
		}
		label := truncateToWidth(sanitizeSingleLine(r.Label), labelWidth)

		// An applied value, and the active profile, are marked in the text
		// itself rather than by colour alone, so the state survives
		// NO_COLOR (spec session-search, "Styling disabled by the
		// environment").
		appliedMark := " "
		if (id == panelProfiles && r.Value == m.profileName) ||
			(id != panelProfiles && r.Value != "" && r.Value == sel) {
			appliedMark = "●"
			label = style(label, ansiBold, m.style)
		}

		line := cursorMark + appliedMark + " " + label
		if count != "" {
			pad := inner - visibleWidth(line) - len(count)
			if pad < 1 {
				pad = 1
			}
			line += strings.Repeat(" ", pad) + style(count, ansiDim, m.style)
		}
		if i == f.cursor && m.focus == id && m.style {
			line = highlightLine(padToWidth(line, inner))
		}
		box.Lines = append(box.Lines, line)
	}
	if len(f.rows) == 0 {
		box.Lines = append(box.Lines, style("   (none)", ansiDim, m.style))
	}
	return box
}

// archivedMarker is the literal, colour-free tag an archived session's row
// carries when showAll is on. The archive state must not exist only as a
// colour: a NO_COLOR user gets the same information in the text (spec
// session-search, "Styling disabled by the environment").
const archivedMarker = "[archived]"

// sessionsPanel draws the session list.
func (m browseModel) sessionsPanel(g geometry) panelBox {
	box := panelBox{
		Title:   panelSessions.title(),
		Count:   m.sessionsCount(),
		Width:   g.rightWidth,
		Height:  g.sessionsH,
		Focused: m.focus == panelSessions,
		Style:   m.style,
	}
	box.Number, box.HasNumber = panelSessions.jumpKey()
	if box.Height == 1 {
		// A one-row Sessions allocation is its numbered header, not a
		// two-border box. Rendering the latter spends a row geometry did
		// not provide and is enough on its own to scroll a four-row frame.
		box.Collapsed = true
		return box
	}

	if len(m.visible) == 0 {
		box.Lines = append(box.Lines, style("  no session matches the filters in use.", ansiDim, m.style))
		return box
	}
	h := box.innerHeight()
	top := m.listTop
	if top > maxInt(len(m.visible)-h, 0) {
		top = maxInt(len(m.visible)-h, 0)
	}
	baseRowWidth := m.sessionRowWidth()
	end := minInt(top+h, len(m.visible))
	for i := top; i < end; i++ {
		it := m.visible[i]
		// The archive marker is budgeted *before* the row is shortened,
		// never appended after truncation - trimming a styled row after the
		// fact risks cutting inside an ANSI escape sequence, and a marker
		// bolted on past the budget would push the line past the panel edge
		// (change fix-row-width-budget).
		marker := ""
		if m.showAll && it.Archived {
			marker = " " + style(archivedMarker, ansiDim, m.style)
		}
		// sessionRowWidthFor shares the exact showAll/archive predicate
		// applyTextFilter uses. RenderRow and the filter therefore shorten
		// this particular row to the same text, including the eleven
		// columns an archive marker takes away, instead of agreeing only
		// for ordinary rows.
		rowOpts := RenderOptions{Width: m.sessionRowWidthFor(it, baseRowWidth), Style: m.style}

		line := RenderRow(it, rowOpts) + marker
		if i == m.cursor {
			if m.style {
				line = highlightLine(line)
			}
			line = "▸ " + line
		} else {
			line = "  " + line
		}
		box.Lines = append(box.Lines, line)
	}
	return box
}

// sessionsCount is the top-border annotation: how many sessions are listed,
// and out of how many the profile holds when that is a smaller number than
// the whole - so "am I looking at everything?" is answered without reading
// four panels. When the hide rules suppressed some of that whole, the count
// says so in the border too, so hiding is never silent in the browser any
// more than it is in the command-line header. The panel's own renderer
// drops the annotation when the border is too narrow for it, so the hidden
// count never widens the frame.
func (m browseModel) sessionsCount() string {
	count := strconv.Itoa(len(m.all))
	if len(m.visible) != len(m.all) {
		count = fmt.Sprintf("%d/%d", len(m.visible), len(m.all))
	}
	if m.hidden > 0 && !m.showAll {
		count += fmt.Sprintf(" · %d hidden", m.hidden)
	}
	return count
}

// detailPanel draws the right-hand pane: a tab strip in the top border and
// the selected session's content beneath it.
func (m browseModel) detailPanel(g geometry) panelBox {
	box := panelBox{
		Title:   m.tabStrip(),
		Width:   g.rightWidth,
		Height:  g.detailH,
		Focused: m.focus == panelDetail,
		Style:   m.style,
	}
	// One row is the collapsed height the stacked narrow layout hands out
	// (narrowHeights), and a box cannot be drawn in it: a full box is a top
	// border plus a bottom border before any content, so rendering one in a
	// single row silently produces two lines and the frame overflows the
	// terminal by exactly that much. Detail collapses to the same header
	// line the facet panels use, keeping its tab strip visible so the user
	// can still see which tab they would land on.
	if box.Height == 1 {
		box.Collapsed = true
		return box
	}
	opts := RenderOptions{Width: box.innerWidth(), Style: m.style}
	content := m.detailContent(opts)

	// The viewport owns the scrolling so a long transcript of prompts or a
	// long comment list can be read without leaving the pane.
	m.detail.Width = box.innerWidth()
	m.detail.Height = box.innerHeight()
	m.detail.SetContent(content)
	for _, line := range strings.Split(m.detail.View(), "\n") {
		box.Lines = append(box.Lines, truncateVisible(line, box.innerWidth()))
	}
	return box
}

// tabStrip is the tab row, drawn into the detail panel's top border. The
// active tab is bold; the others are dim, so the strip reads as one
// control rather than three titles.
func (m browseModel) tabStrip() string {
	parts := make([]string, 0, numDetailTabs)
	for t := detailTab(0); t < numDetailTabs; t++ {
		// The active tab is bracketed as well as bold: with NO_COLOR set,
		// bold is suppressed along with everything else, and a tab strip
		// where nothing marks the active tab is not a tab strip (spec
		// session-search, "Styling disabled by the environment").
		if t == m.tab {
			parts = append(parts, style("["+t.title()+"]", ansiBold, m.style))
		} else {
			parts = append(parts, style(" "+t.title()+" ", ansiDim, m.style))
		}
	}
	return strings.Join(parts, "")
}

func (m browseModel) detailContent(opts RenderOptions) string {
	it := m.current()
	if it == nil {
		return "No session selected."
	}
	switch m.tab {
	case tabPrompts:
		return renderItemPrompts(m.db, *it, opts)
	case tabTranscript:
		return renderItemTranscript(m.convo, *it, m.transcriptPhrase(), opts)
	case tabComments:
		return renderItemComments(m.db, *it, opts)
	}
	return renderItemDetail(m.db, *it, opts)
}

// transcriptPhrase is what the Transcript tab highlights and what n and N
// step through: the full-text search phrase when there is one, otherwise
// the row filter. Both are things the user typed to find this session, and
// the transcript is where they will want to see why it matched.
func (m browseModel) transcriptPhrase() string {
	if m.query != "" {
		return m.query
	}
	return m.textFilter
}

// footer is the bottom line: a transient notice when there is one,
// otherwise the actions available in the focused panel right now (spec
// session-search, "Common actions are visible without being requested").
func (m browseModel) footer() string {
	if m.notice != "" {
		return style(truncateToWidth(sanitizeSingleLine(m.notice), m.width), ansiDim, m.style)
	}
	var parts []string
	for _, a := range browseActions {
		if a.showsFor(m.focus) {
			parts = append(parts, a.key+" "+a.label)
		}
	}
	return style(truncateToWidth(strings.Join(parts, "  "), m.width), ansiDim, m.style)
}

// highlightLine renders a selected row in reverse video. style() emits one
// reset per styled field, so the reverse must be re-applied after every
// reset inside the line; wrapping alone would only highlight the first
// field.
func highlightLine(line string) string {
	if !strings.Contains(line, "\x1b") {
		return ansiReverse + line + ansiReset
	}
	return ansiReverse + strings.ReplaceAll(line, ansiReset, ansiReset+ansiReverse) + ansiReset
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------
// Help and the action table
// ---------------------------------------------------------------------

type browseAction struct {
	key string
	// label is the footer form: short, because the footer is one line and
	// shares it with every other action available right now. help is the
	// descriptive form for the overlay, which has room for a sentence;
	// when it is empty the label is used for both.
	label string
	help  string
	// panels, when non-empty, restricts the footer hint to those panels.
	// Empty means "everywhere". The help overlay always lists everything.
	panels []panelID
	// footer marks the small set of actions worth spending footer columns
	// on; the rest are discoverable through `x` and `?`.
	footer bool
}

// helpText is what the overlay lists: the descriptive form when there is
// one, the footer form otherwise.
func (a browseAction) helpText() string {
	if a.help != "" {
		return a.help
	}
	return a.label
}

// scope names the panels an action is specific to, for the help overlay -
// an action bound to Enter means three different things in three places,
// and a list that does not say which is a list that cannot be trusted.
func (a browseAction) scope() string {
	if len(a.panels) == 0 || len(a.panels) >= int(numPanels)-1 {
		return ""
	}
	names := make([]string, 0, len(a.panels))
	for _, p := range a.panels {
		names = append(names, p.title())
	}
	return "  (" + strings.Join(names, ", ") + ")"
}

func (a browseAction) showsFor(p panelID) bool {
	if !a.footer {
		return false
	}
	if len(a.panels) == 0 {
		return true
	}
	for _, q := range a.panels {
		if q == p {
			return true
		}
	}
	return false
}

// browseActions is the single binding table: the footer hints and the help
// overlay are both derived from it, so the keys advertised can never drift
// from the keys actually bound. Do not add, remove, or substitute a key
// here without also changing handleBrowseKey to match, and vice versa.
var browseActions = []browseAction{
	{key: "0-4", label: "jump to panel", help: "focus the panel with that number"},
	{key: "tab", label: "panel", help: "focus the next panel (shift-tab: previous)", footer: true},
	{key: "H/J/K/L", label: "move focus", help: "move focus to the panel in that screen direction; does nothing at an edge"},
	{key: "j/k, ↑/↓", label: "move in panel"},
	{key: "Ctrl-D/U", label: "move by half a panel"},
	{key: "g/G", label: "first/last row"},
	{key: "enter", label: "resume", help: "resume the selected session", panels: []panelID{panelSessions}, footer: true},
	{key: "enter", label: "filter", help: "filter the sessions by the selected value", panels: []panelID{panelAgents, panelRepos, panelTags}, footer: true},
	{key: "enter", label: "switch", help: "switch to the selected profile", panels: []panelID{panelProfiles}, footer: true},
	{key: "esc", label: "clear filter", help: "clear what this panel is filtering by", panels: []panelID{panelSessions, panelAgents, panelRepos, panelTags}, footer: true},
	{key: "[/]", label: "tab", help: "previous/next tab in the detail pane", panels: []panelID{panelSessions, panelDetail}, footer: true},
	{key: "n/N", label: "next/previous match", help: "on the Transcript tab, scroll to the next/previous occurrence of the search phrase", panels: []panelID{panelSessions, panelDetail}},
	{key: "/", label: "narrow", help: "keep only the focused panel's rows containing what you type", footer: true},
	{key: "s", label: "search phrase", help: "full-text search over your own prompts"},
	{key: "x", label: "menu", help: "action menu for the focused panel", footer: true},
	{key: "X", label: "clear all filters"},
	{key: "R", label: "refresh the index"},
	{key: "m/M", label: "add/remove a tag", help: "add/remove a tag on the selected session"},
	{key: "c/C", label: "add/remove a comment", help: "add/remove a comment on the selected session"},
	{key: "a", label: "archive", help: "archive/unarchive the selected session", footer: true},
	{key: ".", label: "show all", help: "toggle showing sessions the hide rules and the archive flag suppress", footer: true},
	{key: "?", label: "keys", help: "this list", footer: true},
	{key: "q, Ctrl-C", label: "quit", footer: true},
}

func (m browseModel) helpView() string {
	var b strings.Builder
	b.WriteString(style("lazyrecall - key bindings", ansiBold, m.style) + "\n\n")
	for _, a := range browseActions {
		fmt.Fprintf(&b, "  %-14s %s%s\n", a.key, a.helpText(), a.scope())
	}
	b.WriteString("\nPanels: 1 Profiles  2 Agents  3 Repos  4 Tags  0 Sessions  (tab reaches the detail pane)\n")
	b.WriteString("\n")
	b.WriteString(style("any key: close this help", ansiDim, m.style))
	return b.String()
}

// ---------------------------------------------------------------------
// Detail tabs
// ---------------------------------------------------------------------

// renderItemDetail renders the Detail tab for one selected session: its
// composite identifier, handle, working directory, agent, end state, name
// and topic, and its tags - styled to the same standard as a directly
// printed listing (spec session-search, "The browser is styled"), reusing
// RenderRow's own colour choices field-for-field so the detail pane and the
// row list never disagree about what a field means visually. The identifier
// and the handle are deliberately left unstyled: they are the copyable,
// plain-text forms the non-interactive commands accept.
//
// Comments moved to their own tab (change lazy-style-browser); db is kept
// in the signature because every tab is dispatched through one function
// and a caller should not have to know which tab needs the database.
func renderItemDetail(db *sqlitex.Runner, it search.Item, opts RenderOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id:     %s\n", it.SessionID)
	if it.Handle > 0 {
		fmt.Fprintf(&b, "handle: #%d\n", it.Handle)
	}
	fmt.Fprintf(&b, "agent:  %s\n", style(it.Source, ansiDim, opts.Style))
	// The row folds the client into the agent slot to save width, and
	// leaves out the unremarkable terminal case entirely; here it gets its
	// own line either way, showing the source's raw value alongside the
	// short label so the reader can see what the label was inferred from -
	// and so that "driven from the terminal" is distinguishable from "the
	// source never recorded one" (change show-editor-clients).
	if it.Client != nil && *it.Client != "" {
		client := *it.Client
		if label := session.ClientLabel(client); label != "" && label != client {
			client = fmt.Sprintf("%s (%s)", label, *it.Client)
		}
		fmt.Fprintf(&b, "client: %s\n", style(client, ansiDim, opts.Style))
	}
	cwd := "(unknown)"
	cwdColor := ""
	if it.CWD != nil {
		cwd = *it.CWD
		if it.DirExists != nil && !*it.DirExists {
			cwd += " (missing)"
			cwdColor = ansiRed
		}
	}
	fmt.Fprintf(&b, "cwd:    %s\n", style(cwd, cwdColor, opts.Style))
	if it.GitBranch != nil && *it.GitBranch != "" {
		fmt.Fprintf(&b, "branch: %s\n", *it.GitBranch)
	}
	fmt.Fprintf(&b, "state:  %s\n", style(string(it.EndState), stateColor(it.EndState), opts.Style))
	if it.LastActivityAt != nil {
		active := fmt.Sprintf("%s (%s)", it.LastActivityAt.Format(time.RFC3339), relativeTime(*it.LastActivityAt))
		fmt.Fprintf(&b, "active: %s\n", style(active, ansiDim, opts.Style))
	}
	if it.MessageCount != nil {
		fmt.Fprintf(&b, "msgs:   %d\n", *it.MessageCount)
	}
	// The name (when the user set one in the source tool) and the derived
	// topic are shown on lines of their own here, unlike the row, where
	// they share a slot: the detail pane has the space, and seeing both is
	// how you tell what a session was renamed *away* from.
	if it.Name != nil && *it.Name != "" {
		fmt.Fprintf(&b, "name:   %s\n", style(*it.Name, ansiBold, opts.Style))
	}
	topic := ""
	if it.Topic != nil && *it.Topic != "" {
		topic = *it.Topic
	} else if it.LastPrompt != nil && *it.LastPrompt != "" {
		topic = *it.LastPrompt
	}
	if topic != "" {
		fmt.Fprintf(&b, "topic:  %s\n", style(topic, ansiItalic, opts.Style))
	}

	fmt.Fprint(&b, "\ntags:   ")
	if len(it.Tags) == 0 {
		b.WriteString("(none)")
	} else {
		b.WriteString(style("#"+strings.Join(it.Tags, " #"), ansiMagenta, opts.Style))
	}
	b.WriteString("\n")

	return strings.TrimRight(b.String(), "\n")
}

// renderItemComments renders the Comments tab: LazyRecall's own annotations
// for the session's lineage, with the ids the `comment rm` action and the
// non-interactive command both take.
func renderItemComments(db *sqlitex.Runner, it search.Item, opts RenderOptions) string {
	comments, err := annotate.CommentsForLineage(db, it.LineageID)
	if err != nil {
		return "comments: " + err.Error()
	}
	if len(comments) == 0 {
		return style("No comments on this session. Press c to add one.", ansiDim, opts.Style)
	}
	var b strings.Builder
	for _, c := range comments {
		fmt.Fprintf(&b, "%s %s\n",
			style(fmt.Sprintf("[%d]", c.ID), ansiDim, opts.Style),
			style(c.CreatedAt.Format("2006-01-02 15:04"), ansiDim, opts.Style))
		for _, line := range wrapToWidth(c.Body, opts.Width-2) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// conversationCache holds one session's transcript, read once and reused
// until the selection or the search phrase changes. It also carries the
// line offsets of the current phrase's matches in the text as last
// rendered, which is what n and N step through: the offsets are recorded
// by the render that produced the lines the viewport is showing, so a jump
// can never land on a line computed against a different width.
type conversationCache struct {
	// key identifies what the cached turns were read for: the session id
	// and the transcript path together, so a rebuilt index that repoints a
	// session at a different file is not served from the old read.
	key     string
	turns   []transcript.Turn
	dropped int
	err     error
	loaded  bool
	// sourceKeepsTranscripts distinguishes "this agent writes no
	// transcript at all" from "this session has no transcript recorded",
	// which need different explanations.
	sourceKeepsTranscripts bool
	hits                   []int
}

// load reads the session's transcript unless the cache already holds it.
func (c *conversationCache) load(it search.Item) {
	path := ""
	if it.TranscriptPath != nil {
		path = *it.TranscriptPath
	}
	key := it.SessionID + "\x00" + path
	if c.loaded && c.key == key {
		return
	}
	*c = conversationCache{key: key, loaded: true}

	// Not an error when there is no vocab: hermes, goose, opencode and
	// antigravity keep their sessions in a database and write no
	// transcript at all. The renderer says so in those words rather than
	// reporting a failure to read a file that was never supposed to exist.
	vocab, ok := transcript.VocabFor(it.Source)
	c.sourceKeepsTranscripts = ok
	if !ok || path == "" {
		return
	}
	c.turns, c.dropped, c.err = transcript.Conversation(path, vocab, transcript.DefaultConversationLimits)
}

// renderItemTranscript renders the Transcript tab: the conversation itself,
// as far back as the read budget allows, with the active search phrase
// highlighted. This is the tab that answers "is this the session I meant"
// without having to resume it and find out.
func renderItemTranscript(c *conversationCache, it search.Item, phrase string, opts RenderOptions) string {
	c.load(it)

	if c.err != nil {
		// A transcript the index has a path for but that cannot be read is
		// worth naming: the usual cause is the source tool having cleaned
		// it up (Claude Code's cleanupPeriodDays) since the last refresh.
		if os.IsNotExist(c.err) {
			return style("The transcript file is gone - the agent cleaned it up since the last refresh.", ansiDim, opts.Style)
		}
		return "transcript: " + c.err.Error()
	}
	if !c.sourceKeepsTranscripts {
		return style("This agent keeps its sessions in a database, not a transcript file, so there is nothing to read here. Prompts still shows what you asked.", ansiDim, opts.Style)
	}
	if it.TranscriptPath == nil || *it.TranscriptPath == "" {
		return style("The index has no transcript file recorded for this session. A refresh (R) may pick one up.", ansiDim, opts.Style)
	}
	if len(c.turns) == 0 {
		return style("No readable turns in this transcript.", ansiDim, opts.Style)
	}

	var b strings.Builder
	if c.dropped > 0 {
		fmt.Fprintf(&b, "%s\n\n", style(fmt.Sprintf("... %d earlier turns not shown; this is the end of the session.", c.dropped), ansiDim, opts.Style))
	}

	// Hits are collected against the plain text as it is written, before
	// styling adds escape sequences, so a match is counted where the
	// reader sees it and not where an escape happens to fall.
	c.hits = nil
	line := strings.Count(b.String(), "\n")

	for i, t := range c.turns {
		if i > 0 {
			b.WriteString("\n")
			line++
		}
		fmt.Fprintf(&b, "%s\n", turnHeader(t, opts))
		line++
		for _, l := range transcriptBody(t, opts) {
			if phrase != "" && containsFold(l, phrase) {
				c.hits = append(c.hits, line)
			}
			fmt.Fprintf(&b, "  %s\n", highlightPhrase(l, phrase, opts))
			line++
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// turnHeader is the one-line speaker label above a turn's body: who spoke,
// and when, coloured the way the row list colours the same distinctions.
func turnHeader(t transcript.Turn, opts RenderOptions) string {
	label, color := "", ""
	switch t.Kind {
	case transcript.KindUserPrompt:
		label, color = "you", ansiCyan
	case transcript.KindAssistantText:
		label, color = "agent", ansiGreen
	case transcript.KindToolUse:
		// "tool call" rather than a bare "tool" when the source did not
		// name what was invoked, so an unnamed call does not read as a
		// label that lost its text.
		label, color = "tool call", ansiYellow
		if len(t.Tool) > 0 {
			label = "tool " + strings.Join(t.Tool, ", ")
		}
	case transcript.KindCompactionBoundary:
		label, color = "--- compacted ---", ansiMagenta
	}
	head := style(label, color, opts.Style)
	if t.At != nil {
		head += " " + style(t.At.Format("2006-01-02 15:04"), ansiDim, opts.Style)
	}
	return head
}

// transcriptBody is a turn's wrapped body lines, or nothing for the turns
// that are only an event (a tool call, a compaction boundary).
func transcriptBody(t transcript.Turn, opts RenderOptions) []string {
	if t.Text == "" {
		return nil
	}
	lines := wrapToWidth(t.Text, opts.Width-2)
	if t.Truncated {
		lines = append(lines, style("... turn truncated", ansiDim, opts.Style))
	}
	return lines
}

// highlightPhrase marks every case-insensitive occurrence of phrase in line
// in reverse video, so the reason a session matched a search is visible in
// the transcript rather than only in the result row.
func highlightPhrase(line, phrase string, opts RenderOptions) string {
	if !opts.Style || phrase == "" {
		return line
	}
	lower, lowerPhrase := strings.ToLower(line), strings.ToLower(phrase)
	var b strings.Builder
	for {
		i := strings.Index(lower, lowerPhrase)
		if i < 0 {
			b.WriteString(line)
			return b.String()
		}
		b.WriteString(line[:i])
		b.WriteString(ansiReverse + line[i:i+len(phrase)] + ansiReset)
		line, lower = line[i+len(phrase):], lower[i+len(phrase):]
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// promptTabLimit is how many of a session's prompts the Prompts tab reads.
// A long session can hold hundreds; the point of the tab is to recognise
// the session, which the first handful settles.
const promptTabLimit = 40

// renderItemPrompts renders the Prompts tab: what the user actually asked
// in this session, which is usually the only thing they remember about it.
func renderItemPrompts(db *sqlitex.Runner, it search.Item, opts RenderOptions) string {
	prompts, err := search.PromptsForSession(db, it.SessionID, promptTabLimit)
	if err != nil {
		return "prompts: " + err.Error()
	}
	if len(prompts) == 0 {
		return style("No indexed prompts for this session.", ansiDim, opts.Style)
	}
	var b strings.Builder
	for i, p := range prompts {
		fmt.Fprintf(&b, "%s\n", style(fmt.Sprintf("%d.", i+1), ansiDim, opts.Style))
		for _, line := range wrapToWidth(p, opts.Width-2) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// wrapToWidth breaks s into lines of at most w display columns, on word
// boundaries where it can. Comment and prompt bodies are the only free-form
// paragraphs the browser shows; every other surface is one line per item.
func wrapToWidth(s string, w int) []string {
	if w < 8 {
		w = 8
	}
	var out []string
	for _, para := range strings.Split(sanitizeMultiLine(s), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			switch {
			case line == "":
				line = word
			case visibleWidth(line)+1+visibleWidth(word) <= w:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
