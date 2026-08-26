package cli

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"lazyrecall/internal/search"
)

// All fixtures below are synthetic, hand-written data. strp is defined in
// pick_test.go, in this same package.

func longItem() search.Item {
	longTopic := strings.Repeat("investigate the flaky retry loop in the scheduler and why it drops tasks under load ", 3)
	cwd := "/Users/example/personal/some/very/deeply/nested/project/directory/that/is/quite/long/indeed"
	return search.Item{
		SessionID: "claude:p:1", Source: "claude", Handle: 42,
		CWD: &cwd, Topic: strp(longTopic), EndState: "dangling",
	}
}

// TestRenderRowStoredTextUnaffectedByDisplay covers task 1.5 and spec
// session-search scenario "Stored text is unaffected": rendering a session
// whose topic contains line breaks must never mutate the text the item
// still carries afterward - only the single-line copy RenderRow returns is
// sanitized, not the source of truth read back from search.Item.
func TestRenderRowStoredTextUnaffectedByDisplay(t *testing.T) {
	original := "fix the\nauth bug\r\nwith\ttabs too"
	topic := original
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 1, EndState: "completed",
		CWD: strp("/Users/example/project"), Topic: &topic}

	for _, width := range []int{10, 40, 120} {
		_ = RenderRow(it, RenderOptions{Width: width, Style: false})
	}

	if *it.Topic != original {
		t.Errorf("stored topic changed after being rendered: got %q, want %q", *it.Topic, original)
	}
	if !strings.Contains(*it.Topic, "\n") || !strings.Contains(*it.Topic, "\r") || !strings.Contains(*it.Topic, "\t") {
		t.Errorf("stored topic lost its control characters after being rendered: %q", *it.Topic)
	}
}

func TestRenderRowFitsWidth(t *testing.T) {
	it := longItem()
	for _, width := range []int{120, 80, 60, 40, 30} {
		line := RenderRow(it, RenderOptions{Width: width, Style: false})
		if got := len([]rune(line)); got > width {
			t.Errorf("width %d: rendered row is %d runes wide: %q", width, got, line)
		}
		if strings.Contains(line, "\n") {
			t.Errorf("width %d: row must never contain a newline: %q", width, line)
		}
	}
}

// TestRenderRowOneLineInvariantAtManyWidths covers fix-row-width-budget
// tasks 3.1-3.3, and fix-row-newlines-and-profile-switch tasks 2.1-2.2: at a
// range of widths - including one narrower than the decoration a caller like
// the browser prepends plus the shortest possible normal content (design.md
// decision 2 / risk note) - a rendered row must never contain a line break
// and must never exceed the given width, measured in the same display
// columns a terminal actually draws (go-runewidth), not in runes. Widths
// below ~25 are narrower than this row's ordinary floor (handle + source +
// location + end state, the fields fitRowToWidth does not touch under
// normal circumstances) and exercise the extended cascade added for task
// 1.3.
//
// This is the third defect of the "budgeted lines drawn != lines expected"
// class in this row renderer (see row.go's rowSlots comment), and every
// prior instance shipped despite this same invariant already being
// asserted, because the fixtures it ran against were clean single-line
// strings. Real session text is routinely NOT clean - agent prompts are
// frequently multi-line - so the fixtures below are deliberately dirty: an
// interior line break, a LEADING line break (the case that rendered an
// entirely blank, apparently-missing row before this change), a bare
// carriage return, a tab, and a Windows-style CRLF pair (two control
// characters that must collapse to one separator, not two). A clean-fixture
// version of this test is worth nothing - it is exactly what let the first
// two defects of this class ship.
func TestRenderRowOneLineInvariantAtManyWidths(t *testing.T) {
	items := []search.Item{
		longItem(),
		{SessionID: "claude:p:2", Source: "claude", Handle: 999999, EndState: "dangling",
			CWD: strp("/Users/example/a/b/c/d/e/project"), Topic: strp("a topic that is somewhat long")},
		{SessionID: "pi:p:3", Source: "pi", Handle: 3, EndState: "completed", Tags: []string{"a", "bcdef"}},
		{SessionID: "claude:p:4", Source: "claude", Handle: 4, EndState: "completed",
			CWD: strp("/Users/example/project"), Topic: strp("fix the\nauth bug and add a regression test")},
		{SessionID: "claude:p:5", Source: "claude", Handle: 5, EndState: "completed",
			CWD: strp("/Users/example/project"), Topic: strp("\nleads with a line break before any visible text")},
		{SessionID: "claude:p:6", Source: "claude", Handle: 6, EndState: "completed",
			CWD: strp("/Users/example/project"), Topic: strp("before\rafter a bare carriage return")},
		{SessionID: "claude:p:7", Source: "claude", Handle: 7, EndState: "completed",
			CWD: strp("/Users/example/project"), Topic: strp("a\ttabbed\tprompt with columns")},
		{SessionID: "claude:p:8", Source: "claude", Handle: 8, EndState: "completed",
			CWD: strp("/Users/example/project"), Topic: strp("windows\r\nstyle CRLF line ending")},
		{SessionID: "claude:p:9", Source: "claude", Handle: 9, EndState: "completed",
			CWD: strp("/Users/example/pro\nject"), Topic: strp("clean topic, dirty path")},
		{SessionID: "claude:p:10", Source: "claude", Handle: 10, EndState: "completed",
			CWD: strp("/Users/example/project"), Tags: []string{"clean", "dir\nty"}},
	}
	widths := []int{1, 2, 3, 5, 8, 10, 15, 20, 25, 30, 40, 60, 80, 120}
	for _, it := range items {
		for _, width := range widths {
			line := RenderRow(it, RenderOptions{Width: width, Style: false})
			if strings.Contains(line, "\n") {
				t.Errorf("item %s width %d: rendered row contains a line break: %q", it.SessionID, width, line)
			}
			if strings.ContainsAny(line, "\r\t") {
				t.Errorf("item %s width %d: rendered row contains a raw carriage return or tab: %q", it.SessionID, width, line)
			}
			if got := runewidth.StringWidth(line); got > width {
				t.Errorf("item %s width %d: rendered row is %d display columns wide: %q", it.SessionID, width, got, line)
			}
			// Every fixture above has visible (non-control) content, so no
			// width that fits at least the handle should ever draw a blank
			// row - the leading-line-break case in particular regressed
			// this before the fix (spec session-search, "Text beginning
			// with a line break").
			if width >= 2 && line == "" {
				t.Errorf("item %s width %d: rendered row is blank even though the session has visible content", it.SessionID, width)
			}
		}
	}
}

