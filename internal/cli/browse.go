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
	DB    *sqlitex.Runner // the single index, already refreshed
	Repo  string          // initial filter values (from command-line flags)
	Agent string
	// Install is the Agents panel's initial selection when --agent resolved
	// to one specific install rather than a whole source
	// (cmd/lazyrecall.resolveAgentFilter): a configured label, or a
	// discovered install's own name. Agent and Install are never both set -
	// resolveAgentFilter decides which of the two a --agent value means -
	// and the panel accepts whichever one is, since it matches a session
	// against it.Install or it.Source interchangeably (see matchesFacets).
	Install string
	Tag     string
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

	// Group is the browser's initial group filter (change
	// group-sessions-in-one-index): the caller resolves it from --group or
	// cfg.Browse.DefaultGroup before constructing BrowserOptions, the same
	// way --agent/--repo/--tag seed their panels' selections above. The
	// Groups panel and the `p` popup change it once the browser is running.
	Group string
	// Groups is the configured groups (config.Config.Groups) a session's
	// effective group is computed against - needed for every query the
	// browser runs, not only when Group narrows to one of them, because
	// every row's Group column depends on it.
	Groups []config.Group
	// ArchiveColor and UnknownColor are the configured colors for the
	// built-in Archive and Unknown views (config.Config.ArchiveColor,
	// UnknownColor; change archive-unknown-colors) - the raw config values
	// ("yellow", "#3355ff", ...), not yet turned into escape sequences;
	// newBrowseModel does that once via GroupColors, the same way Groups'
	// colors are.
	ArchiveColor string
	UnknownColor string

	// Transcript is the Transcript tab's starting rendering - "clean" or
	// "full" (config.Config.Browse.Transcript; change
	// clean-transcript-mode). The `t` key changes it for the running
	// browser; empty (a test not caring about the tab at all) behaves like
	// "clean", the config default, rather than requiring every existing
	// caller to spell it out.
	Transcript string
	// DateHeaders is cfg.Browse.date_headers - whether the Sessions panel
	// draws its "── Today ──" / "── Yesterday ──" / ... separator rows
	// (change date-separator-rows). It defaults to false here, the ordinary
	// Go zero value, unlike the config key it carries (which defaults to
	// true, config.defaultConfig): every real caller goes through
	// cmd/lazyrecall's cfg.Browse.DateHeaders, which is already true unless
	// a config file turned it off, so the "default true" promise is kept at
	// that layer. Leaving the zero value here as "off" is deliberate: every
	// browseModel test in this package that builds a BrowserOptions by hand
	// - the large majority, none of which are about date headers - keeps
	// behaving exactly as it did before this feature existed, with no row
	// budget to account for, unless it opts in.
	DateHeaders bool

	// Installs lists every install this browser can resolve a session
	// against. There is no more profile switching (change
	// group-sessions-in-one-index: one browsing session now covers every
	// install's data at once), but the Transcript tab still needs to find a
	// database-backed session's own install root (session.InstallFromID),
	// and the in-process refresh action needs the same list to refresh
	// every install into the shared index. Defaults to profile.Discover();
	// injectable so tests never depend on this machine's real config roots.
	Installs func() []profile.Profile

	// InstallLabels is the install-name -> display-label map every row's
	// badge and the Agents panel show (cli.InstallLabels, computed once by
	// the caller from profile.Discover() and the config's [labels] table -
	// never recomputed per row). Injectable, like Installs above, so tests
	// never depend on this machine's real config; nil is a valid "no labels
	// known" value and every row falls back to showing its bare source.
	InstallLabels map[string]string
}

