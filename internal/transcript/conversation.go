package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// Turn is one displayable exchange from a transcript: the reading view of
// the same records Scan folds into an index. Scan exists to answer
// questions *about* a session (what was asked, how it ended) in bounded
// memory and never keeps the conversation itself; Conversation exists to
// show the conversation, and so keeps text Scan discards - but through the
// same Vocab, so the two can never disagree about what a record is
// (design.md decision 3: adapters enumerate, one shared reader parses).
type Turn struct {
	// Kind is one of KindUserPrompt, KindAssistantText, KindToolUse or
	// KindCompactionBoundary. The reader drops every other kind: session
	// meta, topics and titles are already on the Detail tab, and tool
	// results carry no text in any source's vocabulary, so a line for one
	// would say only that something came back.
	Kind Kind
	// Text is the turn's content, already trimmed of surrounding
	// whitespace and truncated to the caller's per-turn budget. Empty for
	// a tool call and for a compaction boundary.
	Text string
	// Truncated reports that Text is the head of a longer turn.
	Truncated bool
	// Tool names the tools a KindToolUse turn invoked, when the source
	// records them (see Record.Tool). Empty means the call happened but
	// went unnamed, never that there was no call.
	Tool []string
	At   *time.Time
}

// ConversationLimits bounds one Conversation read. Both fields must be
// positive; DefaultConversationLimits supplies the values the browser uses.
type ConversationLimits struct {
	// MaxTurns is how many of the most recent turns to keep. The tail is
	// kept rather than the head deliberately: what the head of a session
	// was about is already answered by the Detail and Prompts tabs, while
	// the question this view exists to answer - "where did I leave off,
	// and do I want to resume it" - is only answerable from the end.
	MaxTurns int
	// MaxTurnBytes caps a single turn's text. A pasted file or a long
	// tool-shaped reply must not be able to push the rest of the
	// conversation out of the pane.
	MaxTurnBytes int
}

// DefaultConversationLimits is what the browser reads with: enough turns to
// scroll back through a working session, and a per-turn cap that keeps a
// pasted file from filling the pane.
var DefaultConversationLimits = ConversationLimits{MaxTurns: 400, MaxTurnBytes: 4000}

// Conversation reads the whole transcript at path and returns its
// displayable turns in file order, keeping at most limits.MaxTurns of them.
// dropped is how many earlier turns were discarded to stay within that
// budget, so a caller can say so rather than silently presenting a partial
// conversation as a whole one.
//
// Like Scan, a line that fails to parse as JSON or whose record the vocab
// does not recognize is skipped without aborting the read: a transcript is
// allowed to contain records this program has never seen.
func Conversation(path string, vocab Vocab, limits ConversationLimits) (turns []Turn, dropped int, err error) {
	if limits.MaxTurns <= 0 {
		limits.MaxTurns = DefaultConversationLimits.MaxTurns
	}
	if limits.MaxTurnBytes <= 0 {
		limits.MaxTurnBytes = DefaultConversationLimits.MaxTurnBytes
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// The window grows to twice the budget and is then halved, rather than
	// shifting by one on every turn past the budget: a transcript with
	// tens of thousands of turns would otherwise spend quadratic time
	// copying to keep a few hundred.
	kept := make([]Turn, 0, limits.MaxTurns)
	for sc.Scan() {
		var raw map[string]any
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			continue
		}
		rec, ok := vocab.Classify(raw)
		if !ok {
			continue
		}
		turn, ok := toTurn(rec, limits.MaxTurnBytes)
		if !ok {
			continue
		}
		if len(kept) == 2*limits.MaxTurns {
			n := copy(kept, kept[limits.MaxTurns:])
			kept = kept[:n]
			dropped += limits.MaxTurns
		}
		kept = append(kept, turn)
	}
	// sc.Err() is deliberately not returned. The error a scan of a
	// transcript actually hits is a line longer than the 4MB buffer, which
	// stops the read early - a truncated conversation, not a failed one,
	// and the turns already collected are exactly what the reader wants to
	// see. Returning an error here would replace a mostly-complete
	// conversation with a message about a buffer size.
	return trimToBudget(kept, limits.MaxTurns, &dropped), dropped, nil
}

// LimitTurns applies a Conversation budget to turns a caller assembled
// itself, and is how the database-backed sources (goose, hermes, opencode)
// reach the same result Conversation produces for a transcript file: they
// know how to get their rows, this decides what an empty turn is, how long
// a turn may be, and how many to keep. Without it each of them would answer
// those three questions slightly differently and the same session would
// read differently depending on which agent wrote it.
func LimitTurns(turns []Turn, limits ConversationLimits) ([]Turn, int, error) {
	if limits.MaxTurns <= 0 {
		limits.MaxTurns = DefaultConversationLimits.MaxTurns
	}
	if limits.MaxTurnBytes <= 0 {
		limits.MaxTurnBytes = DefaultConversationLimits.MaxTurnBytes
	}
	kept := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if n, ok := normalizeTurn(t, limits.MaxTurnBytes); ok {
			kept = append(kept, n)
		}
	}
	dropped := 0
	return trimToBudget(kept, limits.MaxTurns, &dropped), dropped, nil
}

func trimToBudget(turns []Turn, max int, dropped *int) []Turn {
	if len(turns) <= max {
		return turns
	}
	*dropped += len(turns) - max
	return turns[len(turns)-max:]
}

// toTurn projects one normalized record into a displayable turn, reporting
// false for the records this view does not show at all and for the ones
// that turn out to carry nothing to show (a thinking-only assistant
// fragment classifies as KindAssistantText with no text).
func toTurn(rec Record, maxBytes int) (Turn, bool) {
	text := ""
	if rec.Text != nil {
		text = *rec.Text
	}
	return normalizeTurn(Turn{Kind: rec.Kind, Text: text, Tool: rec.Tool, At: rec.Timestamp}, maxBytes)
}

// normalizeTurn is the single decision about what a displayable turn is,
// shared by the transcript reader and by the database-backed adapters that
// build Turns directly. It reports false for the kinds this view does not
// show and for the ones that carry nothing to show - a thinking-only
// assistant fragment, or a message whose whole text was whitespace.
func normalizeTurn(t Turn, maxBytes int) (Turn, bool) {
	switch t.Kind {
	case KindUserPrompt, KindAssistantText:
		text := strings.TrimSpace(t.Text)
		if text == "" {
			return Turn{}, false
		}
		// A harness-injected block is not something a person said, and
		// showing one as a "you" turn misreports the conversation. The
		// Claude vocab already drops these at classification time; the
		// database-backed sources build Turns directly, and OpenCode and
		// Kilo were observed storing "<system-reminder>" blocks as user
		// messages exactly the way Claude Code writes them - so the check
		// belongs here, where every path passes through it, rather than
		// being repeated per adapter.
		if t.Kind == KindUserPrompt && isInjectedBlock(text) {
			return Turn{}, false
		}
		if len(text) > maxBytes {
			text = truncateBytes(text, maxBytes)
			t.Truncated = true
		}
		t.Text = text
		return t, true

	case KindToolUse, KindCompactionBoundary:
		t.Text = ""
		return t, true
	}
	return Turn{}, false
}

// truncateBytes cuts s to at most max bytes without splitting a rune.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b begins a UTF-8 encoded rune (i.e. is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