// TestRenderRowLeadingLineBreakNotBlank covers spec session-search scenario
// "Text beginning with a line break" directly: a session whose topic begins
// with a line break must still draw its visible content, not an empty row
// that looks like the session is missing from the list while it remains
// selectable and still shows in the detail pane.
func TestRenderRowLeadingLineBreakNotBlank(t *testing.T) {
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 7, EndState: "completed",
		CWD: strp("/Users/example/project"), Topic: strp("\nvisible text after the leading break")}
	for _, width := range []int{20, 40, 80, 120} {
		line := RenderRow(it, RenderOptions{Width: width, Style: false})
		if line == "" {
			t.Fatalf("width %d: row is blank for a session with a leading line break", width)
		}
		// At narrow widths the ordinary shortening cascade (unrelated to
		// this change) may trim the topic's tail or drop it entirely, same
		// as it would for any other long topic - the invariant this test
		// guards is that the row is never blank and never starts with an
		// empty quoted topic, not that the full text survives shortening.
		if strings.HasPrefix(strings.TrimPrefix(line, "  "), "\"\"") {
			t.Errorf("width %d: topic rendered as an empty quoted string: %q", width, line)
		}
	}
	// At a width wide enough for the whole topic, the visible text after
	// the leading break must actually appear, not just "not be blank".
	wide := RenderRow(it, RenderOptions{Width: 120, Style: false})
	if !strings.Contains(wide, "visible text") {
		t.Errorf("width 120: expected the topic's visible content to survive, got %q", wide)
	}
}

// TestSanitizeSingleLineCollapsesControlRuns covers design.md decision 2: a
// run of several control characters (e.g. a Windows "\r\n") collapses to one
// visible separator, not one per character - otherwise text is replaced by
// deletion in disguise, or a single line break reads as two breaks.
func TestSanitizeSingleLineCollapsesControlRuns(t *testing.T) {
	got := sanitizeSingleLine("a\r\nb")
	want := "a" + singleLineSeparator + "b"
	if got != want {
		t.Errorf("sanitizeSingleLine(%q) = %q, want %q (one separator for the whole \\r\\n run)", "a\r\nb", got, want)
	}
	if strings.Count(sanitizeSingleLine("a\n\n\nb"), singleLineSeparator) != 1 {
		t.Errorf("sanitizeSingleLine collapsed a run of three line breaks into more than one separator: %q", sanitizeSingleLine("a\n\n\nb"))
	}
}

