// Styling is emitted directly, by hand, as raw ANSI escape sequences - no
// third-party colour library (constraint 1; design.md decision 5: "a
// styling library would be the project's first third-party dependency, for
// something that is a handful of constants").
package cli

import (
	"io"
	"os"
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiItalic  = "\x1b[3m"
	ansiReverse = "\x1b[7m"
	ansiCyan    = "\x1b[36m"
	ansiYellow  = "\x1b[33m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiMagenta = "\x1b[35m"
)

// style wraps s in code when enabled is true, and returns s unchanged
// otherwise - the one place every styled field in a row passes through, so
// "no styling at all" (spec session-search, "Output is redirected" /
// "Styling disabled by the environment") is a single condition to get
// right, not one per field.
func style(s, code string, enabled bool) string {
	if !enabled || s == "" || code == "" {
		return s
	}
	return code + s + ansiReset
}

// RenderOptions controls how a row is rendered: the width it must fit
// within (spec session-search, "A listed session occupies one line that
// fits the terminal") and whether ANSI styling is emitted (spec, "Fields
// are visually distinguishable").
type RenderOptions struct {
	Width int
	Style bool
}

// DefaultWidth is used when no output width can be determined at all (not a
// terminal, COLUMNS unset or invalid).
const DefaultWidth = 80

// DefaultRenderOptions is the unstyled default: DefaultWidth, no ANSI
// styling. Used wherever a row is being built for something other than
// direct terminal display.
var DefaultRenderOptions = RenderOptions{Width: DefaultWidth, Style: false}

// DetermineOptions computes the render options for writing to w: the
// terminal's actual column count when it can be determined (COLUMNS env var
// first, then a TIOCGWINSZ query against w when w is a live *os.File),
// falling back to DefaultWidth; and styling enabled only when w is a
// terminal and the environment does not request colour be suppressed (spec
// session-search, "Styling disabled by the environment" - the well-known
// NO_COLOR convention, https://no-color.org).
func DetermineOptions(w io.Writer) RenderOptions {
	width := DefaultWidth
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, ok := parsePositiveInt(v); ok {
			width = n
		}
	} else if f, ok := w.(*os.File); ok {
		if cols, ok := terminalWidth(f); ok {
			width = cols
		}
	}

	styleOn := false
	if os.Getenv("NO_COLOR") == "" {
		if f, ok := w.(*os.File); ok && isTerminal(f) {
			styleOn = true
		}
	}
	return RenderOptions{Width: width, Style: styleOn}
}

// IsTerminal reports whether f is a live terminal - the browser's
// precondition (spec session-search, "Output is not a terminal": browsing
// needs something to draw on, so non-terminal output falls back to the
// non-interactive listing).
func IsTerminal(f *os.File) bool {
	_, ok := terminalWidth(f)
	return ok
}

// isTerminal reports whether f is a live terminal.
func isTerminal(f *os.File) bool {
	_, ok := terminalWidth(f)
	return ok
}

func parsePositiveInt(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}