// discoverInstalls uses the injected lister, defaulting to the standard
// install discovery when none was provided. The lister type has no error
// slot (this is best-effort UI plumbing, not a command path), so a
// discovery failure yields an empty list.
func (o BrowserOptions) discoverInstalls() []profile.Profile {
	if o.Installs != nil {
		return o.Installs()
	}
	installs, _ := profile.Discover()
	return installs
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
	panelGroups panelID = iota
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
	case panelGroups:
		return "Groups"
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

// isFacet reports whether p is one of the four panels that filter the
// session list by a value (change group-sessions-in-one-index: Groups was
// "Profiles", which looked the same but replaced the whole view instead of
// filtering it - now it genuinely narrows by Filter.Group like the other
// three). Sessions/Detail are not filters at all.
func (p panelID) isFacet() bool {
	return p == panelGroups || p == panelAgents || p == panelRepos || p == panelTags
}

// isLeftColumn reports whether p is one of the four panels stacked in the
// left column - Groups, Agents, Repos, Tags.
func (p panelID) isLeftColumn() bool {
	return p == panelGroups || p == panelAgents || p == panelRepos || p == panelTags
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

// transcriptMode is which of the Transcript tab's two renderings is
// showing (change clean-transcript-mode). It is UI state, not data:
// internal/transcript and conversationCache.turns are exactly the same
// either way - only how renderItemTranscriptMode groups and filters them
// differs. The `t` key (toggleTranscriptMode) flips it; browse.transcript
// in config picks what a freshly opened browser starts on.
type transcriptMode int

const (
	// transcriptClean hides KindToolUse and KindCompactionBoundary turns
	// and merges the runs of KindAssistantText turns left adjacent once
	// those are removed into one "agent" block - the default, and the
	// reading view the feature exists for: "the conversation is hard to
	// read because of all the thinking/tool/output sections."
	//
	// It is iota's zero value on purpose: a browseModel (or a test) that
	// never sets transcriptMode explicitly gets the configured default
	// rather than the technical view, matching config.Browse.Transcript's
	// own default of "clean".
	transcriptClean transcriptMode = iota
	// transcriptFull renders every turn Conversation kept, unchanged from
	// how the tab has always looked.
	transcriptFull
)

// transcriptModeFromConfig maps the validated config string
// (config.Browse.Transcript, or BrowserOptions.Transcript carrying it
// through) onto the type the browser actually branches on. Config already
// rejects any value other than "clean"/"full" at Load, but a test building
// BrowserOptions by hand may leave Transcript at its zero value "" - which
// falls to transcriptClean here for the same reason the iota above is
// ordered the way it is: no caller has to know the config default to get
// it.
func transcriptModeFromConfig(value string) transcriptMode {
	if value == "full" {
		return transcriptFull
	}
	return transcriptClean
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
	// modeRename prompts for the selected session's lazyrecall-assigned
	// name (annotate.SetName). Unlike every other prompt above, it opens
	// pre-filled with the session's current custom name rather than blank
	// (see startRenameInput) - a rename is an edit of an existing value,
	// not a fresh one, so requiring it to be retyped from scratch would be
	// actively hostile to the common case of tweaking a name rather than
	// replacing it outright.
	modeRename
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
	case modeRename:
		return "name (blank to clear): "
	case modeMenu:
		return "menu (j/k move, enter select, esc close, / to narrow): "
	}
	return ""
}

// ---------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------

// browseModel is the running state of the browser. Everything here lives in
// the process; nothing is written to disk for coordination.
type browseModel struct {
	db     *sqlitex.Runner
	style  bool
	width  int
	height int

	focus panelID

	// showAll disables the hide rules and the archive flag together, because
	// they are one question to the user: "show me everything". hide is the
	// standing config rules, passed through to the same search.Filter the
	// non-interactive commands build.
	showAll bool
	hide    config.Hide
	// group/groups carry the browser's group filter (change
	// group-sessions-in-one-index): group is the current choice, changed by
	// Enter in the Groups panel or by the `p` popup; groups is the
	// configured list every query needs to compute each row's effective
	// group (BrowserOptions.Groups).
	group  string
	groups []config.Group
	// groupItems is the superset the Groups panel counts its rows from
	// (search.ListForFacets/SearchForFacets, refreshed by loadAll): every
	// session regardless of which group is selected and regardless of
	// archive state, so Archive and every configured group can be counted
	// no matter which view is currently showing (P1 fix, review finding #4
	// - the Groups panel used to source its counts from the whole index via
	// search.Counts, ignoring the Agents/Repos/Tags/text selections already
	// narrowing every other panel). groupRows narrows it by every other
	// active facet and buckets the result by (archived, effective group) on
	// every rebuild, exactly the way countBy/narrow already do for
	// Agents/Repos/Tags over m.all - it is recomputed on every keystroke of
	// a facet filter, not cached, for the same reason those are not.
	groupItems []search.Item
	// groupColors maps a configured group's name to its ANSI foreground code,
	// plus the literal keys "archive" and "unknown" for the two built-in
	// views' colors (change per-group-colors, archive-unknown-colors),
	// computed once by newBrowseModel from opts.Groups, opts.ArchiveColor
	// and opts.UnknownColor via cli.GroupColors - never re-parsed per row or
	// per panel redraw. Used by facetPanel (a group's own name, and the
	// Archive/Unknown row labels) and by sessionsPanel's RenderOptions (each
	// row's handle).
	groupColors map[string]string
	// client is the standing --client narrowing, applied to every query
	// this browser runs (see BrowserOptions.Client).
	client string
	// hidden is how many sessions the rules suppressed in the current
	// result set - the `N hidden` the border reports so hiding is never
	// silent, the same contract the command-line header keeps.
	hidden int

	// all is the whole index's result set for the current search phrase,
	// queried once and sliced in process (see facet.go for why). It is
	// re-queried only when the corpus itself can have changed: an index
	// refresh, a new search phrase, or an annotation edit.
	all   []search.Item
	query string // the full-text search phrase, "" for none

	// groups_ is the Groups panel (change group-sessions-in-one-index; it
	// was "Profiles", a single inert "All" row with no action of its own).
	// With no groups configured it still holds exactly one row, "All" - the
	// slot and its layout code are unchanged, so a config with no groups
	// looks exactly as the browser always did.
	groups_ facet
	agents  facet
	repos   facet
	tags    facet
	// agentSource is a source-level seed for the Agents panel - set only
	// from BrowserOptions.Agent (a bare source name like "claude", spanning
	// every install of it) and never by a panel interaction, which always
	// targets one specific install via agents.Sel instead (change
	// group-sessions-in-one-index, fixing a leak found against real data:
	// see matchesFacets). Any explicit Agents action - Enter, Esc, "clear
	// all filters" - clears this alongside agents.Sel, so a later install
	// selection is never silently ANDed with a source seed left over from
	// how the browser was opened.
	agentSource string
	// installLabels maps an install name to its display label
	// (cli.InstallLabels, computed once by the caller from
	// profile.Discover() and the config's [labels] table) - what a session
	// row's badge shows instead of the bare source, and what the Agents
	// panel shows instead of a raw install name (see BrowserOptions.
	// InstallLabels).
	installLabels map[string]string

	// textFilter narrows the loaded session rows in process as the user
	// types (spec session-search, "Narrowing the loaded rows by typing").
	textFilter string

	visible []search.Item // sessions after every facet and the text filter
	cursor  int
	listTop int

	// dateHeaders is BrowserOptions.DateHeaders, copied once in
	// newBrowseModel (change date-separator-rows): whether the Sessions
	// panel draws bucket separators at all. Even when true, a render can
	// still end up with none - sessionDisplayRows also requires m.visible to
	// actually be in recency order (recencyOrdered), which every state this
	// browser can be in today satisfies, but a text filter or a future
	// change to search ordering is not trusted to keep satisfying by
	// assumption alone.
	dateHeaders bool
	// now is the Sessions panel's clock for bucketing by LastActivityAt
	// (bucketFor) - nil in every model built by newBrowseModel, which is
	// what clock() reads as "use time.Now()". Only tests ever set it
	// directly (there is no matching BrowserOptions field: nothing a real
	// caller could inject a fixed clock from), so "today" can be pinned
	// without the test depending on when it happens to run.
	now func() time.Time

	tab      detailTab
	detail   *viewport.Model
	detailOf string // session id the viewport's content was built for

	// convo caches the transcript the Transcript tab is showing. The pane
	// is rebuilt on every frame, and a transcript is a file read rather
	// than a query against the already-loaded result set, so without this
	// every keystroke would re-read it. Behind a pointer for the same
	// reason the viewport is: the render path takes the model by value.
	convo *conversationCache
	// transcriptMode is clean or full (change clean-transcript-mode),
	// seeded from BrowserOptions.Transcript (config's browse.transcript)
	// and flipped by `t` for the lifetime of this browser - unlike tab,
	// which session is selected, or the search phrase, it survives every
	// one of those changing.
	transcriptMode transcriptMode

	mode       inputMode
	input      textinput.Model
	inputLabel string
	// installs lists every discovered install, for the Transcript tab's
	// per-session lookup (installFor) and the refresh action - see
	// BrowserOptions.Installs.
	installs func() []profile.Profile

	// Action-menu state. The menu is a list of concrete actions for the
	// focused panel, narrowable by typing - the same interaction the old
	// value-selection prompt had, now pointed at verbs instead of values.
	// menuTitle is drawn in the popup's border - `x` sets it from the
	// focused panel's own title, but `p`'s group popup (change
	// group-sessions-in-one-index) is not about the focused panel at all,
	// so it needs a title of its own rather than borrowing one that would
	// say the wrong thing whenever `p` is pressed somewhere other than
	// Sessions.
	menuAll      []menuAction
	menuFiltered []menuAction
	menuCursor   int
	menuTitle    string
	// menuNarrowing is whether the menu is in its "/" quick-filter
	// sub-state (change menu-jk-navigation): false is the default, where
	// j/k and the arrow keys move menuCursor and typed letters do nothing;
	// true is the old typing-narrows behaviour, entered by "/" and left by
	// Esc (which also clears the filter but leaves the menu open - a second
	// Esc is what actually closes it). Reset to false by openMenu and
	// openGroupMenu, so a menu never opens already narrowing from a
	// previous session.
	menuNarrowing bool

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
		db:             opts.DB,
		style:          opts.Style,
		showAll:        opts.ShowAll,
		hide:           opts.Hide,
		group:          opts.Group,
		groups:         opts.Groups,
		groupColors:    GroupColors(opts.Groups, opts.ArchiveColor, opts.UnknownColor),
		installs:       opts.discoverInstalls,
		installLabels:  opts.InstallLabels,
		width:          DefaultWidth,
		height:         24,
		focus:          panelSessions,
		query:          opts.Query,
		client:         opts.Client,
		input:          textinput.New(),
		transcriptMode: transcriptModeFromConfig(opts.Transcript),
		dateHeaders:    opts.DateHeaders,
	}
	// Command-line filters open as the corresponding panels' selections, so
	// `lazyrecall browse --agent=pi` and walking to "pi" in the Agents panel
	// land in exactly the same state. The Agents panel facets by install
	// (change group-sessions-in-one-index), so a seed that resolved to one
	// specific install (a label, or an install's own name) seeds agents.Sel
	// exactly like a panel row would; a seed that resolved to a bare source
	// (spanning every install of it, e.g. plain `--agent=claude`) seeds the
	// separate agentSource field instead - resolveAgentFilter only ever
	// returns one of the two non-empty, so exactly one of these takes
	// effect.
	m.agents.Sel = opts.Install
	m.agentSource = opts.Agent
	m.repos.Sel = opts.Repo
	m.tags.Sel = opts.Tag
	m.groups_.Sel = opts.Group
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
	f := search.Filter{Client: m.client, Hide: m.hide, ShowAll: m.showAll, Group: m.group, Groups: m.groups}
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

	// groupItems is a separate query, not a walk over m.all: m.all is itself
	// scoped to the selected group and (unless showAll or the archive view
	// is selected) excludes archived sessions - exactly what the Groups
	// panel's own superset must not be, since Archive and every other group
	// need to stay countable regardless of which one is currently selected
	// (ListForFacets/SearchForFacets' doc comments). It is refreshed here,
	// alongside the corpus itself, on every reload (R, a group change, an
	// archive toggle, a search phrase) - and never recomputed by rebuild,
	// which runs on every keystroke of a facet filter and must stay a pure
	// in-process walk, the same discipline m.all already follows.
	gf := f
	gf.Group = ""
	var gitems []search.Item
	var gerr error
	if m.query != "" {
		gitems, gerr = search.SearchForFacets(m.db, m.query, gf)
	} else {
		gitems, gerr = search.ListForFacets(m.db, gf)
	}
	if gerr != nil {
		m.notice = "browse: " + gerr.Error()
	} else {
		m.groupItems = gitems
	}
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
	corpus := func(install, source, repo, tag string) []search.Item {
		return m.applyTextFilter(narrow(m.all, install, source, repo, tag))
	}
	// Agents' own rows exclude the whole agent dimension, install and
	// source together - not just agents.Sel - so switching away from a
	// source-level seed (agentSource) stays exactly as easy as switching
	// away from a specific install: the panel always shows what every
	// option would give, never only what is left after its own current
	// filter.
	m.agents.setRows(countBy(corpus("", "", m.repos.Sel, m.tags.Sel), "all agents", installKey, m.installLabel))
	m.repos.setRows(countBy(corpus(m.agents.Sel, m.agentSource, "", m.tags.Sel), "all repos", repoKey, abbreviateHome))
	m.tags.setRows(countBy(corpus(m.agents.Sel, m.agentSource, m.repos.Sel, ""), "all tags", tagKeys, func(s string) string { return "#" + s }))
	m.groups_.setRows(m.groupRows())

	m.visible = corpus(m.agents.Sel, m.agentSource, m.repos.Sel, m.tags.Sel)
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

// groupCountsFromItems narrows m.groupItems by every OTHER active facet -
// Agents/Repos/Tags plus the "/" text filter, the same corpus() would apply
// for any other panel (rebuild) - and buckets the result by archive state
// and effective group. The group selection itself is deliberately excluded
// from the narrowing: a Groups panel row has to say what selecting it would
// give, which is meaningless if the count is already restricted to
// whichever group is currently selected (the same reason Agents' own rows
// exclude the Agent dimension - see rebuild's comment on corpus).
func (m browseModel) groupCountsFromItems() (all, archive, unknown int, byGroup map[string]int) {
	items := m.applyTextFilter(narrow(m.groupItems, m.agents.Sel, m.agentSource, m.repos.Sel, m.tags.Sel))
	byGroup = map[string]int{}
	for _, it := range items {
		if it.Archived {
			archive++
			continue
		}
		all++
		if it.Group == "" {
			unknown++
		} else {
			byGroup[it.Group]++
		}
	}
	return
}

// groupRows builds the Groups panel's rows from m.groupItems, narrowed by
// every other active facet exactly the way the Agents/Repos/Tags panels
// narrow theirs (change group-sessions-in-one-index, P1 fix #4: this used
// to come from a separate whole-index query - search.Counts - that ignored
// those selections entirely, advertising a group as if it had sessions
// under the current filter when it did not).
//
// Zero-count rows follow the same convention countBy already applies to
// Agents/Repos/Tags (a value with no items under the current narrowing
// simply is not offered), with two exceptions particular to this panel: All
// is always shown even at zero (it is the anchor every other row is
// relative to, exactly like the "all" row of any other facet), and the
// currently selected row is never hidden even at zero - clearing a
// selection that narrowed its own row out of existence must stay reachable
// from the panel that applied it. Zero config keeps its existing shape
// otherwise: All, plus Archive only when it (now filter-aware) is nonzero
// or selected; Unknown stays hidden in the zero-groups case even when
// nonzero, since with no groups configured every non-archived session lands
// in Unknown and a row for it would only duplicate All.
func (m browseModel) groupRows() []facetRow {
	all, archive, unknown, byGroup := m.groupCountsFromItems()

	rows := []facetRow{{Value: "", Label: "All", Count: all}}
	if len(m.groups) == 0 {
		if archive > 0 || m.group == "archive" {
			rows = append(rows, facetRow{Value: "archive", Label: "Archive", Count: archive})
		}
		return rows
	}
	for _, g := range m.groups {
		n := byGroup[g.Name]
		if n > 0 || m.group == g.Name {
			rows = append(rows, facetRow{Value: g.Name, Label: g.Name, Count: n})
		}
	}
	if archive > 0 || m.group == "archive" {
		rows = append(rows, facetRow{Value: "archive", Label: "Archive", Count: archive})
	}
	if unknown > 0 || m.group == "unknown" {
		rows = append(rows, facetRow{Value: "unknown", Label: "Unknown", Count: unknown})
	}
	return rows
}

// installLabel is the Agents panel's label function (see countBy): the
// display label m.installLabels has cached for an install name, falling
// back to the install name itself when none is configured - the same
// fallback profile.Profile.Label applies, kept in step with it here because
// the panel has no config.Config of its own, only the precomputed map.
func (m browseModel) installLabel(name string) string {
	if label, ok := m.installLabels[name]; ok && label != "" {
		return label
	}
	return name
}

// setGroupFilter changes which group the browser is showing: the Groups
// panel's Enter and Esc, and (indirectly, via loadAll's own reload) every
// place that files a session into a different group. Unlike the Agents/
// Repos/Tags facets, a group change cannot be answered by narrowing m.all in
// process - Filter.Group changes which rows the query itself returns (the
// "archive" and "unknown" views in particular are not expressible as a walk
// over a corpus that already excludes archived sessions) - so this reloads
// rather than rebuilds.
func (m *browseModel) setGroupFilter(name string) {
	m.group = name
	m.groups_.Sel = name
	m.cursor, m.listTop = 0, 0
	m.loadAll()
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
func matchText(it search.Item, width int, labels map[string]string) string {
	slots := rowSlots(it, labels)
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
		if strings.Contains(strings.ToLower(matchText(it, m.sessionRowWidthFor(it, baseWidth), m.installLabels)), needle) {
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
	case panelGroups:
		return &m.groups_
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
// m.listTop stays a session index exactly as it was before date headers
// existed (see rebuild's own listTop/cursor comparison, which depends on
// that) - what changed is the cost of showing the window from m.listTop
// through m.cursor, which is no longer "one row per session" once a bucket
// boundary in between adds a header row of its own. windowCost measures that
// cost in display rows; the loop below is this function's old direct jump
// (`m.listTop = m.cursor - h + 1`) turned into single steps, since a
// header's contribution to the cost depends on exactly which sessions end
// up in the window and so cannot be computed by one subtraction the way a
// uniform one-row-per-session list could.
func (m *browseModel) keepCursorVisible() {
	h := m.geometry().sessionsInner
	if h <= 0 || len(m.visible) == 0 {
		return
	}
	if m.cursor < m.listTop {
		m.listTop = m.cursor
	}
	rows, sessionRow := m.sessionDisplayRows()
	for m.listTop < m.cursor && windowCost(rows, sessionRow, m.listTop, m.cursor) > h {
		m.listTop++
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
	case "p":
		return m.openGroupMenu()
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
	case "t":
		m.toggleTranscriptMode()
	case "m":
		return m.startInput(modeAddTag)
	case "M":
		return m.startInput(modeRemoveTag)
	case "d":
		m.removeTagUnderCursor()
	case "c":
		return m.startInput(modeAddComment)
	case "C":
		return m.startInput(modeRemoveComment)
	case "r":
		return m.startRenameInput()
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
	case panelGroups:
		return g.groupsH > 0
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
// is the point of the feature, so K on Groups, J on Tags, and L on
// Sessions or Detail simply do nothing, the same way move() does nothing
// past the first or last row of a list.
//
// H always resolves to Groups specifically, never to whichever left panel
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
	switch dir {
	case dirLeft:
		if m.focus == panelSessions || m.focus == panelDetail {
			candidates = []panelID{panelGroups}
		}
	case dirRight:
		if m.focus.isLeftColumn() {
			candidates = []panelID{panelSessions}
		}
	case dirDown, dirUp:
		candidates = m.columnNeighbours(dir)
	}
	for _, p := range candidates {
		if m.panelDrawnWhenFocused(p) {
			m.focus = p
			m.notice = ""
			return
		}
	}
}

// visualColumn is the column containing p, in the order it is drawn top to
// bottom. It is what makes J and K follow the screen rather than a fixed
// table: when the terminal is too narrow for two columns every panel is
// stacked into one, and a table written for the two-column layout leaves
// Sessions and Detail unreachable by J/K there - they are directly below
// Tags on screen, but the table says they are in a different column.
func (m browseModel) visualColumn(p panelID) []panelID {
	if !m.geometry().sidebar {
		return []panelID{panelGroups, panelAgents, panelRepos, panelTags, panelSessions, panelDetail}
	}
	if p.isLeftColumn() {
		return []panelID{panelGroups, panelAgents, panelRepos, panelTags}
	}
	return []panelID{panelSessions, panelDetail}
}

// columnNeighbours lists the panels J or K should consider, nearest first:
// everything past the focused panel in that direction, so a collapsed or
// undrawn neighbour is stepped over rather than blocking the move.
func (m browseModel) columnNeighbours(dir direction) []panelID {
	col := m.visualColumn(m.focus)
	at := -1
	for i, p := range col {
		if p == m.focus {
			at = i
			break
		}
	}
	if at < 0 {
		return nil
	}
	var out []panelID
	if dir == dirDown {
		out = append(out, col[at+1:]...)
		return out
	}
	for i := at - 1; i >= 0; i-- {
		out = append(out, col[i])
	}
	return out
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
	g := m.geometry()
	var h int
	switch m.focus {
	case panelSessions:
		h = g.sessionsInner
	case panelDetail:
		// The detail pane is not a facet, so facetInnerHeight reports zero
		// for it and Ctrl-D/Ctrl-U used to fall through to the one-line
		// floor - a half-page key that scrolled a line, which is what a
		// long transcript made obvious.
		h = g.detailInner
	default:
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
	case panelGroups:
		row := m.groups_.index()
		if row == nil {
			return m, nil
		}
		m.setGroupFilter(row.Value) // the "all" row carries "", which is "no filter"
	case panelAgents:
		row := m.agents.index()
		if row == nil {
			return m, nil
		}
		m.agents.Sel = row.Value // the "all" row carries "", which is "no filter"
		// An explicit install choice replaces any source-level seed the
		// browser opened with - otherwise the two would AND together, and
		// a seed from a different source than the chosen install (e.g.
		// --agent=pi, then picking the omp install from the panel) would
		// match nothing at all (change group-sessions-in-one-index).
		m.agentSource = ""
		m.cursor, m.listTop = 0, 0
		m.rebuild()
	case panelRepos, panelTags:
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
	case panelGroups:
		if m.group == "" {
			return
		}
		m.setGroupFilter("")
	case panelAgents:
		if m.agents.Sel == "" && m.agentSource == "" {
			return
		}
		m.agents.Sel = ""
		m.agentSource = ""
		m.cursor, m.listTop = 0, 0
		m.rebuild()
	case panelRepos, panelTags:
		f := m.facetFor(m.focus)
		if f.Sel == "" {
			return
		}
		f.Sel = ""
		m.cursor, m.listTop = 0, 0
		m.rebuild()
	}
}

// clearAllFilters is X: every facet at once, including the group (P1 fix,
// group-sessions-in-one-index review) - the group was left out of the
// original list here, the same omission a new facet needs to remember not
// to repeat. A group change can't be answered by a rebuild the way
// Agent/Repo/Tag can (setGroupFilter's own doc comment: "archive" and
// "unknown" are not expressible as a walk over a corpus already loaded for
// a different group), so clearing it needs the same reload the search
// phrase already triggers here - one reload covers both when either is set.
func (m *browseModel) clearAllFilters() {
	m.agents.Sel, m.repos.Sel, m.tags.Sel = "", "", ""
	m.agentSource = ""
	m.textFilter = ""
	m.cursor, m.listTop = 0, 0
	needsReload := m.query != "" || m.group != ""
	m.query = ""
	m.group = ""
	m.groups_.Sel = ""
	if needsReload {
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

// removeTagUnderCursor is "d" in the Tags panel: take the tag the cursor is
// on off the selected session. It is the same operation as M, reached by
// pointing at the tag instead of retyping it - which is the only way the
// operation is usable at all once tags are longer than a word.
//
// It removes the tag from the selected session, not from every session that
// carries it: the Tags panel is a filter over the whole profile, so a key
// that deleted a tag everywhere would destroy other sessions' annotations
// from a panel that never showed them. The notice names both the tag and
// the session so what happened is never in doubt.
func (m *browseModel) removeTagUnderCursor() {
	if m.focus != panelTags {
		m.notice = "d removes a tag - move to the Tags panel (4) and put the cursor on it."
		return
	}
	f := m.facetFor(panelTags)
	if f == nil || f.cursor < 0 || f.cursor >= len(f.rows) {
		return
	}
	tag := f.rows[f.cursor].Value
	it := m.current()
	if it == nil {
		m.notice = "No session selected, so there is nothing to take the tag off."
		return
	}
	// Removing a tag a session does not have would report success and
	// change nothing, which reads as the key having silently failed on the
	// session the user meant.
	if !hasTag(*it, tag) {
		m.notice = fmt.Sprintf("The selected session is not tagged #%s.", tag)
		return
	}
	if err := annotate.RemoveTag(m.db, it.LineageID, tag); err != nil {
		m.notice = "tag: " + err.Error()
		return
	}
	m.notice = fmt.Sprintf("removed #%s from %s.", tag, it.SessionID)
	m.detailOf = ""
	m.loadAll()
}

func hasTag(it search.Item, tag string) bool {
	for _, t := range it.Tags {
		if t == tag {
			return true
		}
	}
	return false
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

// toggleTranscriptMode flips the Transcript tab between clean and full -
// the `t` key (change clean-transcript-mode). Like n/N (jumpToHit), it only
// means something while that tab is showing; acting on it from another tab
// would silently change a pane the user cannot see and leave them wondering
// later why nothing looked different.
//
// There is nothing to re-render here beyond the mode itself: detailContent
// runs on every frame regardless of which tab is showing (there is no
// "already rendered" cache to invalidate the way tab switches clear
// detailOf for), so the very next View picks up the new mode, recomputes
// convo.hits against the new line layout, and n/N step through exactly
// those. GotoTop puts the pane where opening the tab already does
// (cycleTab) - a toggle is a second way to arrive at a freshly drawn
// transcript, not a special case of it.
func (m *browseModel) toggleTranscriptMode() {
	if m.tab != tabTranscript {
		m.notice = "t switches clean/full transcript view on the Transcript tab."
		return
	}
	if m.transcriptMode == transcriptClean {
		m.transcriptMode = transcriptFull
		m.notice = "full transcript - every turn, tool calls included."
	} else {
		m.transcriptMode = transcriptClean
		m.notice = "clean transcript - tool calls and compaction boundaries hidden."
	}
	m.detail.GotoTop()
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

// startRenameInput opens the rename prompt (modeRename) pre-filled with the
// selected session's current lazyrecall-assigned name, cursor at the end -
// unlike startInput's other callers, which always open blank. A rename is
// an edit of whatever is already there, so the prompt starts from it rather
// than making every rename retype a name that is only being tweaked. With
// no session selected (an empty Sessions list) it still opens, blank, the
// same as any other prompt would with nothing to act on - submitInput
// handles that case the same way modeAddTag/modeAddComment already do, by
// simply doing nothing if m.current() is nil.
func (m *browseModel) startRenameInput() (tea.Model, tea.Cmd) {
	mod, cmd := m.startInput(modeRename)
	if it := m.current(); it != nil && it.CustomName != nil {
		m.input.SetValue(*it.CustomName)
		m.input.CursorEnd()
	}
	return mod, cmd
}

func (m *browseModel) updateInputMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if ok && m.mode == modeMenu {
		// The menu (x and p) has its own key handling - j/k navigate by
		// default rather than narrowing - kept in updateMenuMode instead of
		// interleaved with every other prompt's text-input handling below
		// (change menu-jk-navigation).
		return m.updateMenuMode(key)
	}
	if ok {
		switch key.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.mode = modeNone
			m.input.Blur()
			m.notice = ""
			return m, nil
		case tea.KeyEnter:
			return m, m.submitInput(strings.TrimSpace(m.input.Value()))
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.mode == modeFilter {
		// The narrowing filter applies as it is typed, which is what makes
		// it feel like filtering rather than like filling in a form.
		m.applyFilterText(m.input.Value())
	}
	return m, cmd
}

// updateMenuMode handles keys while a menu (x or p) is open (change
// menu-jk-navigation). By default j/k and the arrow keys move the
// highlighted entry (menuCursor) and typed letters do nothing else - the
// menu is a short, fully visible list, so stealing every letter to narrow
// by typing cost more (j/k, the browser's own move keys, stopped working
// the moment a menu opened) than it bought. Pressing "/" switches into
// menuNarrowing, where typing behaves as the whole menu used to: characters
// filter menuAll into menuFiltered as they're typed (refilterMenu),
// backspace edits, Enter applies the highlighted entry, and Esc leaves
// narrowing - clearing the filter but keeping the menu open, matching the
// rest of the browser's "Esc cancels this one step" contract - rather than
// closing the menu outright; a second Esc does that.
func (m *browseModel) updateMenuMode(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.menuNarrowing {
		switch key.Type {
		case tea.KeyEsc:
			m.menuNarrowing = false
			m.input.SetValue("")
			m.input.Blur()
			m.refilterMenu()
			return m, nil
		case tea.KeyCtrlC:
			m.mode = modeNone
			m.menuNarrowing = false
			m.input.Blur()
			m.notice = ""
			return m, nil
		case tea.KeyEnter:
			return m, m.runMenuSelection()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(key)
		m.refilterMenu()
		return m, cmd
	}

	switch key.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.mode = modeNone
		m.input.Blur()
		m.notice = ""
		return m, nil
	case tea.KeyEnter:
		return m, m.runMenuSelection()
	case tea.KeyUp:
		m.menuCursor = clampIndex(m.menuCursor-1, len(m.menuFiltered))
		return m, nil
	case tea.KeyDown:
		m.menuCursor = clampIndex(m.menuCursor+1, len(m.menuFiltered))
		return m, nil
	case tea.KeyRunes:
		switch string(key.Runes) {
		case "j":
			m.menuCursor = clampIndex(m.menuCursor+1, len(m.menuFiltered))
		case "k":
			m.menuCursor = clampIndex(m.menuCursor-1, len(m.menuFiltered))
		case "/":
			m.menuNarrowing = true
			m.input.SetValue("")
			return m, m.input.Focus()
		}
		// Any other rune - a letter that used to narrow - does nothing:
		// see the doc comment above.
	}
	return m, nil
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
	case modeRename:
		// Unlike modeAddTag/modeAddComment above, an empty value is not a
		// no-op here: it is how a name set earlier is cleared (spec-shaped
		// like modeFilter's "blank clears" exception to submitting empty
		// applying nothing). annotate.SetName already treats an empty or
		// whitespace-only name as "clear", so this always calls it rather
		// than gating on value != "".
		if it := m.current(); it != nil {
			if err := annotate.SetName(m.db, it.LineageID, value); err != nil {
				m.notice = "name: " + err.Error()
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
			menuAction{"rename this session", func(m *browseModel) tea.Cmd { _, c := m.startRenameInput(); return c }},
			menuAction{"filter these sessions", func(m *browseModel) tea.Cmd { _, c := m.startInput(modeFilter); return c }},
		)
		// The same group entries the `p` popup offers (change
		// group-sessions-in-one-index), so filing a session into a group is
		// discoverable from `x` too, not only from a key a user has to
		// already know exists.
		if it := m.current(); it != nil {
			out = append(out, m.groupMenuActions(*it)...)
		}
	case panelGroups, panelAgents, panelRepos, panelTags:
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
	m.menuTitle = m.focus.title() + " actions"
	m.menuNarrowing = false
	mod, cmd := m.startInput(modeMenu)
	// The input widget stays blurred until "/" starts narrowing - see
	// updateMenuMode - so no cursor blinks next to instructions that, by
	// default, typing does not act on.
	m.input.Blur()
	return mod, cmd
}

// groupMenuActions is the set of group choices for one session: each
// configured group, Archive, and Automatic - the same entries whether they
// are reached from the Sessions `x` menu or from `p`'s dedicated popup
// (change group-sessions-in-one-index), so the two surfaces can never offer
// different choices for the same key concept. With no groups configured the
// loop over m.groups contributes nothing, which is exactly "the popup
// offers only Archive and Automatic" from the zero-config requirement - no
// separate branch needed.
//
// The currently-applied choice is marked in the label itself (markLabel),
// not only by where the cursor starts: a marker baked into the text survives
// typing to narrow the menu, where the cursor position does not mean
// anything in particular any more.
func (m *browseModel) groupMenuActions(it search.Item) []menuAction {
	// Precedence matches annotate.SetGroup's own model: a manual override
	// (group_name) is independent of the archive flag and can coexist with
	// it (the plain `archive` command sets archived_at without touching
	// group_name), so GroupManual is checked first - a session filed under
	// "work" and separately archived still shows "work" as its current
	// choice, not Archive.
	current := ""
	switch {
	case it.GroupManual:
		current = it.Group
	case it.Archived:
		current = "archive"
	}

	auto := search.PathGroup(m.groups, cwdOf(it))
	autoLabel := "unknown"
	if auto != "" {
		autoLabel = auto
	}

	out := make([]menuAction, 0, len(m.groups)+2)
	for _, g := range m.groups {
		name := g.Name
		out = append(out, menuAction{
			label: markLabel(name, current == name),
			run:   func(m *browseModel) tea.Cmd { return m.setSessionGroup(it, name) },
		})
	}
	out = append(out,
		menuAction{
			label: markLabel("Archive", current == "archive"),
			run:   func(m *browseModel) tea.Cmd { return m.setSessionGroup(it, "archive") },
		},
		menuAction{
			label: markLabel(fmt.Sprintf("Automatic (%s)", autoLabel), current == ""),
			run:   func(m *browseModel) tea.Cmd { return m.setSessionGroup(it, "") },
		},
	)
	return out
}

// markLabel prefixes an entry's label with the same "applied" mark the
// facet panels use (●), so the currently-applied choice is visible in the
// popup regardless of where the cursor happens to start (see
// groupMenuActions).
func markLabel(label string, current bool) string {
	if current {
		return "● " + label
	}
	return "  " + label
}

// cwdOf is search.PathGroup's nil-safe accessor for it.CWD: a session with
// no recorded working directory has no path rule to match, the same "" ==
// Unknown PathGroup itself treats an empty cwd as.
func cwdOf(it search.Item) string {
	if it.CWD == nil {
		return ""
	}
	return *it.CWD
}

// setSessionGroup applies one group choice to it's lineage via
// annotate.SetGroup - the same function the CLI's `group` command calls, so
// the popup and the command line can never disagree about what a choice
// does - then reloads (a group change alters which query rows come back,
// not just how they're grouped in process) and leaves a notice naming what
// happened, including what Automatic actually resolved to, since "group:
// automatic" alone would not say whether that means work or unknown.
func (m *browseModel) setSessionGroup(it search.Item, choice string) tea.Cmd {
	if err := annotate.SetGroup(m.db, config.Config{Groups: m.groups}, it.LineageID, choice); err != nil {
		m.notice = "group: " + err.Error()
		return nil
	}
	switch choice {
	case "":
		auto := search.PathGroup(m.groups, cwdOf(it))
		if auto == "" {
			auto = "unknown"
		}
		m.notice = fmt.Sprintf("group: automatic (%s)", auto)
	case "archive":
		m.notice = "group: archive"
	default:
		m.notice = "group: " + choice
	}
	m.detailOf = ""
	m.loadAll()
	return nil
}

// currentGroupMenuIndex finds the entry groupMenuActions marked as applied
// (markLabel), so openGroupMenu can start the cursor there - a popup opened
// to change a choice should not make the reader hunt for where they
// currently stand.
func currentGroupMenuIndex(actions []menuAction) int {
	for i, a := range actions {
		if strings.HasPrefix(a.label, "● ") {
			return i
		}
	}
	return 0
}

// openGroupMenu is `p`: the same modeMenu machinery `x` uses (navigate with
// j/k or the arrow keys, Enter applies, Esc cancels with no change, "/"
// narrows by substring - see updateMenuMode), pointed at one session's group
// choices instead of the focused panel's actions. It acts on the selected
// session regardless of which panel has focus, the same way `a` (archive)
// and `.` (show all) do, since "which session" is a property of the
// Sessions list, not of where the cursor happens to be parked.
func (m *browseModel) openGroupMenu() (tea.Model, tea.Cmd) {
	it := m.current()
	if it == nil {
		m.notice = "No session selected, so there is nothing to file into a group."
		return m, nil
	}
	m.menuAll = m.groupMenuActions(*it)
	m.menuFiltered = m.menuAll
	m.menuCursor = currentGroupMenuIndex(m.menuAll)
	m.menuTitle = "Session group"
	m.menuNarrowing = false
	mod, cmd := m.startInput(modeMenu)
	m.input.Blur()
	return mod, cmd
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

// refreshCmd is the explicit index refresh action (spec session-search,
// "Implement an explicit index refresh without leaving the browser"): it
// re-runs the refresh pass over every discovered install into the single
// index (change group-sessions-in-one-index - there is no more "the active
// profile" to refresh alone) and reports back into the model, which then
// reloads its rows.
func (m browseModel) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		r, err := refresh.New(m.installs(), "")
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
	groupsH       int
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
	// Groups and Agents are short, known-length lists and need only what
	// they hold; if what is left over still gives Repos and Tags a usable
	// window each, every panel shows content and nothing collapses. That is
	// the sizing this browser had before the accordion, and it is the right
	// one whenever there is room: a wide, tall terminal has space for all
	// four, and collapsing three of them there hides dimensions the user
	// could otherwise read at a glance without pressing anything.
	//
	// Groups' own cap (maxGroupsRows) is generous rather than tight like
	// Agents' - a group is something a user configured by hand in a file,
	// so the row count is bounded by how many [groups.*] tables they wrote
	// (All, each one, Archive, Unknown), not by how many sessions or
	// installs exist. A tight cap here is what let Unknown scroll out of
	// view below a Tags panel that had empty lines to spare (bug found
	// against real data: 2 groups + Archive + Unknown is 5 rows, and the
	// old cap of 4 always hid one of them even with room to give it).
	//
	// The accordion below is for the case that sizing could not handle -
	// the old "rest < 8" branch, which dropped Tags outright. Collapsing a
	// panel to a header line is strictly better than deleting it, but it is
	// a concession to a short column, not an improvement on a roomy one.
	if room := g.bodyHeight - boxHeight(len(m.groups_.rows), 1, maxGroupsRows) - boxHeight(len(m.agents.rows), 1, 5); room >= 8 {
		g.groupsH = boxHeight(len(m.groups_.rows), 1, maxGroupsRows)
		g.agentsH = boxHeight(len(m.agents.rows), 1, 5)
		g.reposH = room * 3 / 5
		g.tagsH = room - g.reposH
	} else if g.bodyHeight >= 4 {
		g.groupsH, g.agentsH, g.reposH, g.tagsH = 1, 1, 1, 1
		switch expanded {
		case panelGroups:
			g.groupsH = g.bodyHeight - 3
		case panelAgents:
			g.agentsH = g.bodyHeight - 3
		case panelRepos:
			g.reposH = g.bodyHeight - 3
		case panelTags:
			g.tagsH = g.bodyHeight - 3
		}
	} else {
		heights := [numPanels]int{
			panelGroups: 1, panelAgents: 1, panelRepos: 1, panelTags: 1,
		}
		used := 4
		for _, p := range [...]panelID{panelTags, panelRepos, panelAgents, panelGroups} {
			if used <= g.bodyHeight || (p == expanded && focus.isLeftColumn()) {
				continue
			}
			heights[p]--
			used--
		}
		g.groupsH = heights[panelGroups]
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
// the end of the draw order - Detail, Tags, Repos, Agents, Groups, then
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

	// Draw order is Groups, Agents, Repos, Tags, Sessions, Detail - the
	// digit order with Sessions' 0 last, which is also where lazygit puts
	// its own [0] panel. Detail collapses like the rest: it is content for
	// the selected row, so it earns its rows only when it is what the user
	// is reading.
	heights := [numPanels]int{
		panelGroups:   1,
		panelAgents:   1,
		panelRepos:    1,
		panelTags:     1,
		panelSessions: 3,
		panelDetail:   1,
	}
	used := 8
	for _, p := range [...]panelID{panelDetail, panelTags, panelRepos, panelAgents, panelGroups, panelSessions} {
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

	g.groupsH = heights[panelGroups]
	g.agentsH = heights[panelAgents]
	g.reposH = heights[panelRepos]
	g.tagsH = heights[panelTags]
	g.sessionsH = heights[panelSessions]
	g.detailH = heights[panelDetail]
}

// maxGroupsRows is the roomy layout's row cap for the Groups panel: All,
// every configured group, Archive, and Unknown. It is generous rather than
// tight (contrast Agents' cap of 5) because that count is bounded by how
// many [groups.*] tables a user wrote in their config file, not by how much
// data is indexed - see wideHeights.
const maxGroupsRows = 12

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
	case panelGroups:
		return maxInt(g.groupsH-2, 0)
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
			m.facetPanel(panelGroups, &m.groups_, g.leftWidth, g.groupsH, m.group),
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
			m.facetPanel(panelGroups, &m.groups_, g.rightWidth, g.groupsH, m.group),
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
		fmt.Fprintf(&b, "%s%s\n", m.currentInputLabel(), m.input.View())
	}
	b.WriteString(m.footer())
	return b.String()
}

// currentInputLabel is what is drawn ahead of the input widget on the input
// bar. It is m.inputLabel (set once by startInput from mode.prompt())
// except for the menu's own narrowing sub-state (change
// menu-jk-navigation), which needs a label that flips between "here's how
// to drive the menu" and "type to narrow" as menuNarrowing itself flips,
// something mode.prompt() cannot see since it only knows the inputMode, not
// the menu's internal state.
func (m browseModel) currentInputLabel() string {
	if m.mode == modeMenu && m.menuNarrowing {
		return "narrow (esc clears, esc again closes): "
	}
	return m.inputLabel
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
		Title:   m.menuTitle,
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
		box.Value = sanitizeSingleLine(sel)
		return box
	}
	// Every panel says where the cursor is and how many rows it has. The
	// applied filter is deliberately not repeated here: the full box marks
	// it as a ● row, and spending the border on it too would cost the
	// count on a narrow sidebar, where the annotation is dropped whole. A
	// typed narrowing is the exception - it has no row of its own, so
	// without this the panel would silently be showing a subset.
	box.Count = positionCount(f.cursor, len(f.rows))
	if f.filter != "" {
		box.Count = "/" + truncateToWidth(sanitizeSingleLine(f.filter), 8) + " " + box.Count
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
		count := strconv.Itoa(r.Count)
		labelWidth := inner - facetDecorationWidth - len(count) - 1
		if labelWidth < 1 {
			labelWidth = 1
		}
		label := truncateToWidth(sanitizeSingleLine(r.Label), labelWidth)

		// The Groups panel draws a configured group's own name in its
		// configured color (change per-group-colors), and the Archive and
		// Unknown rows (r.Value "archive"/"unknown") in their own configured
		// colors the same way (change archive-unknown-colors) - m.groupColors
		// holds both under those literal keys (see GroupColors), so this one
		// lookup serves all three with no extra branch. All (r.Value "") is
		// never a key in m.groupColors and so always falls through to no
		// color, keeping its own styling. Every other panel (Agents, Repos,
		// Tags) is restricted to id == panelGroups so a tag or repo that
		// happens to share a group's name is never colored by accident.
		groupColor := ""
		if id == panelGroups {
			groupColor = m.groupColors[r.Value]
		}

		// An applied value is marked in the text itself rather than by
		// colour alone, so the state survives NO_COLOR (spec
		// session-search, "Styling disabled by the environment"). Every
		// panel but Groups never marks its own "all" row (r.Value == "")
		// even while nothing is applied - moving through it previews
		// nothing (facet.Sel is set only by Enter), so an unmarked "all"
		// row is what "nothing is filtering yet" looks like. Groups is the
		// exception: All is a real, commonly-active view of its own, not
		// merely "no filter", so it is marked exactly like any other group
		// once it is the one actually showing (change
		// group-sessions-in-one-index).
		applied := r.Value == sel && (id == panelGroups || r.Value != "")
		appliedMark := " "
		switch {
		case applied:
			appliedMark = "●"
			label = style(label, groupColor+ansiBold, m.style)
		case groupColor != "":
			label = style(label, groupColor, m.style)
		}

		line := cursorMark + appliedMark + " " + label
		pad := inner - visibleWidth(line) - len(count)
		if pad < 1 {
			pad = 1
		}
		line += strings.Repeat(" ", pad) + style(count, ansiDim, m.style)
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
	rows, sessionRow := m.sessionDisplayRows()
	top := m.listTop
	if top < 0 {
		top = 0
	}
	if top > len(m.visible)-1 {
		top = len(m.visible) - 1
	}
	// Pull back while there is still slack: the pre-header version of this
	// clamped once, with `top > len(visible)-h`, to stop a stale scroll
	// position from leaving blank rows below a short tail. A header can sit
	// anywhere in that tail now, so "how many sessions fill h rows" is no
	// longer a subtraction - windowCost measures it directly, and the loop
	// keeps pulling top back one session at a time for as long as doing so
	// still fits, the same "maximize the window's use of the space it has"
	// intent as the single subtraction it replaces.
	for top > 0 && windowCost(rows, sessionRow, top-1, len(m.visible)-1) <= h {
		top--
	}
	inner := box.innerWidth()
	startDisplay := sessionRow[top]
	// top's own header (the row immediately before its session row, present
	// only when top is the first session of its bucket) is included only
	// when there is room left over for at least one more row after it - h>1
	// guarantees that, since exactly one session row (top's own) always
	// follows immediately below. At h==1 this is the small-terminal floor:
	// the selected session must always be visible even when its header
	// cannot be (see keepCursorVisible, which forces top==cursor whenever a
	// window this small cannot hold both).
	if h > 1 && startDisplay > 0 && rows[startDisplay-1].kind == rowHeader {
		startDisplay--
	}
	baseRowWidth := m.sessionRowWidth()
	drawn := 0
	for d := startDisplay; d < len(rows) && drawn < h; d++ {
		row := rows[d]
		if row.kind == rowHeader {
			box.Lines = append(box.Lines, dateHeaderLine(row.bucket, inner, m.style))
			drawn++
			continue
		}
		it := m.visible[row.session]
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
		rowOpts := RenderOptions{Width: m.sessionRowWidthFor(it, baseRowWidth), Style: m.style, InstallLabels: m.installLabels, GroupColors: m.groupColors}

		line := RenderRow(it, rowOpts) + marker
		if row.session == m.cursor {
			if m.style {
				line = highlightLine(line)
			}
			line = "▸ " + line
		} else {
			line = "  " + line
		}
		box.Lines = append(box.Lines, line)
		drawn++
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
	count := positionCount(m.cursor, len(m.visible))
	// The filtered-out sessions are still worth naming when there are any:
	// "12 of 40" alone would leave the user wondering where the other 75
	// went, which is the question the panels exist to keep answerable.
	if len(m.visible) != len(m.all) {
		count += fmt.Sprintf(" of %d", len(m.all))
	}
	if m.hidden > 0 && !m.showAll {
		count += fmt.Sprintf(" · %d hidden", m.hidden)
	}
	return count
}

// positionCount is the "6 of 38" a panel's border carries: where the cursor
// is and how many rows there are, so the size of a list and the reader's
// place in it are both legible without scrolling to the end of it.
func positionCount(cursor, total int) string {
	if total <= 0 {
		return "0 of 0"
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= total {
		cursor = total - 1
	}
	return fmt.Sprintf("%d of %d", cursor+1, total)
}

// detailPanel draws the right-hand pane: a tab strip in the top border and
// the selected session's content beneath it.
func (m browseModel) detailPanel(g geometry) panelBox {
	box := panelBox{
		Title:   m.tabStrip(),
		Count:   m.transcriptModeHint(),
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

// transcriptModeHint is the detail pane's top-border annotation while the
// Transcript tab is showing (change clean-transcript-mode): which of the
// two renderings is active, and the key that switches it, so the toggle is
// discoverable without opening the full `?` help. panelBox already colours
// this slot dim the same way sessionsCount's "N of M" is (see
// panelBox.borderLine), so it reads as chrome rather than content. Empty on
// every other tab - there is nothing to say there, the same way Count is
// "" for any panel with nothing to report.
func (m browseModel) transcriptModeHint() string {
	if m.tab != tabTranscript {
		return ""
	}
	if m.transcriptMode == transcriptFull {
		return "full · t hides tool calls"
	}
	return "clean · t shows tool calls"
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
		return renderItemTranscriptMode(m.convo, *it, m.transcriptPhrase(), m.installFor, m.transcriptMode, opts)
	case tabComments:
		return renderItemComments(m.db, *it, opts)
	}
	return renderItemDetail(m.db, *it, m.groups, m.installInfo, opts)
}

// installInfo resolves it's install to what the Detail tab's "install:"
// line shows: the display label (m.installLabels, never recomputed here)
// and the install's own config root, found the same way installFor finds
// it for the Transcript tab - by the install name embedded in the
// session's own composite id, not by "the profile being browsed" (change
// group-sessions-in-one-index). ok is false when no discovered install
// matches, in which case the line still shows the label (or the raw
// install name) without a root.
func (m browseModel) installInfo(it search.Item) (label, root string, ok bool) {
	label = m.installLabel(it.Install)
	p, err := m.installFor(it)
	if err != nil {
		return label, "", false
	}
	return label, p.Root(), true
}

// installFor resolves the discovered install that produced it, by the
// install name embedded in its own composite session id
// (session.InstallFromID) - not "the profile being browsed", now that one
// browsing session covers every install's data together (change
// group-sessions-in-one-index). This is what a database-backed source's
// conversation reader needs: the roots that say where that install's own
// database is. It is passed to the renderer as a function of the item
// rather than a value so the resolution happens only for the sources that
// need it - the file-backed ones already have a path.
func (m browseModel) installFor(it search.Item) (profile.Profile, error) {
	name := session.InstallFromID(it.SessionID)
	for _, p := range m.installs() {
		if p.Name == name {
			return p, nil
		}
	}
	return profile.Profile{}, fmt.Errorf("no discovered install named %q", name)
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
	if s := m.searchStatus(); s != "" {
		return s
	}
	var parts []string
	for _, a := range browseActions {
		if a.showsFor(m.focus) {
			parts = append(parts, a.key+" "+a.label)
		}
	}
	return style(truncateToWidth(strings.Join(parts, "  "), m.width), ansiDim, m.style)
}

// searchStatus is the footer line shown while a search phrase is in play:
// what is being searched for, how much it found, and the keys that act on
// it. It replaces the general action list rather than sharing the line,
// because a search is a mode the user is in and the keys that leave it are
// the ones worth the columns while they are in it.
//
// On the Transcript tab it counts matches within the conversation and says
// which one the pane is on; anywhere else it counts the sessions the phrase
// selected, which is what the phrase did at that point.
func (m browseModel) searchStatus() string {
	phrase := m.transcriptPhrase()
	if phrase == "" {
		return ""
	}
	quoted := "'" + sanitizeSingleLine(phrase) + "'"

	var line string
	if m.tab == tabTranscript && len(m.convo.hits) > 0 {
		line = fmt.Sprintf("Search: matches for %s (%d of %d)  n: next match, N: previous match",
			quoted, m.convo.hitIndex(m.detail.YOffset), len(m.convo.hits))
	} else if m.tab == tabTranscript {
		line = fmt.Sprintf("Search: %s - no match in this transcript", quoted)
	} else {
		line = fmt.Sprintf("Search: %s (%d sessions)", quoted, len(m.visible))
	}
	if m.query != "" {
		line += ",  s: change, X: clear"
	} else {
		line += ",  /: change, esc: clear"
	}
	return style(truncateToWidth(line, m.width), ansiDim, m.style)
}

// hitIndex is which match the pane is sitting on, 1-based, for the "1 of 2"
// in the search footer: the last match at or above the top visible line.
// Zero means the pane is scrolled above the first one.
func (c *conversationCache) hitIndex(yOffset int) int {
	idx := 0
	for i, h := range c.hits {
		if h <= yOffset {
			idx = i + 1
		}
	}
	return idx
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
	{key: "enter", label: "filter", help: "filter the sessions by the selected value", panels: []panelID{panelGroups, panelAgents, panelRepos, panelTags}, footer: true},
	{key: "esc", label: "clear filter", help: "clear what this panel is filtering by", panels: []panelID{panelSessions, panelGroups, panelAgents, panelRepos, panelTags}, footer: true},
	{key: "[/]", label: "tab", help: "previous/next tab in the detail pane", panels: []panelID{panelSessions, panelDetail}, footer: true},
	{key: "n/N", label: "next/previous match", help: "on the Transcript tab, scroll to the next/previous occurrence of the search phrase", panels: []panelID{panelSessions, panelDetail}},
	{key: "t", label: "clean/full", help: "on the Transcript tab, toggle between the clean question/answer view and the full technical transcript", panels: []panelID{panelSessions, panelDetail}},
	{key: "/", label: "narrow", help: "keep only the focused panel's rows containing what you type", footer: true},
	{key: "s", label: "search phrase", help: "full-text search over your own prompts"},
	{key: "x", label: "menu", help: "action menu for the focused panel - j/k or ↑/↓ move, enter applies, esc closes, / narrows by typing", footer: true},
	{key: "p", label: "group", help: "file the selected session into a group, archive it, or return it to automatic - same j/k, enter, esc, / as the action menu", footer: true},
	{key: "X", label: "clear all filters"},
	{key: "R", label: "refresh the index"},
	{key: "m/M", label: "add/remove a tag", help: "add/remove a tag on the selected session"},
	{key: "d", label: "remove this tag", help: "take the tag under the cursor off the selected session", panels: []panelID{panelTags}, footer: true},
	{key: "c/C", label: "add/remove a comment", help: "add/remove a comment on the selected session"},
	{key: "r", label: "rename", help: "set or clear lazyrecall's own name for the selected session (blank clears it)"},
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
	b.WriteString("\nPanels: 1 Groups  2 Agents  3 Repos  4 Tags  0 Sessions  (tab reaches the detail pane)\n")
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
func renderItemDetail(db *sqlitex.Runner, it search.Item, groups []config.Group, installInfo func(search.Item) (label, root string, ok bool), opts RenderOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id:     %s\n", it.SessionID)
	if it.Handle > 0 {
		fmt.Fprintf(&b, "handle: #%d\n", it.Handle)
	}
	fmt.Fprintf(&b, "agent:  %s\n", style(it.Source, ansiDim, opts.Style))
	// Which account produced this session - the install's display label,
	// and its config root so "which ~/.claude* is this" is answerable
	// without leaving the browser (change group-sessions-in-one-index).
	// Shown even when installInfo cannot resolve a root (ok=false): the
	// label (or, absent one, the raw install name) still says which
	// account, just not where its root is on disk.
	if it.Install != "" {
		label, root, ok := installInfo(it)
		if label == "" {
			label = it.Install
		}
		if ok && root != "" {
			label += " (" + abbreviateHome(root) + ")"
		}
		fmt.Fprintf(&b, "install: %s\n", style(label, ansiDim, opts.Style))
	}
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
	// The name shown here is the effective one: lazyrecall's own name
	// (CustomName, set from inside the browser or `lazyrecall name`) when
	// the user chose one, else the name the source tool recorded (Name -
	// Claude Code's rename). When a custom name is set AND the source
	// recorded a different one, the source's own name gets a line of its
	// own too - "agent name:" - so what the session was renamed away from,
	// from either direction, stays visible; the derived topic below is
	// shown separately either way, unlike the row, where name and topic
	// share one slot.
	customName := ""
	if it.CustomName != nil && *it.CustomName != "" {
		customName = *it.CustomName
	}
	sourceName := ""
	if it.Name != nil && *it.Name != "" {
		sourceName = *it.Name
	}
	effectiveName := customName
	if effectiveName == "" {
		effectiveName = sourceName
	}
	if effectiveName != "" {
		fmt.Fprintf(&b, "name:   %s\n", style(effectiveName, ansiBold, opts.Style))
	}
	if customName != "" && sourceName != "" && sourceName != customName {
		fmt.Fprintf(&b, "agent name: %s\n", style(sourceName, ansiDim, opts.Style))
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
	// No group line at all with no groups configured (change
	// group-sessions-in-one-index, "Zero-config and public users": the
	// browser must look exactly as it did before groups existed). With
	// groups configured, say *why* a session landed where it did - set
	// manually, a path rule (naming the specific path that matched, from
	// search.PathGroupPath), or Unknown when neither claims it.
	if len(groups) > 0 {
		group := "unknown"
		switch {
		case it.GroupManual:
			group = it.Group + " (set manually)"
		case it.Group != "":
			if _, path := search.PathGroupPath(groups, cwdOf(it)); path != "" {
				group = fmt.Sprintf("%s (path %s)", it.Group, abbreviateHome(path))
			} else {
				group = it.Group
			}
		}
		fmt.Fprintf(&b, "group:  %s\n", style(group, ansiDim, opts.Style))
	}

	fmt.Fprint(&b, "\ntags:   ")
	if len(it.Tags) == 0 {
		b.WriteString("(none)")
	} else {
		b.WriteString(style("#"+strings.Join(it.Tags, " #"), ansiMagenta, opts.Style))
	}
	b.WriteString("\n")

	// The most recent 3 comments, so the Detail tab already answers "is
	// there anything noted on this session" without switching tabs - the
	// Comments tab itself (renderItemComments) is unchanged and still shows
	// every one of them. "(none)" follows the same convention the tags line
	// above uses for an empty set, and an error reading them is shown
	// inline rather than losing the rest of an otherwise-successful render.
	fmt.Fprint(&b, "\ncomments: ")
	comments, cerr := annotate.CommentsForLineage(db, it.LineageID)
	switch {
	case cerr != nil:
		b.WriteString(cerr.Error())
		b.WriteString("\n")
	case len(comments) == 0:
		b.WriteString("(none)")
		b.WriteString("\n")
	default:
		b.WriteString("\n")
		recent := comments
		if len(comments) > 3 {
			recent = comments[len(comments)-3:]
		}
		renderCommentList(&b, recent, opts)
		if extra := len(comments) - len(recent); extra > 0 {
			fmt.Fprintf(&b, "(+%d more on the Comments tab)\n", extra)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// renderCommentList writes one comment per entry - its id, creation date,
// and wrapped body - in the format both the Comments tab (renderItemComments)
// and the Detail tab's preview use, so the two can never draw a comment
// differently depending on which one is asking.
func renderCommentList(b *strings.Builder, comments []annotate.Comment, opts RenderOptions) {
	for _, c := range comments {
		fmt.Fprintf(b, "%s %s\n",
			style(fmt.Sprintf("[%d]", c.ID), ansiDim, opts.Style),
			style(c.CreatedAt.Format("2006-01-02 15:04"), ansiDim, opts.Style))
		for _, line := range wrapToWidth(c.Body, opts.Width-2) {
			fmt.Fprintf(b, "  %s\n", line)
		}
	}
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
	renderCommentList(&b, comments, opts)
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
	// source says where this session's conversation can be read from,
	// which is what the three "nothing to show" messages are told apart
	// by: a source with no readable conversation at all, a source that
	// keeps transcript files but has no path recorded for this session,
	// and a database-backed source that simply had no turns.
	source conversationSource
	hits   []int
}

// conversationSource is where a session's conversation lives.
type conversationSource int

const (
	// sourceUnreadable is a source whose conversations this program cannot
	// read at all - antigravity, whose per-conversation detail is protobuf
	// with no available schema. It is the zero value because it is what a
	// cache that found no reader is left holding.
	sourceUnreadable conversationSource = iota
	// sourceKeepsTranscript writes per-session transcript files.
	sourceKeepsTranscript
	// sourceKeepsDatabase keeps the conversation in its own database.
	sourceKeepsDatabase
)

// load reads the session's conversation unless the cache already holds it.
//
// There are two ways to get one. claude, pi and omp write transcript files,
// which internal/transcript reads. goose, hermes, opencode and kilo write
// no file at all - but the conversation is not lost, it is in the same
// database the adapter already queries, so the adapter reads it back. Only
// antigravity has neither, because its per-conversation detail is protobuf
// with no available schema.
func (c *conversationCache) load(it search.Item, installFor func(search.Item) (profile.Profile, error)) {
	path := ""
	if it.TranscriptPath != nil {
		path = *it.TranscriptPath
	}
	key := it.SessionID + "\x00" + path
	if c.loaded && c.key == key {
		return
	}
	*c = conversationCache{key: key, loaded: true}

	if vocab, ok := transcript.VocabFor(it.Source); ok {
		c.source = sourceKeepsTranscript
		if path == "" {
			return
		}
		c.turns, c.dropped, c.err = transcript.Conversation(path, vocab, transcript.DefaultConversationLimits)
		return
	}

	reader, ok := refresh.ConversationReaderFor(it.Source, "")
	if !ok {
		return
	}
	c.source = sourceKeepsDatabase
	// The session's own install, not "the active profile" - one browsing
	// session now covers every install's data together (change
	// group-sessions-in-one-index), so a hermes/goose/opencode/kilo session
	// from install X must be read using X's root even when it is not the
	// install the browser happens to be showing anything else from.
	p, err := installFor(it)
	if err != nil {
		c.err = err
		return
	}
	c.turns, c.dropped, c.err = reader.Conversation(p, it.SourceSessionID, transcript.DefaultConversationLimits)
}

// renderItemTranscript renders the Transcript tab in full mode: every turn
// Conversation kept, exactly as the tab looked before clean mode existed.
// It is a thin wrapper around renderItemTranscriptMode rather than mode
// becoming a parameter here, so every existing caller - and every test
// written against this signature - keeps rendering exactly what it always
// has (change clean-transcript-mode: "Full mode must render exactly what
// renders today").
func renderItemTranscript(c *conversationCache, it search.Item, phrase string, installFor func(search.Item) (profile.Profile, error), opts RenderOptions) string {
	return renderItemTranscriptMode(c, it, phrase, installFor, transcriptFull, opts)
}

// renderItemTranscriptMode is what the browser actually calls: the
// conversation itself, as far back as the read budget allows, with the
// active search phrase highlighted, in either of the Transcript tab's two
// renderings (change clean-transcript-mode). This is the tab that answers
// "is this the session I meant" without having to resume it and find out -
// full mode as the technical record, clean mode as question/answer.
func renderItemTranscriptMode(c *conversationCache, it search.Item, phrase string, installFor func(search.Item) (profile.Profile, error), mode transcriptMode, opts RenderOptions) string {
	c.load(it, installFor)

	if c.err != nil {
		// A transcript the index has a path for but that cannot be read is
		// worth naming: the usual cause is the source tool having cleaned
		// it up (Claude Code's cleanupPeriodDays) since the last refresh.
		if os.IsNotExist(c.err) {
			return style("The transcript file is gone - the agent cleaned it up since the last refresh.", ansiDim, opts.Style)
		}
		return "transcript: " + c.err.Error()
	}
	if c.source == sourceUnreadable {
		return style("This agent stores its conversations in a format lazyrecall cannot decode, so there is nothing to read here. Prompts still shows what you asked.", ansiDim, opts.Style)
	}
	if c.source == sourceKeepsTranscript && (it.TranscriptPath == nil || *it.TranscriptPath == "") {
		return style("The index has no transcript file recorded for this session. A refresh (R) may pick one up.", ansiDim, opts.Style)
	}
	if len(c.turns) == 0 {
		return style("No readable turns in this transcript.", ansiDim, opts.Style)
	}

	// turns is what gets rendered below; c.turns itself is left untouched
	// either way, so switching modes never has to re-read anything and full
	// mode - which renders c.turns directly - can never see a clean-mode
	// side effect.
	turns := c.turns
	if mode == transcriptClean {
		turns = cleanTranscriptTurns(c.turns)
		if len(turns) == 0 {
			// Every turn Conversation kept was a tool call or a compaction
			// boundary - rare, but distinct from an empty conversation
			// (the check above): there is something to show, just not in
			// this mode.
			return style("Nothing left to show once tool calls are hidden - press t for the full transcript.", ansiDim, opts.Style)
		}
	}

	var b strings.Builder
	if c.dropped > 0 {
		fmt.Fprintf(&b, "%s\n\n", style(fmt.Sprintf("... %d earlier turns not shown; this is the end of the session.", c.dropped), ansiDim, opts.Style))
	}

	// Hits are collected against the plain text as it is written, before
	// styling adds escape sequences, so a match is counted where the
	// reader sees it and not where an escape happens to fall.
	//
	// words splits phrase on whitespace once, up front: search now matches
	// independent per-word prefix terms (sqlitex.FTS5PrefixTerms), so a line
	// counts as a hit - and gets highlighted - if it contains ANY of the
	// typed words, not only the whole phrase as one contiguous substring.
	// Without this a multi-word query could match a session in the index
	// (its words present anywhere, any order) while highlighting nothing at
	// all in a transcript where those words never appear adjacent. A
	// single-word query is exactly one entry in words, so this is a
	// superset of the old behaviour, not a change to it.
	//
	// Hits are recomputed from this same loop on every render regardless of
	// which mode is active or just became active - clean and full lay the
	// same phrase over different lines, so n/N always step through matches
	// that agree with what is actually on screen (change
	// clean-transcript-mode).
	c.hits = nil
	words := strings.Fields(phrase)
	line := strings.Count(b.String(), "\n")

	for i, t := range turns {
		if i > 0 {
			b.WriteString("\n")
			line++
		}
		fmt.Fprintf(&b, "%s\n", turnHeader(t, opts))
		line++
		for _, l := range transcriptBody(t, opts) {
			if containsAnyFold(l, words) {
				c.hits = append(c.hits, line)
			}
			fmt.Fprintf(&b, "  %s\n", highlightWords(l, words, opts))
			line++
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// cleanTranscriptTurns is what clean mode renders in place of the turns
// Conversation kept (change clean-transcript-mode): KindToolUse and
// KindCompactionBoundary turns removed, and the runs of KindAssistantText
// turns that removing them leaves adjacent merged into one block under the
// first turn's own header, the merged paragraphs separated by a blank line
// (transcriptBody already renders one wherever wrapToWidth sees "\n\n" in a
// turn's text, so joining with it is enough - no separate blank-line
// bookkeeping here). KindUserPrompt turns are never merged, even into a
// neighboring run of their own kind: a user turn marks a new question
// regardless of what does or does not separate it from the one before it.
//
// The merge only ever looks at the immediately preceding *kept* turn
// (out's last element), never at a turn already discarded above it - which
// is exactly what "left adjacent once removed" means: a reply, three tool
// calls, then another reply merges into one block the same as two replies
// with nothing between them, because nothing user-visible sits between
// them either way.
//
// A merged block's Truncated is set if any part of it was: transcriptBody
// appends one "... turn truncated" note at the end of the whole block
// rather than marking exactly which paragraph ran over the per-turn byte
// budget - truncation is rare enough (DefaultConversationLimits.
// MaxTurnBytes is 4000) that a block-level note is enough to say "there is
// more here than is shown," without teaching transcriptBody to interleave
// per-paragraph markers for a case this uncommon.
func cleanTranscriptTurns(turns []transcript.Turn) []transcript.Turn {
	out := make([]transcript.Turn, 0, len(turns))
	for _, t := range turns {
		switch t.Kind {
		case transcript.KindToolUse, transcript.KindCompactionBoundary:
			continue
		case transcript.KindAssistantText:
			if n := len(out); n > 0 && out[n-1].Kind == transcript.KindAssistantText {
				merged := &out[n-1]
				merged.Text += "\n\n" + t.Text
				merged.Truncated = merged.Truncated || t.Truncated
				continue
			}
			out = append(out, t)
		default:
			out = append(out, t)
		}
	}
	return out
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

// highlightWords marks every case-insensitive occurrence of any of words in
// line in reverse video, so the reason a session matched a search is
// visible in the transcript rather than only in the result row. Search
// matches each typed word as an independent prefix term (change
// group-sessions-in-one-index, review fix #5: sqlitex.FTS5PrefixTerms), so
// highlighting looks for each word separately rather than the whole typed
// phrase as one contiguous substring - the old behaviour, kept intact for a
// single-word query (words has exactly one entry then, so this reduces to
// exactly the old loop) but wrong for a multi-word one: "fix retry" can
// match a document with "fix" and "retry" nowhere near each other, and the
// old contiguous-substring search would then highlight nothing at all.
//
// Matches from different words can overlap (both "sketch" and "bar" occur
// inside "sketchybar") or sit back to back; spans are collected first and
// merged before rendering, so an overlapping pair prints as one highlighted
// run instead of two escape sequences fighting over the same characters.
func highlightWords(line string, words []string, opts RenderOptions) string {
	if !opts.Style || len(words) == 0 {
		return line
	}
	lower := strings.ToLower(line)
	type span struct{ start, end int }
	var spans []span
	for _, w := range words {
		if w == "" {
			continue
		}
		lw := strings.ToLower(w)
		for i := 0; i < len(lower); {
			idx := strings.Index(lower[i:], lw)
			if idx < 0 {
				break
			}
			start := i + idx
			end := start + len(lw)
			spans = append(spans, span{start, end})
			i = end
		}
	}
	if len(spans) == 0 {
		return line
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}

	var b strings.Builder
	prev := 0
	for _, s := range merged {
		b.WriteString(line[prev:s.start])
		b.WriteString(ansiReverse + line[s.start:s.end] + ansiReset)
		prev = s.end
	}
	b.WriteString(line[prev:])
	return b.String()
}

// containsAnyFold reports whether s contains any of words, case-insensitive
// - the highlighting-consistent replacement for a single containsFold(s,
// phrase) check (see highlightWords).
func containsAnyFold(s string, words []string) bool {
	lower := strings.ToLower(s)
	for _, w := range words {
		if w == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(w)) {
			return true
		}
	}
	return false
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
