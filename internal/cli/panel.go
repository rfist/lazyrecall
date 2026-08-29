// Panel chrome for the browser: a bordered box with its jump number and a
// count in the top border, and the helpers that stack boxes into columns
// (change lazy-style-browser). Colour and the uneven-height column join
// come from lipgloss, which is already compiled in via bubbletea - the
// dependency policy in openspec/config.yaml forbids external applications
// the user has to install, not Go modules, and joining two columns whose
// panels do not line up is exactly the fiddly, easy-to-get-wrong work a
// library should be doing.
//
// Row *content* is still rendered by RenderRow's own hand-emitted ANSI
// (style.go): that renderer is shared with plain non-terminal output and
// has to keep producing escape sequences that are correct outside any
// bubbletea program. lipgloss here is for chrome only.
package cli

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Border colours: the focused panel is the only bright thing on screen, so
// "where am I" is answerable at a glance without reading any text.
var (
	focusedBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))   // cyan
	blurredBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // grey
	panelTitle    = lipgloss.NewStyle().Bold(true)
	panelCount    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// panelBox describes one bordered panel. Width and Height are the box's
// outer dimensions, borders included, so a caller that has budgeted screen
// space does not have to remember to subtract two from each.
type panelBox struct {
	// Number is the digit that jumps to this panel, drawn only when
	// HasNumber is set. A separate bool, rather than Number's own zero
	// value, is what lets Sessions' jump key be 0 without that reading as
	// "no number" - the same ambiguity panelID.jumpKey() (browse.go) exists
	// to avoid, carried through to where the digit is actually drawn.
	Number    int
	HasNumber bool
	Title     string
	Count     string   // right-hand annotation in the top border; "" for none
	Lines     []string // already-rendered, already-shortened content lines
	Width     int
	Height    int
	Focused   bool
	Style     bool // false = NO_COLOR / not a terminal: emit no escapes at all
}

// innerWidth is the space a panel's content actually gets: its outer width
// less the two border columns. Callers must shorten their lines to this
// before handing them over - renderPanel truncates as a backstop, but a
// styled line can only be shortened correctly by whatever built it, before
// the escape sequences went in.
func (p panelBox) innerWidth() int {
	w := p.Width - 2
	if w < 1 {
		w = 1
	}
	return w
}

// innerHeight is the number of content lines a panel can show.
func (p panelBox) innerHeight() int {
	h := p.Height - 2
	if h < 0 {
		h = 0
	}
	return h
}

// render draws the panel as Height lines of exactly Width columns.
//
// The box is drawn by hand rather than with lipgloss.Border because the
// title and the count live *inside* the top border, which lipgloss v1 has
// no way to express; lipgloss still supplies the colour, so a panel border
// and a lipgloss-joined column agree about what "dim" means.
func (p panelBox) render() string {
	inner := p.innerWidth()
	border := blurredBorder
	if p.Focused {
		border = focusedBorder
	}
	paint := func(s string) string {
		if !p.Style {
			return s
		}
		return border.Render(s)
	}

	var b strings.Builder

	// Top border: ╭─1 Title ────────── 290 ─╮
	title := p.Title
	if p.HasNumber {
		title = itoa(p.Number) + " " + title
	}
	// The title can carry styling of its own - the detail pane's tab strip
	// is a title - so both ends of the border are measured by visible
	// width. Counting escape bytes as columns here is what makes a border
	// stop short of the panel it is supposed to close.
	left := "─" + title + " "
	right := ""
	if p.Count != "" {
		right = " " + p.Count + " "
	}
	fill := inner - visibleWidth(left) - visibleWidth(right)
	if fill < 0 {
		// Too narrow for both: keep the title, drop the count.
		right = ""
		left = truncateVisible(left, inner)
		fill = inner - visibleWidth(left)
		title = truncateVisible(title, maxInt(inner-2, 1))
	}
	if fill < 0 {
		fill = 0
	}
	b.WriteString(paint("╭"))
	b.WriteString(paint("─"))
	if p.Style {
		b.WriteString(panelTitle.Render(title))
	} else {
		b.WriteString(title)
	}
	b.WriteString(paint(" "))
	b.WriteString(paint(strings.Repeat("─", fill)))
	if right != "" {
		if p.Style {
			b.WriteString(paint(" ") + panelCount.Render(p.Count) + paint(" "))
		} else {
			b.WriteString(right)
		}
	}
	b.WriteString(paint("╮"))
	b.WriteString("\n")

	// Content, padded or truncated to exactly innerHeight lines.
	for i := 0; i < p.innerHeight(); i++ {
		line := ""
		if i < len(p.Lines) {
			line = p.Lines[i]
		}
		b.WriteString(paint("│"))
		b.WriteString(padToWidth(line, inner))
		b.WriteString(paint("│"))
		b.WriteString("\n")
	}

	// Bottom border.
	b.WriteString(paint("╰" + strings.Repeat("─", inner) + "╯"))
	return b.String()
}

