// Row rendering: one session per line, the handle inline, independently
// truncated to fit the output width, optionally styled (spec
// session-search, "A listed session occupies one line that fits the
// terminal" / "Fields are visually distinguishable"; design.md decisions
// 5-7; tasks 4.1-4.5, 5.1-5.5). This is the one row renderer shared by
// list, search, and review output (task 4.5), and by the interactive
// picker (internal/cli/pick.go) - and, per the plan for
// add-interactive-browse, its `browse` command's row renderer too.
package cli

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"

	"lazyrecall/internal/search"
	"lazyrecall/internal/session"
)

// slot indices into the fixed-order row layout. Optional fields (age,
// topic, tags) are simply the empty string when absent; empty slots are
// skipped entirely when the row is joined, rather than leaving a blank gap.
const (
	slotHandle = iota
	slotSource
	slotLocation
	slotAge
	slotState
	slotTopic
	slotTags
	numSlots
)

// RenderRow renders one session as a single line: "#<handle> [<source>]
// <location> <age> [<end state>] "<name or topic>"  #<tags>", truncated to
// opts.Width and, when opts.Style is set, with ANSI styling that
// distinguishes each field (task 5.2) and marks sessions needing attention
// (task 5.3). The string returned never contains a newline, since callers
// including the numbered-list picker (pick.go) print it as a single line
// per session.
func RenderRow(it search.Item, opts RenderOptions) string {
	width := opts.Width
	if width <= 0 {
		width = DefaultWidth
	}

	slots := rowSlots(it)
	fitRowToWidth(slots, width)

	var b strings.Builder
	first := true
	for i, s := range slots {
		if s == "" {
			continue
		}
		if !first {
			b.WriteString(" ")
		}
		first = false
		b.WriteString(style(s, slotColor(i, it), opts.Style))
	}
	return b.String()
}

// rowText is what the topic slot shows, in descending order of how
// deliberately it names the session: the name the user gave it inside the
// source tool, then the agent-written topic, then the last prompt. A
// chosen name wins over a derived one - the reason Name is a field of its
// own rather than something merged into Topic during indexing (change
// show-session-names). The same precedence is used by the browse detail
// pane, which additionally shows the name and the topic on separate lines.
func rowText(it search.Item) string {
	if it.Name != nil && *it.Name != "" {
		return *it.Name
	}
	if it.Topic != nil && *it.Topic != "" {
		return *it.Topic
	}
	if it.LastPrompt != nil && *it.LastPrompt != "" {
		return *it.LastPrompt
	}
	return ""
}

func rowSlots(it search.Item) []string {
	slots := make([]string, numSlots)

	if it.Handle > 0 {
		slots[slotHandle] = fmt.Sprintf("#%d", it.Handle)
	} else {
		slots[slotHandle] = "#?"
	}
	slots[slotSource] = "[" + it.Source + "]"

	location := "(no working directory)"
	if it.CWD != nil {
		location = abbreviateHome(*it.CWD)
		if it.DirExists != nil && !*it.DirExists {
			location += " (missing)"
		}
	}
	slots[slotLocation] = location

	if it.LastActivityAt != nil {
		slots[slotAge] = relativeTime(*it.LastActivityAt)
	}

	slots[slotState] = "[" + string(it.EndState) + "]"

	if text := rowText(it); text != "" {
		slots[slotTopic] = `"` + text + `"`
	}

	if len(it.Tags) > 0 {
		slots[slotTags] = "#" + strings.Join(it.Tags, " #")
	}

	// Neutralise line breaks, carriage returns, tabs, and other control
	// characters before fitRowToWidth ever runs (change
	// fix-row-newlines-and-profile-switch, design.md decision 1: the
	// single-line boundary, before shortening - shortening is column
	// arithmetic and assumes its input is already one line). This is the
	// third "budgeted lines drawn != lines expected" defect in this row
	// renderer: the first missed that View() draws two separator lines,
	// not one; the second shortened to the full width and only then
	// prepended two columns of decoration; this one shortened correctly but
	// let session text - topic and last-prompt text routinely contain a
	// line break, since agent prompts are frequently multi-line - introduce
	// line breaks of its own. A session whose text BEGAN with a line break
	// rendered as an entirely blank row (still selectable, still shown in
	// the detail pane) and looked missing from the list. Applied to every
	// slot, not just the topic, so a location or a tag containing a control
	// character (spec session-search: "a topic, a prompt, a path, or a
	// tag") can never reintroduce the same defect from a different field.
	// The stored text itself is untouched - only this single-line rendering
	// copy is sanitised (task 1.4, 1.5); the detail pane reads the
	// search.Item fields directly and keeps showing them across lines.
	for i, s := range slots {
		slots[i] = sanitizeSingleLine(s)
	}

	return slots
}