// TestSanitizeSingleLineReplacesNotDeletes covers design.md decision 2's
// stated reason for a separator over deletion: deleting the break would run
// "fix the" and "auth bug" together into "fix theauth bug", a different and
// misleading phrase. A visible separator must remain between the two words.
func TestSanitizeSingleLineReplacesNotDeletes(t *testing.T) {
	got := sanitizeSingleLine("fix the\nauth bug")
	if strings.Contains(got, "theauth") {
		t.Errorf("sanitizeSingleLine deleted the line break instead of replacing it, words ran together: %q", got)
	}
	if !strings.Contains(got, "the"+singleLineSeparator+"auth") {
		t.Errorf("expected the separator between the two words, got %q", got)
	}
}

// TestRenderRowKeepsHandleVisibleAtExtremeNarrowWidth covers task 1.3: even
// when the terminal is too narrow for the row's ordinary floor, the row is
// never rendered empty - something identifying the session (starting from
// the handle) survives as long as there is at least one column to draw in.
func TestRenderRowKeepsHandleVisibleAtExtremeNarrowWidth(t *testing.T) {
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 42, EndState: "dangling",
		CWD: strp("/Users/example/a/b/c/project"), Topic: strp("something")}
	for _, width := range []int{1, 2, 3, 4} {
		line := RenderRow(it, RenderOptions{Width: width, Style: false})
		if line == "" {
			t.Errorf("width %d: expected a non-empty row even at extreme narrow width", width)
		}
	}
	// At a width that comfortably fits only the handle, the handle's digits
	// must still be legible (not reduced to just an ellipsis).
	line := RenderRow(it, RenderOptions{Width: 3, Style: false})
	if !strings.Contains(line, "42") && !strings.Contains(line, "4") {
		t.Errorf("expected the handle to remain identifiable at width 3, got %q", line)
	}
}

// TestRenderRowDoubleWidthCharactersRespectInvariant covers task 3.4: a
// topic or location containing double-width (e.g. CJK) characters must
// still leave the rendered row within the given display-column budget. A
// rune-counting budget would under-count these characters (each is one rune
// but two terminal columns) and let the row overflow into a second line -
// this test would fail if fitRowToWidth went back to counting runes instead
// of display width.
func TestRenderRowDoubleWidthCharactersRespectInvariant(t *testing.T) {
	cwd := "/Users/example/short"
	// Each CJK character below is 1 rune but 2 display columns, so a
	// rune-based budget would see this string as "16 wide" while a
	// terminal actually draws it at 32 columns.
	topic := strings.Repeat("配置管理系统日志分析報告書", 3)
	it := search.Item{
		SessionID: "claude:p:1", Source: "claude", Handle: 1, CWD: &cwd,
		Topic: &topic, EndState: "completed",
	}
	for _, width := range []int{20, 30, 40, 50, 60, 80} {
		line := RenderRow(it, RenderOptions{Width: width, Style: false})
		if strings.Contains(line, "\n") {
			t.Errorf("width %d: rendered row contains a line break: %q", width, line)
		}
		if got := runewidth.StringWidth(line); got > width {
			t.Errorf("width %d: rendered row is %d display columns wide (rune count would have said %d): %q",
				width, got, len([]rune(line)), line)
		}
	}
}

func TestRenderRowHandleInlineNoSeparateIdentifierLine(t *testing.T) {
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 7, EndState: "completed"}
	line := RenderRow(it, DefaultRenderOptions)
	if !strings.Contains(line, "#7") {
		t.Errorf("expected the handle inline in the row, got %q", line)
	}
	if strings.Contains(line, "\n") {
		t.Errorf("expected exactly one line, got %q", line)
	}
	// The fully-qualified id must not appear as a separate visible field -
	// only the short handle is shown inline (task 4.1).
	if strings.Contains(line, it.SessionID) {
		t.Errorf("expected no separate identifier line/field, got %q", line)
	}
}

// TestRenderRowShrinksTopicBeforeLocation covers task 4.2: when a row does
// not fit, the topic is shortened first - the location is only touched if
// shrinking the topic to nothing still isn't enough.
func TestRenderRowShrinksTopicBeforeLocation(t *testing.T) {
	cwd := "/Users/example/short"
	it := search.Item{
		SessionID: "claude:p:1", Source: "claude", Handle: 1, CWD: &cwd,
		Topic: strp(strings.Repeat("word ", 40)), EndState: "completed",
	}
	// A width that comfortably fits the location but not the full topic.
	line := RenderRow(it, RenderOptions{Width: 60, Style: false})
	if !strings.Contains(line, abbreviateHome(cwd)) {
		t.Errorf("expected the (short) location to survive in full while the topic shrinks, got %q", line)
	}
}