// padToWidth right-pads s with spaces to exactly w display columns,
// truncating instead when it is already wider. Escape sequences occupy no
// columns, so the visible width is what is measured - a styled line and a
// plain one of the same text pad identically.
func padToWidth(s string, w int) string {
	vis := visibleWidth(s)
	if vis > w {
		return truncateVisible(s, w)
	}
	return s + strings.Repeat(" ", w-vis)
}

// truncateVisible shortens s to at most w visible columns, keeping every
// escape sequence it passes over so styling is never cut in half, and
// closing with a reset when it stopped inside styled text. truncateToWidth
// (row.go) is the plain-text counterpart: it is applied to a field's text
// *before* the escapes go in, which is the right order for a row and the
// wrong one for a line that arrives already styled.
func truncateVisible(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if !strings.Contains(s, "\x1b") {
		return truncateToWidth(s, w)
	}
	var b strings.Builder
	width, styled, inEscape := 0, false, false
	for _, r := range s {
		switch {
		case inEscape:
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == '\x1b':
			b.WriteRune(r)
			inEscape, styled = true, true
		default:
			rw := runewidth.RuneWidth(r)
			if width+rw > w {
				if styled {
					b.WriteString(ansiReset)
				}
				return b.String()
			}
			b.WriteRune(r)
			width += rw
		}
	}
	return b.String()
}

// visibleWidth is the display width of s ignoring ANSI escape sequences.
func visibleWidth(s string) int {
	if !strings.Contains(s, "\x1b") {
		return runewidth.StringWidth(s)
	}
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == '\x1b':
			inEscape = true
		default:
			b.WriteRune(r)
		}
	}
	return runewidth.StringWidth(b.String())
}

// stackPanels renders panels one above another into a single column of
// exactly width columns.
func stackPanels(boxes ...panelBox) string {
	parts := make([]string, 0, len(boxes))
	for _, b := range boxes {
		if b.Height <= 0 {
			continue
		}
		parts = append(parts, b.render())
	}
	return strings.Join(parts, "\n")
}

// joinColumns places rendered blocks side by side, top-aligned, padding the
// shorter one so the taller column's lines are never pulled leftwards.
func joinColumns(left, right string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// dropVisible returns s with its first n visible columns removed, carrying
// forward any styling that was in effect at the cut so the remainder is
// drawn the colour it was written in. It is truncateVisible's other half,
// and the two together are what let a popup be spliced into the middle of
// an already-styled line.
func dropVisible(s string, n int) string {
	if n <= 0 {
		return s
	}
	var (
		active   strings.Builder // escapes seen before the cut
		escape   strings.Builder
		width    int
		inEscape bool
	)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inEscape:
			escape.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
				if escape.String() == ansiReset {
					active.Reset()
				} else {
					active.WriteString(escape.String())
				}
				escape.Reset()
			}
		case r == '\x1b':
			escape.Reset()
			escape.WriteRune(r)
			inEscape = true
		default:
			width += runewidth.RuneWidth(r)
			if width > n {
				return active.String() + string(runes[i:])
			}
		}
	}
	return ""
}

// overlay splices box's lines into the middle of base, so a popup covers
// the frame it is drawn over instead of pushing it around. Both are already
// styled, which is why the splice is done by visible column.
func overlay(base, box string) string {
	baseLines := strings.Split(base, "\n")
	boxLines := strings.Split(box, "\n")
	if len(boxLines) == 0 || len(baseLines) == 0 {
		return base
	}
	boxWidth := 0
	for _, l := range boxLines {
		if w := visibleWidth(l); w > boxWidth {
			boxWidth = w
		}
	}
	baseWidth := 0
	for _, l := range baseLines {
		if w := visibleWidth(l); w > baseWidth {
			baseWidth = w
		}
	}
	left := (baseWidth - boxWidth) / 2
	if left < 0 {
		left = 0
	}
	top := (len(baseLines) - len(boxLines)) / 2
	if top < 0 {
		top = 0
	}
	for i, bl := range boxLines {
		y := top + i
		if y >= len(baseLines) {
			break
		}
		row := baseLines[y]
		head := padToWidth(truncateVisible(row, left), left)
		tail := dropVisible(row, left+visibleWidth(bl))
		baseLines[y] = head + bl + ansiReset + tail
	}
	return strings.Join(baseLines, "\n")
}