// singleLineSeparator visibly marks where a run of control characters was
// removed from text drawn on one line, rather than deleting it outright
// (design.md decision 2): deleting would let text from two different lines
// run together into a misleading phrase (e.g. "fix theauth bug" from "fix
// the\nauth bug"), and would leave a session whose text begins with a line
// break with nothing to distinguish it from a session with no text at all.
const singleLineSeparator = "⏎"

// sanitizeSingleLine replaces every run of line breaks, carriage returns,
// tabs, and other control characters in s with a single singleLineSeparator,
// so s is safe to draw on exactly one terminal line (spec session-search,
// "Session text drawn on one line contains no line breaks"). A run of
// several control characters (e.g. a Windows-style "\r\n") collapses to one
// separator, not one per character. Ordinary content - including the
// double-width and other non-control runes fitRowToWidth's width budget
// already accounts for - passes through unchanged.
func sanitizeSingleLine(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	inControlRun := false
	for _, r := range s {
		if unicode.IsControl(r) {
			if !inControlRun {
				b.WriteString(singleLineSeparator)
				inControlRun = true
			}
			continue
		}
		inControlRun = false
		b.WriteRune(r)
	}
	return b.String()
}

// fitRowToWidth shrinks slots in place until the joined row (single spaces
// between non-empty slots, exactly as RenderRow joins them) fits within
// width, measured in terminal display columns rather than runes (task 3.4:
// a double-width character - e.g. in a topic or a directory name - occupies
// two columns but is one rune, so a rune count would let a row wrap even
// after this budget was supposedly honoured; go-runewidth measures what the
// terminal actually draws).
//
// Cascade, in order: the topic is shortened first, all the way to nothing
// if necessary; then the location, keeping its end rather than its
// beginning (task 4.2/4.3); tags are dropped next. Ordinary content never
// goes further than this. Only when the terminal is narrower than even
// this floor - narrower than the decoration plus the shortest possible
// content (design.md risk note, task 1.3) - do the remaining, more
// identifying fields give way too: age, then end state, then the location
// entirely, then the source, and finally the handle itself is shortened as
// the very last resort, since it is what identifies the row and so is the
// last thing to give ground.
func fitRowToWidth(slots []string, width int) {
	if joinedWidth(slots) <= width {
		return
	}

	if slots[slotTopic] != "" {
		shrinkTopic(slots, width)
	}
	if joinedWidth(slots) <= width {
		return
	}

	shrinkLocation(slots, width)
	if joinedWidth(slots) <= width {
		return
	}

	slots[slotTags] = ""
	if joinedWidth(slots) <= width {
		return
	}

	// Below this point the terminal is narrower than normal content can
	// reach even at its shortest - not the common case this row layout was
	// designed for, but the one-line invariant must still hold at any
	// width (spec session-search, "A rendered row occupies exactly one
	// line including decoration"; design.md decision 2).
	slots[slotAge] = ""
	if joinedWidth(slots) <= width {
		return
	}

	slots[slotState] = ""
	if joinedWidth(slots) <= width {
		return
	}

	slots[slotLocation] = ""
	if joinedWidth(slots) <= width {
		return
	}

	slots[slotSource] = ""
	if joinedWidth(slots) <= width {
		return
	}

	slots[slotHandle] = truncateToWidth(slots[slotHandle], width)
}

func shrinkTopic(slots []string, width int) {
	over := joinedWidth(slots) - width
	inner := unquote(slots[slotTopic])
	innerWidth := runewidth.StringWidth(inner)
	newWidth := innerWidth - over
	if newWidth <= 0 {
		slots[slotTopic] = ""
		return
	}
	slots[slotTopic] = `"` + truncateToWidth(inner, newWidth) + `"`
}

func shrinkLocation(slots []string, width int) {
	over := joinedWidth(slots) - width
	loc := slots[slotLocation]
	locWidth := runewidth.StringWidth(loc)
	newWidth := locWidth - over
	if newWidth < 1 {
		newWidth = 1
	}
	slots[slotLocation] = truncatePathTailToWidth(loc, newWidth)
}

// joinedWidth computes the display width of slots as RenderRow will
// actually join them: non-empty slots separated by exactly one column-wide
// space, empty slots skipped and contributing no separator. Measured in
// terminal columns (go-runewidth), not runes - see fitRowToWidth.
func joinedWidth(slots []string) int {
	n, count := 0, 0
	for _, s := range slots {
		if s == "" {
			continue
		}
		n += runewidth.StringWidth(s)
		count++
	}
	if count > 1 {
		n += count - 1
	}
	return n
}

func unquote(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return s
}

