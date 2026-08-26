// The in-process interactive browser (change replace-fzf-browser-with-tui,
// design.md decision 1): a bubbletea program that holds every piece of
// browsing state - active filters, active profile, selection, scroll
// position - in the running process. There is no external fuzzy finder, no
// coordination file, and no re-invocation of the binary; the terminal is
// owned and restored by the interface itself on every exit path.
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
	"github.com/lithammer/fuzzysearch/fuzzy"

	"recall/internal/annotate"
	"recall/internal/profile"
	"recall/internal/refresh"
	"recall/internal/search"
	"recall/internal/sqlitex"
)

// BrowserOptions carries the initial state for one browsing session.
type BrowserOptions struct {
	DB          *sqlitex.Runner // the active profile's database, already refreshed
	ProfileName string
	Repo        string // initial filter values (from command-line flags)
	Agent       string
	Tag         string
	Query       string
	Style       bool // NO_COLOR-aware: suppress all styling when the environment asks

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
	return profile.Resolve(profile.Discover(), name)
}

// discoverProfiles uses the injected lister, defaulting to the standard
// profile discovery when none was provided.
func (o BrowserOptions) discoverProfiles() []profile.Profile {
	if o.Profiles != nil {
		return o.Profiles()
	}
	return profile.Discover()
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

// inputMode is what the browser is currently prompting for. Prompts are
// input modes owned by the interface (design.md decision 1) - cancelling
// one (Esc) returns to browsing with no change applied, and submitting
// empty applies nothing except on a filter prompt, where it clears that one
// filter (spec session-search, "Leaving input mode" / design.md decision 2
// "INPUT MODE").
type inputMode int

const (
	modeNone inputMode = iota
	modeFuzzyFilter
	modeRepoFilter
	modeAgentFilter
	modeTagFilter
	modeSearchPhrase
	modeProfile
	modeAddTag
	modeRemoveTag
	modeAddComment
	modeRemoveComment
	// modeSelect is the one selection mode (change choose-from-known-values,
	// design.md decision 1) reused by every prompt whose valid values are
	// already known to the program: profile switch, agent filter, and tag
	// filter. m.selectFor records which of those three underlying actions a
	// selection is being made for - modeSelect itself is never the thing
	// submitInput dispatches on; submitSelect sets m.mode back to
	// m.selectFor before delegating to submitInput, so the existing
	// per-action switch there needs no new case.
	modeSelect
)

func (m inputMode) prompt() string {
	switch m {
	case modeFuzzyFilter:
		return "filter listed sessions (blank to clear): "
	case modeRepoFilter:
		return "repo filter (blank to clear): "
	case modeAgentFilter:
		return "agent filter (choose or type to narrow, blank to clear): "
	case modeTagFilter:
		return "tag filter (choose or type to narrow, blank to clear): "
	case modeSearchPhrase:
		return "search phrase (blank to clear): "
	case modeProfile:
		return "switch to profile (choose or type to narrow): "
	case modeAddTag:
		return "tag to add: "
	case modeRemoveTag:
		return "tag to remove: "
	case modeAddComment:
		return "comment to add: "
	case modeRemoveComment:
		return "comment id to remove: "
	}
	return ""
}

// browseModel is the running state of the browser. Everything here lives in
// the process; nothing is written to disk for coordination (design.md
// decision 3).
type browseModel struct {
	db          *sqlitex.Runner
	profileName string
	style       bool
	width       int
	height      int

	// Server-side filters: what the current result set was queried with.
	// Changing one re-queries (search.Search when a phrase is set,
	// search.List otherwise) - the same paths the non-interactive commands
	// use (design.md decision 4).
	repo, agent, tag, query string

	// Client-side fuzzy query: narrows the loaded rows in-process as the
	// user types (design.md decision 4; spec session-search, "Fuzzy
	// filtering of the loaded rows as the user types").
	fuzzyQuery string

	rows    []search.Item // the current server-side result set
	visible []search.Item // rows after fuzzy narrowing
	cursor  int           // selection index into visible
	listTop int           // first row index shown in the list pane

	detail *viewport.Model

	mode       inputMode
	input      textinput.Model
	inputLabel string
	resolve    func(name string) (profile.Profile, error)
	profiles   func() []profile.Profile

	// Selection-mode state (modeSelect - change choose-from-known-values,
	// design.md decision 1). selectAll is the full candidate list for the
	// active selection prompt; selectFiltered is selectAll narrowed by
	// m.input's current text (the same fuzzy matcher plain typing already
	// uses to narrow the loaded rows - design.md non-goal: "no new
	// fuzzy-matching"); selectCursor indexes into selectFiltered.
	selectFor      inputMode
	selectAll      []string
	selectFiltered []string
	selectCursor   int
	// selectCursorTouched becomes true the moment the user explicitly moves
	// the cursor (Up/Down) - see refilterSelect for why this distinction
	// matters (change fix-row-newlines-and-profile-switch, task 3.2).
	selectCursorTouched bool

	help   bool // the full action list is showing
	notice string

	selected   search.Item
	selectedOK bool
}

// Init satisfies tea.Model: the browser needs no startup command beyond
// the initial refresh already performed by the caller before the program
// was created (design.md decision 5: refresh once on open).
func (m browseModel) Init() tea.Cmd {
	return nil
}

func newBrowseModel(opts BrowserOptions) browseModel {
	m := browseModel{
		db:          opts.DB,
		profileName: opts.ProfileName,
		style:       opts.Style,
		resolve:     opts.resolve,
		profiles:    opts.discoverProfiles,
		width:       DefaultWidth,
		height:      24,
		repo:        opts.Repo,
		agent:       opts.Agent,
		tag:         opts.Tag,
		query:       opts.Query,
		input:       textinput.New(),
	}
	m.input.CharLimit = 1000
	detail := viewport.New(m.width, 12)
	m.detail = &detail
	m.reload()
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
// Queries
// ---------------------------------------------------------------------

func (m browseModel) queryRows() ([]search.Item, error) {
	f := search.Filter{Agent: m.agent, Repo: m.repo, Tag: m.tag}
	if m.query != "" {
		return search.Search(m.db, m.query, f)
	}
	return search.List(m.db, f)
}

// agentCandidates lists the agents sessions actually exist for in the
// active profile (change choose-from-known-values, design.md decision 2:
// "taken from the sessions present in the active profile, not from a fixed
// list of adapter names") - queried unfiltered so switching from an already
// narrowed view still offers every agent the profile has, not just the ones
// visible under the current filters.
func (m browseModel) agentCandidates() []string {
	rows, err := search.List(m.db, search.Filter{})
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, it := range rows {
		if it.Source != "" && !seen[it.Source] {
			seen[it.Source] = true
			out = append(out, it.Source)
		}
	}
	sort.Strings(out)
	return out
}

// tagCandidates lists the tags currently in use in the active profile
// (design.md decision 2; "tags from annotate.AllTags") - the tag *filter*
// only, distinct from tag creation (m/modeAddTag), which must keep
// accepting a value that does not exist yet (design.md decision 3).
func (m browseModel) tagCandidates() []string {
	tags, err := annotate.AllTags(m.db)
	if err != nil {
		return nil
	}
	return tags
}

// profileCandidates lists every profile that can be switched to, excluding
// the one already active - switching to the current profile would be a
// no-op, and it is not a useful choice to offer (design.md decision 2's
// "a value that would return nothing is not a useful choice" reasoning
// applies equally here).
func (m browseModel) profileCandidates() []string {
	var out []string
	for _, p := range m.profiles() {
		if p.Name != "" && p.Name != m.profileName {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// matchText is the per-row haystack fuzzy filtering matches against:
// everything the user can see on the row plus the handle.
func matchText(it search.Item) string {
	parts := []string{it.Source}
	if it.Handle > 0 {
		parts = append(parts, fmt.Sprintf("#%d", it.Handle))
	}
	if it.CWD != nil {
		parts = append(parts, *it.CWD)
	}
	if it.GitBranch != nil && *it.GitBranch != "" {
		parts = append(parts, *it.GitBranch)
	}
	if it.Name != nil {
		parts = append(parts, *it.Name)
	}
	if it.Topic != nil {
		parts = append(parts, *it.Topic)
	}
	if it.LastPrompt != nil {
		parts = append(parts, *it.LastPrompt)
	}
	parts = append(parts, it.Tags...)
	return strings.Join(parts, " ")
}

// applyFuzzy narrows rows to the fuzzy matches of m.fuzzyQuery, best
// matches first. An empty query keeps the rows as loaded.
func (m browseModel) applyFuzzy(rows []search.Item) []search.Item {
	if m.fuzzyQuery == "" {
		return rows
	}
	targets := make([]string, len(rows))
	for i := range rows {
		targets[i] = matchText(rows[i])
	}
	ranks := fuzzy.RankFindFold(m.fuzzyQuery, targets)
	out := make([]search.Item, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, rows[r.OriginalIndex])
	}
	return out
}

// reload re-runs the current server-side query and re-applies the fuzzy
// filter, clamping the selection into the new result set.
func (m *browseModel) reload() {
	rows, err := m.queryRows()
	if err != nil {
		m.notice = "browse: " + err.Error()
		return
	}
	m.rows = rows
	m.visible = m.applyFuzzy(rows)
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

func (m *browseModel) current() *search.Item {
	if m.cursor >= 0 && m.cursor < len(m.visible) {
		return &m.visible[m.cursor]
	}
	return nil
}

// keepCursorVisible scrolls the list window so the cursor row is shown.
func (m *browseModel) keepCursorVisible() {
	h := m.listHeight()
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
		if m.detail != nil {
			m.detail.Width = msg.Width
			m.detail.Height = m.detailHeight()
		}
		m.keepCursorVisible()
		return m, nil

	case refreshDoneMsg:
		if msg.err != nil {
			m.notice = "refresh failed: " + msg.err.Error()
		} else {
			m.notice = "index refreshed."
			m.reload()
		}
		return m, nil

	case dbSwitchedMsg:
		if msg.err != nil {
			m.notice = "profile switch failed: " + msg.err.Error()
			return m, nil
		}
		m.db = msg.db
		m.profileName = msg.name
		m.cursor = 0
		m.listTop = 0
		m.reload()
		return m, nil
	}

	// Input modes own every key while active (design.md decision 6):
	// Esc/Ctrl-C cancels without applying, Enter submits.
	if m.mode != modeNone {
		return m.updateInputMode(msg)
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.help {
			switch msg.Type {
			case tea.KeyEsc, tea.KeyCtrlC, tea.KeyRunes:
				if msg.Type != tea.KeyRunes || string(msg.Runes) == "?" {
					m.help = false
				}
			}
			return m, nil
		}
		return m.handleBrowseKey(msg)
	}
	return m, nil
}

// handleBrowseKey dispatches normal-mode keys. Normal mode is unmodified
// keys only (design.md decision 1: "The browser becomes modal") - every
// action below is bound exactly as design.md decision 2's binding table
// specifies, with no substituted or added keys. Alt/Meta combinations are
// never bound to anything: the user's window manager reserves them, so they
// would never reach the program (spec session-search, "The browser binds no
// Alt combinations").
func (m *browseModel) handleBrowseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// No action is ever bound to Alt/Meta (spec session-search, "The browser
	// binds no Alt combinations") - checked once, up front, rather than
	// per-case, so it can never be missed by a future addition below.
	if msg.Alt {
		return m, nil
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		m.selectedOK = false
		return m, tea.Quit

	case tea.KeyEnter:
		if it := m.current(); it != nil {
			m.selected = *it
			m.selectedOK = true
			return m, tea.Quit
		}
		return m, nil

	case tea.KeyUp:
		m.moveSelection(-1)
		return m, nil
	case tea.KeyDown:
		m.moveSelection(1)
		return m, nil
	case tea.KeyCtrlU:
		m.moveSelection(-m.halfScreen())
		return m, nil
	case tea.KeyCtrlD:
		m.moveSelection(m.halfScreen())
		return m, nil

	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "j":
			m.moveSelection(1)
		case "k":
			m.moveSelection(-1)
		case "g":
			m.cursor = 0
			m.listTop = 0
		case "G":
			m.cursor = len(m.visible) - 1
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.keepCursorVisible()
		case "q":
			m.selectedOK = false
			return m, tea.Quit
		case "?":
			m.help = true
		case "/":
			return m.startInput(modeFuzzyFilter)
		case "s":
			return m.startInput(modeSearchPhrase)
		case "r":
			// The repository filter stays free text: repository paths
			// cannot be enumerated usefully (design.md decision 3).
			return m.startInput(modeRepoFilter)
		case "a":
			return m.startSelect(modeAgentFilter, m.agentCandidates())
		case "t":
			return m.startSelect(modeTagFilter, m.tagCandidates())
		case "p":
			return m.startSelect(modeProfile, m.profileCandidates())
		case "x":
			m.repo, m.agent, m.tag, m.query, m.fuzzyQuery = "", "", "", "", ""
			m.reload()
		case "R":
			return m, m.refreshCmd()
		case "m":
			return m.startInput(modeAddTag)
		case "M":
			return m.startInput(modeRemoveTag)
		case "c":
			return m.startInput(modeAddComment)
		case "C":
			return m.startInput(modeRemoveComment)
		}
		return m, nil
	}

	// Every other key (including any Alt/Meta combination) is inert in
	// normal mode: it is neither a bound action nor text, because normal
	// mode never enters text.
	return m, nil
}

// moveSelection shifts the cursor by delta rows, clamped to the visible
// range, and keeps it scrolled into view.
func (m *browseModel) moveSelection(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if lastIdx := len(m.visible) - 1; m.cursor > lastIdx {
		m.cursor = lastIdx
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.keepCursorVisible()
}

// halfScreen is the number of rows Ctrl-D/Ctrl-U move by - half of the
// list pane's height, by vim convention (design.md decision 2 notes: "Ctrl-D
// is half-screen-down by convention").
func (m *browseModel) halfScreen() int {
	h := m.listHeight() / 2
	if h < 1 {
		h = 1
	}
	return h
}

// startInput begins a free-text input mode: the prompt is drawn by the
// interface, and the text input owns the keyboard until submitted or
// cancelled. The returned command starts the input cursor blinking.
//
// The input's placeholder is deliberately left blank (change
// choose-from-known-values, task 3.1) - it used to be assigned mode.prompt(),
// which is also what m.inputLabel renders right before the input box, so a
// prompt read "switch to profile: switch to profile:" on screen (the label,
// shown by View(), immediately followed by the same text again as the empty
// field's placeholder). A prompt's label is only ever shown once now.
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
	cmd := m.input.Focus()
	return m, cmd
}

// startSelect begins the one selection mode (design.md decision 1), used by
// every prompt whose valid values are already known to the program: the
// profile switch, the agent filter, and the tag filter. target names which
// of those the selection is for; candidates is what's offered.
//
// An empty candidate set is reported and applies nothing rather than
// entering selection mode over an empty list (design.md decision 2
// consequence; task 1.4) - there is nothing useful to choose from, and a
// value that would return nothing is not a useful choice.
func (m *browseModel) startSelect(target inputMode, candidates []string) (tea.Model, tea.Cmd) {
	if len(candidates) == 0 {
		m.notice = "no " + selectNoun(target) + " to choose from."
		return m, nil
	}
	m.mode = modeSelect
	m.selectFor = target
	m.selectAll = candidates
	m.selectFiltered = candidates
	// -1: nothing highlighted yet, mirroring the blank default a free-text
	// prompt starts with. Confirming right away (Enter with no navigation
	// AND no typing) therefore behaves exactly like the old empty-submit
	// did - applies nothing, except on a filter prompt, where it clears
	// that filter (design.md decision 4) - rather than surprising the user
	// by acting on whatever candidate happens to sort first. Once the user
	// types anything that narrows the list, refilterSelect auto-highlights
	// the best match instead (task 3.2 fix) - see its comment.
	m.selectCursor = -1
	m.selectCursorTouched = false
	m.inputLabel = target.prompt()
	m.input.Prompt = ""
	m.input.Placeholder = ""
	m.input.SetValue("")
	m.input.Width = m.width - len(m.inputLabel) - 2
	if m.input.Width < 10 {
		m.input.Width = 10
	}
	cmd := m.input.Focus()
	return m, cmd
}

// selectNoun names what's missing in the empty-candidate-set notice.
func selectNoun(target inputMode) string {
	switch target {
	case modeAgentFilter:
		return "agents"
	case modeTagFilter:
		return "tags"
	case modeProfile:
		return "other profiles"
	}
	return "values"
}

func (m *browseModel) updateInputMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.mode = modeNone
			m.input.Blur()
			m.notice = ""
			return m, nil
		case tea.KeyEnter:
			if m.mode == modeSelect {
				return m, m.submitSelect()
			}
			cmd := m.submitInput(strings.TrimSpace(m.input.Value()))
			return m, cmd
		case tea.KeyUp:
			if m.mode == modeSelect {
				m.moveSelectCursor(-1)
				return m, nil
			}
		case tea.KeyDown:
			if m.mode == modeSelect {
				m.moveSelectCursor(1)
				return m, nil
			}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.mode == modeSelect {
		m.refilterSelect()
	}
	return m, cmd
}

// moveSelectCursor shifts the highlighted candidate by delta, clamped to
// the narrowed candidate list. -1 (nothing highlighted) is a valid position,
// reachable again by moving up from the first candidate - so a user who
// navigated past the value they wanted can always get back to "apply
// nothing" without cancelling and reopening the prompt.
func (m *browseModel) moveSelectCursor(delta int) {
	m.selectCursorTouched = true
	m.selectCursor += delta
	if m.selectCursor < -1 {
		m.selectCursor = -1
	}
	if last := len(m.selectFiltered) - 1; m.selectCursor > last {
		m.selectCursor = last
	}
}

// refilterSelect narrows selectAll to selectFiltered by the input's current
// text (task 1.1: "narrowable by typing"), reusing the exact fuzzy matcher
// plain typing already uses to narrow the loaded rows (design.md non-goal:
// no new fuzzy-matching change) rather than a second matching scheme.
//
// Established cause of "choosing a profile has no effect" (change
// fix-row-newlines-and-profile-switch, task 3.1, reported after this
// change's first pass wrongly concluded the switch path had no bug):
// typing to narrow the list used to never move selectCursor off -1, only
// clamp it. The real, reported user flow is open the prompt, type to
// narrow ("claude"), press Enter - never touching Up/Down at all. With the
// old clamp-only logic that flow left the cursor at -1 even after typing
// narrowed the list to exactly the wanted candidate, so Enter submitted an
// empty value and submitInput's "empty value applies nothing" case fired
// silently - the switch (or agent/tag filter - all three selection prompts
// share this function) never happened, and nothing on screen indicated why.
//
// Fix: once the user has typed something that narrows the list AND has
// never explicitly navigated with Up/Down (selectCursorTouched), the best
// (first-ranked) match is auto-highlighted, so Enter after typing selects
// it - matching what typing-then-Enter looks like it should do. Clearing
// the typed text back to blank (still untouched) reverts to -1, exactly
// mirroring the state the prompt opened in, so "open and confirm
// immediately" (design.md decision 4 from choose-from-known-values:
// confirming with nothing selected applies nothing, except a filter
// prompt clears that filter) is unchanged - that convention only ever
// covered the case where the user typed nothing, not the case where
// typing had already narrowed the list to a single obvious choice. The
// moment the user does press Up/Down, selectCursorTouched latches true
// and this function goes back to pure clamping, so a cursor the user
// positioned on purpose is never silently overridden by further typing.
func (m *browseModel) refilterSelect() {
	q := m.input.Value()
	if q == "" {
		m.selectFiltered = m.selectAll
	} else {
		ranks := fuzzy.RankFindFold(q, m.selectAll)
		out := make([]string, 0, len(ranks))
		for _, r := range ranks {
			out = append(out, m.selectAll[r.OriginalIndex])
		}
		m.selectFiltered = out
	}

	if !m.selectCursorTouched {
		if q != "" && len(m.selectFiltered) > 0 {
			m.selectCursor = 0
		} else {
			m.selectCursor = -1
		}
		return
	}

	if last := len(m.selectFiltered) - 1; m.selectCursor > last {
		m.selectCursor = last
	}
}

// submitSelect applies the highlighted candidate, if any, for the prompt
// modeSelect was entered for. It delegates to submitInput by temporarily
// restoring m.mode to m.selectFor, so the one switch in submitInput that
// already knows how to apply each of these three prompts' values needs no
// second, selection-specific copy. Confirming with nothing highlighted
// (task 1.3) is exactly submitInput's existing empty-value case: applies
// nothing, except on a filter prompt (agent/tag), where it clears that
// filter - the same convention every other prompt already has.
func (m *browseModel) submitSelect() tea.Cmd {
	value := ""
	if m.selectCursor >= 0 && m.selectCursor < len(m.selectFiltered) {
		value = m.selectFiltered[m.selectCursor]
	}
	m.mode = m.selectFor
	return m.submitInput(value)
}

// submitInput applies the submitted value for the current input mode. An
// empty value applies nothing (spec session-search, "Submitting an empty
// value"): clearing a filter is done by submitting a blank line to that
// filter's prompt - an explicit action, not an accident. Returns the
// command to run after applying (the profile switch, when the mode was a
// profile switch).
func (m *browseModel) submitInput(value string) tea.Cmd {
	mode := m.mode
	m.mode = modeNone
	m.input.Blur()

	switch mode {
	case modeFuzzyFilter:
		m.fuzzyQuery = value // blank clears this one filter, leaving the rest
	case modeRepoFilter:
		m.repo = value // blank clears this one filter, leaving the rest
	case modeAgentFilter:
		m.agent = value
	case modeTagFilter:
		m.tag = value
	case modeSearchPhrase:
		m.query = value
	case modeProfile:
		if value == "" {
			return nil // submitting empty to the profile prompt applies nothing
		}
		return m.switchProfileCmd(value)
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
	m.reload()
	return nil
}

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
// Layout and View
// ---------------------------------------------------------------------

// separatorLines is how many separator rules View() actually draws: one
// above the list and one between the list and the detail pane. listHeight
// and detailHeight must budget for both, or the rendered frame ends up one
// line taller than the terminal and the terminal scrolls it - silently
// pushing the header (profile, active filters) off the top of the screen.
// That was a pre-existing off-by-one (found and fixed while verifying this
// change over a real pty, task 5.2/5.3 - unrelated to the key rebinding
// itself; see devdocs/fyi.md) that only a real terminal's own scrolling
// could surface: a string comparison in a unit test has no terminal height
// to overflow.
const separatorLines = 2

func (m browseModel) listHeight() int {
	avail := m.height - m.headerLines() - m.footerLines() - separatorLines
	if m.mode != modeNone {
		avail-- // the input line
	}
	d := avail * 2 / 5
	if d < 4 {
		d = 4
	}
	l := avail - d
	if l < 1 {
		l = 1
	}
	return l
}

func (m browseModel) detailHeight() int {
	avail := m.height - m.headerLines() - m.footerLines() - separatorLines
	if m.mode != modeNone {
		avail--
	}
	d := avail * 2 / 5
	if d < 4 {
		d = 4
	}
	return d
}

func (m browseModel) headerLines() int { return 2 }
func (m browseModel) footerLines() int {
	if m.notice != "" {
		return 1
	}
	return 1
}

// rowDecorationWidth is the number of columns View() always draws in front
// of a row's own content: the two-column selection marker "▸ " on the
// selected row, and equal padding "  " on every other row (design.md
// decision 1 / fix-row-width-budget: the width handed to row rendering
// must already exclude this, not have it added on afterwards - trimming a
// styled row after the fact risks cutting inside an ANSI escape sequence).
const rowDecorationWidth = 2

func (m browseModel) View() string {
	if m.help {
		return m.helpView()
	}
	if m.mode == modeSelect {
		return m.selectView()
	}
	opts := RenderOptions{Width: m.width, Style: m.style}
	// The detail pane carries no decoration, so it renders at the full
	// width; only list rows need the decoration reserved out of their
	// budget before shortening runs (RenderRow itself decides what to
	// shorten - never trim its output afterwards).
	rowOpts := opts
	rowOpts.Width -= rowDecorationWidth
	if rowOpts.Width < 1 {
		// Never pass <= 0 through to RenderRow: 0/negative is that
		// function's "width wasn't set at all, use the default" sentinel,
		// which is the opposite of what a too-narrow terminal means here.
		rowOpts.Width = 1
	}

	var b strings.Builder
	// Header line 1: the active profile (always identifiable - spec
	// session-search, "Active profile is visible while browsing") and every
	// filter in effect (spec, "The filters currently in effect SHALL be
	// visible while browsing"). repo, query, and the fuzzy filter below are
	// free text typed or pasted directly into a prompt (design.md decision
	// 3: the repo filter stays free text; the search phrase and fuzzy
	// filter always have) and so can carry a control character the same way
	// any other session text can - sanitized here for the same reason row
	// text is (change fix-row-newlines-and-profile-switch, task 1.3: apply
	// in every single-line context, including the header).
	fmt.Fprintf(&b, "%s", style("profile: "+sanitizeSingleLine(m.profileName), ansiBold, m.style))
	if m.repo != "" {
		fmt.Fprintf(&b, "  repo: %s", sanitizeSingleLine(m.repo))
	}
	if m.agent != "" {
		fmt.Fprintf(&b, "  agent: %s", sanitizeSingleLine(m.agent))
	}
	if m.tag != "" {
		fmt.Fprintf(&b, "  tag: %s", sanitizeSingleLine(m.tag))
	}
	if m.query != "" {
		fmt.Fprintf(&b, "  search: %q", sanitizeSingleLine(m.query))
	}
	if m.repo == "" && m.agent == "" && m.tag == "" && m.query == "" {
		b.WriteString("  (no filters)")
	}
	b.WriteString("\n")

	// Header line 2: the in-process fuzzy filter box, so the user always
	// sees what typing will narrow by.
	fmt.Fprintf(&b, "filter: %s\n", sanitizeSingleLine(m.fuzzyQuery))

	b.WriteString(strings.Repeat("─", min(m.width, 120)) + "\n")

	// List pane.
	listH := m.listHeight()
	if len(m.visible) == 0 {
		b.WriteString("No session matched the filters in use.\n")
	} else {
		m.keepCursorVisible()
		end := m.listTop + listH
		if end > len(m.visible) {
			end = len(m.visible)
		}
		for i := m.listTop; i < end; i++ {
			line := RenderRow(m.visible[i], rowOpts)
			if i == m.cursor {
				if m.style {
					line = highlightLine(line)
				}
				line = "▸ " + line
			} else {
				line = "  " + line
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString(strings.Repeat("─", min(m.width, 120)) + "\n")

	// Detail pane.
	if m.detail != nil {
		m.detail.SetContent(m.detailText(opts))
		b.WriteString(m.detail.View())
		b.WriteString("\n")
	}

	// Input line.
	if m.mode != modeNone {
		fmt.Fprintf(&b, "%s%s", m.inputLabel, m.input.View())
		b.WriteString("\n")
	}

	// Footer: a transient notice if there is one, otherwise the common
	// actions (spec session-search, "Common actions are visible without
	// being requested").
	if m.notice != "" {
		b.WriteString(style(m.notice, ansiDim, m.style))
	} else {
		b.WriteString(style(browseActionsHint(), ansiDim, m.style))
	}
	return b.String()
}

// selectView draws the one selection mode (design.md decision 1): the
// prompt's own label, the narrowing text typed so far, and the candidates
// it currently matches with the highlighted one marked - the same "▸ "/"  "
// decoration and one-line-per-entry invariant fix-row-width-budget
// established for session rows, applied here to candidate rows too (task
// 1.1's "reused by every prompt" extends to how a candidate is drawn, not
// only to how it is chosen).
func (m browseModel) selectView() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", style("profile: "+sanitizeSingleLine(m.profileName), ansiBold, m.style))
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s%s\n", m.inputLabel, m.input.View())
	b.WriteString(strings.Repeat("─", min(m.width, 120)) + "\n")

	if len(m.selectFiltered) == 0 {
		b.WriteString("No values match.\n")
	} else {
		h := m.listHeight()
		top := 0
		if m.selectCursor >= h {
			top = m.selectCursor - h + 1
		}
		end := top + h
		if end > len(m.selectFiltered) {
			end = len(m.selectFiltered)
		}
		candidateWidth := m.width - rowDecorationWidth
		if candidateWidth < 1 {
			candidateWidth = 1
		}
		for i := top; i < end; i++ {
			// A tag candidate is free text a user typed when creating a
			// tag (modeAddTag stays free text - design.md decision 3) and
			// so can carry a control character the same as any other
			// session text (change fix-row-newlines-and-profile-switch,
			// task 1.3: "the selection candidates").
			line := truncateToWidth(sanitizeSingleLine(m.selectFiltered[i]), candidateWidth)
			if i == m.selectCursor {
				if m.style {
					line = highlightLine(line)
				}
				line = "▸ " + line
			} else {
				line = "  " + line
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString(strings.Repeat("─", min(m.width, 120)) + "\n")

	if m.notice != "" {
		b.WriteString(style(m.notice, ansiDim, m.style))
	} else {
		b.WriteString(style("↑/↓ move  enter choose  esc cancel, applies nothing", ansiDim, m.style))
	}
	return b.String()
}

func (m browseModel) detailText(opts RenderOptions) string {
	it := m.current()
	if it == nil {
		return "No session selected."
	}
	return renderItemDetail(m.db, *it, opts)
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

// ---------------------------------------------------------------------
// Help and action table (spec session-search, "The browser's actions are
// discoverable")
// ---------------------------------------------------------------------

type browseAction struct {
	key    string
	label  string
	common bool // shown in the footer hint without being requested
}

// browseActions is the single binding table (design.md decision 3): the
// footer hints and the help overlay are both derived from it, so the keys
// advertised can never drift from the keys actually bound. It transcribes
// design.md decision 2's binding table exactly - do not add, remove, or
// substitute a key here without also changing handleBrowseKey/
// updateInputMode to match, and vice versa.
var browseActions = []browseAction{
	{"j/k, ↑/↓", "move selection", true},
	{"Ctrl-D/Ctrl-U", "move by half a screen", false},
	{"g/G", "first/last session", false},
	{"enter", "resume selected session", true},
	{"q, Ctrl-C", "quit", true},
	{"?", "help overlay", true},
	{"/", "filter the listed sessions", true},
	{"s", "set the search phrase", true},
	{"r", "repository filter", false},
	{"a", "agent filter", false},
	{"t", "tag filter", false},
	{"p", "switch profile", false},
	{"x", "clear all filters", false},
	{"R", "refresh the index", false},
	{"m/M", "add/remove a tag", true},
	{"c/C", "add/remove a comment", true},
	{"esc", "cancel input, apply nothing", false},
}

func browseActionsHint() string {
	var parts []string
	for _, a := range browseActions {
		if a.common {
			parts = append(parts, a.key+" "+a.label)
		}
	}
	return strings.Join(parts, "  ")
}

func (m browseModel) helpView() string {
	var b strings.Builder
	b.WriteString("recall browse - key bindings\n\n")
	for _, a := range browseActions {
		fmt.Fprintf(&b, "  %-22s %s\n", a.key, a.label)
	}
	b.WriteString("\n")
	b.WriteString(style("? or esc: close this help", ansiDim, m.style))
	return b.String()
}

// renderItemDetail renders the detail pane for one selected session: its
// composite identifier, handle, working directory, agent, end state, topic,
// and its comments and tags (spec session-search, "Session detail is shown
// alongside the list"), styled to the same standard as a directly printed
// listing (spec, "The browser is styled") - reusing RenderRow's own colour
// choices field-for-field so the detail pane and the row list never
// disagree about what a field means visually. The identifier and the
// handle are deliberately left unstyled: they are the copyable, plain-text
// forms the non-interactive commands accept (task 4.7).
func renderItemDetail(db *sqlitex.Runner, it search.Item, opts RenderOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id:     %s\n", it.SessionID)
	if it.Handle > 0 {
		fmt.Fprintf(&b, "handle: #%d\n", it.Handle)
	}
	fmt.Fprintf(&b, "agent:  %s\n", style(it.Source, ansiDim, opts.Style))
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

	comments, cErr := annotate.CommentsForLineage(db, it.LineageID)
	fmt.Fprint(&b, "\ncomments:\n")
	if cErr != nil {
		fmt.Fprintf(&b, "  error: %v\n", cErr)
	} else if len(comments) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, c := range comments {
			fmt.Fprintf(&b, "  [%d] %s: %s\n", c.ID, c.CreatedAt.Format(time.RFC3339), c.Body)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}
