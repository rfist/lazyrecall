// Styling is emitted directly, by hand, as raw ANSI escape sequences - no
// third-party colour library (constraint 1; design.md decision 5: "a
// styling library would be the project's first third-party dependency, for
// something that is a handful of constants").
package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
	ansiWhite   = "\x1b[37m"
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

	// InstallLabels maps an install name (search.Item.Install) to the
	// display label its row badge should show instead of the bare source -
	// "[claude]" becomes "[ccp]" (change group-sessions-in-one-index). Built
	// once per load by InstallLabels, never per row; nil is a valid "no
	// labels known" value and every source falls back to its own name, the
	// pre-groups behaviour.
	InstallLabels map[string]string

	// GroupColors maps a configured group's name (search.Item.Group) to the
	// raw ANSI escape sequence its sessions' handles - and, in the browser,
	// its own name in the Groups panel - are drawn in. Built once per load
	// by GroupColors from config.Config.Groups, never re-parsed per row, the
	// same discipline InstallLabels follows. nil (or a group with no entry)
	// is the "no color configured" case and leaves the handle in its
	// default style.
	GroupColors map[string]string
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

// groupColorNames maps a configured group color's normalized form
// (internal/config.Group.Color, already validated and lowercased by
// config.Load) to its ANSI SGR foreground parameter. Bright variants use
// the dedicated high-intensity codes (90-97) rather than "bold + base
// color" (1;3x), so a bright group color composes with a field that is
// separately bold (the handle, in slotColor) without either one clobbering
// the other's bold bit.
var groupColorNames = map[string]string{
	"black": "30", "red": "31", "green": "32", "yellow": "33",
	"blue": "34", "magenta": "35", "cyan": "36", "white": "37",
	"bright-black": "90", "bright-red": "91", "bright-green": "92", "bright-yellow": "93",
	"bright-blue": "94", "bright-magenta": "95", "bright-cyan": "96", "bright-white": "97",
}

// groupAnsiCode converts a config.Group.Color value into the raw escape
// sequence style() expects - the one place in the program that turns a
// group's configured color into terminal control bytes, mirroring how
// slotColor is the one place a row's other colors are chosen. color has
// already been validated and normalized by internal/config.Load (a name or
// a lowercased "#rrggbb" hex triplet), so this never has anything to
// reject; an unrecognized value falls back to "", the "no color" case,
// rather than panicking on state that should be unreachable.
func groupAnsiCode(color string) string {
	if color == "" {
		return ""
	}
	if code, ok := groupColorNames[color]; ok {
		return "\x1b[" + code + "m"
	}
	if r, g, b, ok := parseHexColor(color); ok {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	}
	return ""
}

// parseHexColor reads a "#rrggbb" triplet into its three byte components.
func parseHexColor(s string) (r, g, b int, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseInt(strings.ToLower(s[1:]), 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
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