// TestRenderRowGivesPathEndPriority covers task 4.3: when the location
// itself must be shortened, the end of the path (its distinguishing final
// component) is kept, not the beginning.
func TestRenderRowGivesPathEndPriority(t *testing.T) {
	cwd := "/Users/example/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/distinguishing-project-name"
	it := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 1, CWD: &cwd, EndState: "completed"}
	line := RenderRow(it, RenderOptions{Width: 40, Style: false})
	// At this width there isn't room for the whole final segment, but
	// whatever survives must be the *end* of the path, not its beginning.
	if !strings.Contains(line, "project-name") {
		t.Errorf("expected the tail of the path's final segment to survive truncation, got %q", line)
	}
	if strings.Contains(line, "/Users/example/a/b") {
		t.Errorf("expected the path's beginning to be elided first, got %q", line)
	}

	// At a width generous enough to hold the whole final segment, it must
	// appear in full.
	roomy := RenderRow(it, RenderOptions{Width: 70, Style: false})
	if !strings.Contains(roomy, "distinguishing-project-name") {
		t.Errorf("expected the full final path segment at a generous width, got %q", roomy)
	}
}

func TestAbbreviateHomeInsideHome(t *testing.T) {
	t.Setenv("HOME", "/Users/example")
	got := abbreviateHome("/Users/example/personal/aim")
	if got != "~/personal/aim" {
		t.Errorf("got %q", got)
	}
}

func TestAbbreviateHomeOutsideHome(t *testing.T) {
	t.Setenv("HOME", "/Users/example")
	got := abbreviateHome("/opt/other/place")
	if got != "/opt/other/place" {
		t.Errorf("expected an unrelated path to be unchanged, got %q", got)
	}
}

func TestAbbreviateHomeDoesNotMatchOnPrefixCollision(t *testing.T) {
	// /Users/example is a *prefix* of /Users/example2/foo but not a parent
	// directory of it - must not be abbreviated.
	t.Setenv("HOME", "/Users/example")
	got := abbreviateHome("/Users/example2/foo")
	if got != "/Users/example2/foo" {
		t.Errorf("expected no false-positive abbreviation, got %q", got)
	}
}

// TestRenderRowStylingDistinguishesEveryField covers task 5.2: handle,
// location, age, end state, and topic must each carry their own styling
// treatment so a terminal listing can visually distinguish them.
func TestRenderRowStylingDistinguishesEveryField(t *testing.T) {
	cwd := "/missing/dir"
	dirExists := false
	at := time.Now().Add(-2 * time.Hour)
	it := search.Item{
		SessionID: "claude:p:1", Source: "claude", Handle: 1, CWD: &cwd, DirExists: &dirExists,
		LastActivityAt: &at, EndState: "completed", Topic: strp("a topic"),
	}
	line := RenderRow(it, RenderOptions{Width: 120, Style: true})
	for _, code := range []string{ansiBold + ansiCyan, ansiDim, ansiItalic, ansiGreen, ansiRed} {
		if !strings.Contains(line, code) {
			t.Errorf("expected styling code %q to appear (handle/source-or-age/topic/state/missing-location distinguishers), got %q", code, line)
		}
	}
}

func TestRenderRowStylingDistinguishesAttentionState(t *testing.T) {
	needsAttention := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 1, EndState: "dangling"}
	completed := search.Item{SessionID: "claude:p:2", Source: "claude", Handle: 2, EndState: "completed"}

	a := RenderRow(needsAttention, RenderOptions{Width: 80, Style: true})
	c := RenderRow(completed, RenderOptions{Width: 80, Style: true})
	if !strings.Contains(a, ansiYellow) {
		t.Errorf("expected a needs-attention state to be styled distinctly, got %q", a)
	}
	if !strings.Contains(c, ansiGreen) {
		t.Errorf("expected a completed state to be styled distinctly, got %q", c)
	}
}

var ansiCodeRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiCodeRe.ReplaceAllString(s, "")
}

// TestStyledOutputStrippedEqualsPlainOutput covers task 6.2: piped (plain)
// output must be byte-identical to styled output with the ANSI codes
// removed - styling must never change what text is shown, only how.
func TestStyledOutputStrippedEqualsPlainOutput(t *testing.T) {
	items := []search.Item{
		longItem(),
		{SessionID: "pi:p:2", Source: "pi", Handle: 2, EndState: "unknown", Tags: []string{"a", "b"}},
		{SessionID: "omp:p:3", Source: "omp", Handle: 3, EndState: "abandoned"},
	}
	for _, it := range items {
		styled := RenderRow(it, RenderOptions{Width: 80, Style: true})
		plain := RenderRow(it, RenderOptions{Width: 80, Style: false})
		if stripANSI(styled) != plain {
			t.Errorf("styled-then-stripped != plain:\nstripped: %q\nplain:    %q", stripANSI(styled), plain)
		}
	}
}