// ellipsisRune replaces elided text everywhere a field is shortened. Its
// own display width (usually 1, but measured rather than assumed - task
// 3.4) is accounted for in the budget so the result never exceeds maxWidth
// columns even when it contains a double-width character.
const ellipsisRune = '…'

// truncateToWidth shortens s to at most maxWidth display columns,
// replacing the tail with a single ellipsis when shortening was needed -
// unlike a three-dot "..." suffix, this keeps the result's own display
// width within budget exactly, which independent per-row width fitting
// depends on. Measured in columns (go-runewidth), not runes, so a
// double-width character is never left straddling the budget.
func truncateToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxWidth {
		return s
	}
	budget := maxWidth - runewidth.RuneWidth(ellipsisRune)
	if budget < 0 {
		// Not even the ellipsis fits the budget: still return it alone
		// rather than nothing, so a field that must survive (the handle,
		// as an absolute last resort) is never rendered empty.
		return string(ellipsisRune)
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteRune(ellipsisRune)
	return b.String()
}

// truncatePathTailToWidth shortens a path to at most maxWidth display
// columns by keeping its end and eliding the front, since a path's
// distinguishing component is its final segment (design.md decision 6 /
// task 4.3), unlike truncateToWidth which keeps the front (used for
// topics, where the beginning is what a person reads first).
func truncatePathTailToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxWidth {
		return s
	}
	budget := maxWidth - runewidth.RuneWidth(ellipsisRune)
	if budget < 0 {
		return string(ellipsisRune)
	}
	r := []rune(s)
	w := 0
	i := len(r)
	for i > 0 {
		rw := runewidth.RuneWidth(r[i-1])
		if w+rw > budget {
			break
		}
		w += rw
		i--
	}
	return string(ellipsisRune) + string(r[i:])
}

// homeDir mirrors internal/profile's own home-directory resolution (HOME
// env var first, else os.UserHomeDir) - duplicated rather than imported to
// keep this rendering package free of a dependency on the profile package
// for one four-line lookup.
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// abbreviateHome shows the portion of path inside the user's home
// directory abbreviated as "~" (spec session-search, "Home directory is
// abbreviated in displayed paths"). A path outside the home directory, or
// when the home directory itself cannot be determined, is returned
// unchanged - this is a display-only transform; nothing given to or
// reported by a command depends on it, so paths given to or reported by
// commands remain usable.
func abbreviateHome(path string) string {
	home := homeDir()
	if home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	prefix := strings.TrimRight(home, "/") + "/"
	if strings.HasPrefix(path, prefix) {
		return "~/" + path[len(prefix):]
	}
	return path
}

// slotColor picks the ANSI code for one row slot, distinguishing each
// field from the others (task 5.2) and, for the end-state slot, sessions
// needing attention from those that do not (task 5.3). This is the single
// built-in colour scheme (design.md non-goal: no theming/configuration).
func slotColor(idx int, it search.Item) string {
	switch idx {
	case slotHandle:
		return ansiBold + ansiCyan
	case slotSource:
		return ansiDim
	case slotLocation:
		if it.DirExists != nil && !*it.DirExists {
			return ansiRed
		}
		return ""
	case slotAge:
		return ansiDim
	case slotState:
		return stateColor(it.EndState)
	case slotTopic:
		// A user-chosen name is shown upright and bold; derived text
		// (topic, last prompt) stays italic, so the two are still
		// distinguishable despite sharing one slot (spec session-search,
		// "Fields are visually distinguishable").
		if it.Name != nil && *it.Name != "" {
			return ansiBold
		}
		return ansiItalic
	case slotTags:
		return ansiMagenta
	default:
		return ""
	}
}

func stateColor(s session.EndState) string {
	switch {
	case s.NeedsAttention():
		return ansiYellow
	case s == session.EndStateCompleted:
		return ansiGreen
	default:
		return ansiDim
	}
}

// sanitizeMultiLine is sanitizeSingleLine's counterpart for the two
// surfaces that legitimately show a paragraph - the browser's comment and
// prompt tabs. Newlines are kept, because they are the structure the reader
// is there to see; every other control character is replaced the same way
// sanitizeSingleLine replaces it, so nothing in a session's text can move
// the cursor, repaint the screen, or break out of the pane it is drawn in.
func sanitizeMultiLine(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	inControlRun := false
	for _, r := range s {
		if r == '\n' {
			b.WriteRune('\n')
			inControlRun = false
			continue
		}
		if unicode.IsControl(r) {
			if !inControlRun {
				b.WriteString(singleLineSeparator)
				inControlRun = true
			}
			continue
		}
		inControlRun = false
		b.WriteRune(r)
	}
	return b.String()
}
