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
	switch rec.Kind {
	case KindUserPrompt, KindAssistantText:
		if rec.Text == nil {
			return Turn{}, false
		}
		text := strings.TrimSpace(*rec.Text)
		if text == "" {
			return Turn{}, false
		}
		truncated := false
		if len(text) > maxBytes {
			text = truncateBytes(text, maxBytes)
			truncated = true
		}
		return Turn{Kind: rec.Kind, Text: text, Truncated: truncated, At: rec.Timestamp}, true

	case KindToolUse:
		return Turn{Kind: rec.Kind, Tool: rec.Tool, At: rec.Timestamp}, true

	case KindCompactionBoundary:
		return Turn{Kind: rec.Kind, At: rec.Timestamp}, true
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