func TestRenderRowNoStylingWhenDisabled(t *testing.T) {
	it := longItem()
	line := RenderRow(it, RenderOptions{Width: 80, Style: false})
	if strings.Contains(line, "\x1b[") {
		t.Errorf("expected no ANSI escape codes when Style is false, got %q", line)
	}
}

func TestDetermineOptionsRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	opts := DetermineOptions(discardWriter{})
	if opts.Style {
		t.Error("expected NO_COLOR to suppress styling even if the writer were a terminal")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestDetermineOptionsNonFileWriterNeverStyled(t *testing.T) {
	opts := DetermineOptions(discardWriter{})
	if opts.Style {
		t.Error("expected a non-*os.File writer to never be styled")
	}
}

func TestDetermineOptionsUsesColumnsEnv(t *testing.T) {
	t.Setenv("COLUMNS", "50")
	opts := DetermineOptions(discardWriter{})
	if opts.Width != 50 {
		t.Errorf("got width %d, want 50", opts.Width)
	}
}

func TestDetermineOptionsFallsBackToDefaultWidth(t *testing.T) {
	t.Setenv("COLUMNS", "")
	opts := DetermineOptions(discardWriter{})
	if opts.Width != DefaultWidth {
		t.Errorf("got width %d, want default %d", opts.Width, DefaultWidth)
	}
}

// TestRenderRowPrefersNameOverTopic covers the display half of change
// show-session-names: when the user named the session inside the source
// tool, that name is what the row's text slot shows - the agent-written
// topic and the last prompt are fallbacks, in that order, for sessions
// that were never named.
func TestRenderRowPrefersNameOverTopic(t *testing.T) {
	base := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 7,
		CWD: strp("/Users/example/project"), EndState: "completed"}

	named := base
	named.Name = strp("retry-loop")
	named.Topic = strp("Flaky retry loop investigation")
	named.LastPrompt = strp("why does the loop drop tasks")
	line := RenderRow(named, RenderOptions{Width: 200, Style: false})
	if !strings.Contains(line, `"retry-loop"`) {
		t.Errorf("row should show the session name: %q", line)
	}
	if strings.Contains(line, "Flaky retry loop investigation") {
		t.Errorf("row should not also show the topic once a name is set: %q", line)
	}

	unnamed := base
	unnamed.Topic = strp("Flaky retry loop investigation")
	unnamed.LastPrompt = strp("why does the loop drop tasks")
	line = RenderRow(unnamed, RenderOptions{Width: 200, Style: false})
	if !strings.Contains(line, `"Flaky retry loop investigation"`) {
		t.Errorf("row should fall back to the topic when no name is set: %q", line)
	}

	// An empty (rather than absent) name is not a name: fall back too.
	blank := base
	blank.Name = strp("")
	blank.Topic = strp("Flaky retry loop investigation")
	line = RenderRow(blank, RenderOptions{Width: 200, Style: false})
	if !strings.Contains(line, `"Flaky retry loop investigation"`) {
		t.Errorf("an empty name should fall back to the topic: %q", line)
	}
}

// TestRenderRowNameStyledDistinctlyFromTopic: name and topic share one
// slot, so styling is what keeps them distinguishable (spec
// session-search, "Fields are visually distinguishable").
func TestRenderRowNameStyledDistinctlyFromTopic(t *testing.T) {
	base := search.Item{SessionID: "claude:p:1", Source: "claude", Handle: 7,
		CWD: strp("/Users/example/project"), EndState: "completed"}

	named := base
	named.Name = strp("retry-loop")
	unnamed := base
	unnamed.Topic = strp("retry-loop")

	nameLine := RenderRow(named, RenderOptions{Width: 200, Style: true})
	topicLine := RenderRow(unnamed, RenderOptions{Width: 200, Style: true})
	if !strings.Contains(nameLine, ansiBold+`"retry-loop"`) {
		t.Errorf("a session name should render bold: %q", nameLine)
	}
	if !strings.Contains(topicLine, ansiItalic+`"retry-loop"`) {
		t.Errorf("a topic should stay italic: %q", topicLine)
	}
}
