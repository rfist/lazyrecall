// Package transcript is the one shared component that reads line-delimited
// session transcripts and turns per-source record shapes into a small,
// normalized event vocabulary (design.md decision 3: "Adapters enumerate;
// one shared reader parses transcripts"). Adapters never parse transcripts
// themselves; they call Scan and project the result into a session.Session.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"time"

	"lazyrecall/internal/session"
)

// Kind is the normalized vocabulary every source's records are mapped into
// (task 4.2 - the "small mapping table" is the per-source Vocab
// implementations in claude.go / pi.go / omp.go).
type Kind int

const (
	KindUnknown Kind = iota
	// KindSessionMeta carries the session's working directory and, when
	// present, its git branch. Read from record content only - never from
	// a source's on-disk directory encoding (task 4.4).
	KindSessionMeta
	// KindUserPrompt is text the user actually typed - not a tool result
	// injected under a "user" role.
	KindUserPrompt
	// KindAssistantText is the agent's textual reply.
	KindAssistantText
	// KindToolUse is the agent invoking a tool.
	KindToolUse
	// KindToolResult is the result of a tool call being recorded.
	KindToolResult
	// KindTopic is an agent-written title/topic for the session.
	KindTopic
	// KindLastPrompt is a trailer some sources write summarizing the most
	// recent prompt.
	KindLastPrompt
	// KindCompactionBoundary marks a compaction event.
	KindCompactionBoundary
	// KindCustomTitle is a name the *user* gave the session inside the
	// source tool (Claude Code's session rename). Distinct from KindTopic,
	// which the agent writes for itself: a name is chosen, a topic is
	// derived, and a chosen name is never overwritten by a derived one.
	KindCustomTitle
)

// Record is one normalized event extracted from a raw source record.
type Record struct {
	Kind       Kind
	Timestamp  *time.Time
	CWD        *string
	GitBranch  *string
	Text       *string // prompt text, topic text, or assistant text, depending on Kind
	StopReason *string // assistant stop/finish reason, when the source records one
	Compaction *CompactionInfo

	// Origin is who drove the session this record belongs to, when the
	// source records it on its records (Claude Code's "entrypoint" field).
	// The zero value (OriginUnknown) is what sources that record no such
	// field produce.
	Origin session.Origin

	// Client is the program that drove the session, verbatim as the source
	// named it (Claude Code's "entrypoint": "cli" for its own terminal,
	// "sdk-ts" for a program embedding the TypeScript SDK - an editor
	// client such as CodeCompanion's ACP adapter - "sdk-cli" for a script
	// driving the CLI). Stored raw rather than as a friendly label so the
	// index never records an inference: the short name a listing shows is
	// derived at display time (change show-editor-clients).
	Client *string

	// HumanTyped marks a record the source itself attributes to a person
	// rather than to its own machinery (Claude Code's origin.kind "human").
	// It is the one signal that outranks Client/Origin: a session reached
	// through an SDK entrypoint is still a conversation when a person is
	// the one typing into it.
	HumanTyped bool

	// SourceID is the session's own identifier, as the source itself
	// recorded it inside the transcript - never derived from the
	// transcript's file name (change fix-resume-session-identity, design.md
	// decision 1: "a storage detail is not an identity", the same rule
	// already applied to working directories). Only pi and omp's "session"
	// records carry this today; nil for every other record.
	SourceID *string
}

// CompactionInfo is the compaction detail extracted from one
// KindCompactionBoundary record, when the source recorded it (task 4.6).
type CompactionInfo struct {
	Automatic     *bool
	DroppedTokens *int64
	PreTokens     *int64
}

// Vocab maps one source's raw JSONL record shape into the normalized
// Record vocabulary above. Implementations skip (return ok=false) any
// record they don't recognize rather than erroring - transcripts are
// tolerated to contain record types this program doesn't know about (spec
// session-index, "Unrecognized or malformed record").
type Vocab interface {
	Classify(raw map[string]any) (Record, bool)
}

// VocabFor returns the Vocab for a source name ("claude", "pi", "omp").
// hermes has no transcripts and does not use this package.
func VocabFor(source string) (Vocab, bool) {
	switch source {
	case "claude":
		return claudeVocab{}, true
	case "pi":
		return piVocab{}, true
	case "omp":
		return ompVocab{}, true
	default:
		return nil, false
	}
}

// tailWindow bounds the number of recent normalized records Scan keeps in
// memory for end-state classification, regardless of how many lines it
// reads (task 4.3: bounded reads).
const tailWindow = 12

// Result is what one Scan pass over a transcript (or a byte range of one)
// produces.
type Result struct {
	// CWD / GitBranch are set from the first record in this scan that
	// carried them. Callers merge this with any value already known from a
	// prior scan (session meta typically appears once, near the start).
	CWD       *string
	GitBranch *string

	// SourceID is the session's own identifier, set from the first record
	// in this scan that carried one (the source's own "session" record,
	// always the first line of the file - an incremental scan starting
	// past it will not see it again). Callers merge this with any value
	// already known from a prior scan, exactly like CWD/GitBranch (change
	// fix-resume-session-identity).
	SourceID *string

	// Topic is set from the last KindTopic record seen in this scan (a
	// later title supersedes an earlier one).
	Topic *string

	// CustomTitle is set from the last KindCustomTitle record seen in this
	// scan - the user-chosen session name. Sources write it again on every
	// rename (and, in Claude Code, repeatedly with the current name), so
	// last-one-wins is what makes a rename take effect.
	CustomTitle *string

	// LastPrompt is the most recent user-authored text seen in this scan:
	// updated by KindUserPrompt and KindLastPrompt records.
	LastPrompt *string

	// Origin is the strongest origin signal any record in this scan
	// carried, so a session counts as automated if any of its records says
	// so. Callers merge it across scans like Topic/CustomTitle. It is
	// always one of the closed set - OriginUnknown when no record named
	// an origin (the zero value of session.Origin is an empty string, not
	// OriginUnknown, so Scan never reports that).
	Origin session.Origin

	// Client is the first client any record in this scan named, kept
	// verbatim (see Record.Client). Callers merge it across scans like
	// CWD/GitBranch: it rides every record for the sources that carry it
	// at all, so a delta either sees it on every record or on none.
	Client *string

	// HumanPrompt reports that at least one prompt in this scan was one
	// the source attributed to a person. It is reported separately from
	// Origin because callers merging scans need to know that the evidence
	// was in *this* delta, not just what the evidence concluded.
	HumanPrompt bool

	// Prompts collects every KindUserPrompt's text seen in this scan, in
	// order. This is the tier-2 search-index source for sources with no
	// prompt index of their own (pi), and the fallback tier-2 source for
	// any individual session a source's own index doesn't cover (task 6.4).
	Prompts []PromptText

	// Tail holds up to tailWindow of the most recent normalized records,
	// for end-state classification (task 4.5).
	Tail []Record

	// Compactions accumulates every KindCompactionBoundary record found in
	// this scan (task 4.6). Callers append this to whatever was already
	// stored for the session from earlier scans.
	Compactions []CompactionInfo

	// RecordCount is how many recognizable records (of any kind) were
	// classified in this scan.
	RecordCount int64

	// EndOffset is the byte offset in the file immediately after the last
	// complete line consumed. Callers persist this as the cursor for the
	// next incremental Scan (task 6.1).
	EndOffset int64
}

// PromptText is one user-authored prompt extracted directly from a
// transcript, with its timestamp when known.
type PromptText struct {
	Text string
	At   *time.Time
}

// Scan reads path starting at fromOffset (0 for a first build) up to EOF,
// classifying every line through vocab and folding the result into a
// bounded-memory Result. It is the single read path both a full initial
// build and an incremental refresh use - the only difference is
// fromOffset (task 4.1, 4.3).
//
// A line that fails to parse as JSON, or whose record the vocab does not
// recognize, is skipped without aborting the scan (spec session-index,
// "Unrecognized or malformed record").
func Scan(path string, fromOffset int64, vocab Vocab) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return Result{}, err
		}
	}

	var res Result
	res.EndOffset = fromOffset
	// The zero value is an empty string, not OriginUnknown, so seed the
	// classification: a scan whose records never name an origin (pi, omp,
	// hermes - they record no entrypoint field at all) stays unknown
	// rather than leaking an empty value.
	res.Origin = session.OriginUnknown

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // transcripts can have long lines (embedded file content)

	offset := fromOffset
	for sc.Scan() {
		line := sc.Bytes()
		lineLen := int64(len(line)) + 1 // +1 for the newline the scanner strips
		offset += lineLen

		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue // malformed record: skip, keep going
		}
		rec, ok := vocab.Classify(raw)
		if !ok {
			continue // unrecognized record type: skip, keep going
		}
		res.RecordCount++

		// cwd/branch can ride along on any record kind (Claude stamps them
		// on every record, not only KindSessionMeta ones), so this check is
		// unconditional rather than scoped to one Kind.
		if res.CWD == nil && rec.CWD != nil {
			res.CWD = rec.CWD
		}
		if res.GitBranch == nil && rec.GitBranch != nil {
			res.GitBranch = rec.GitBranch
		}
		if res.SourceID == nil && rec.SourceID != nil {
			res.SourceID = rec.SourceID
		}
		// A session takes the strongest origin signal its records carry: one
		// automated record is enough to mark the whole session automated, and
		// a later interactive record never overrides that. Records carrying
		// no signal (or an explicit unknown) leave res.Origin untouched.
		if rec.Origin == session.OriginAutomated {
			res.Origin = session.OriginAutomated
		} else if rec.Origin == session.OriginInteractive && res.Origin != session.OriginAutomated {
			res.Origin = session.OriginInteractive
		}
		if res.Client == nil && rec.Client != nil {
			res.Client = rec.Client
		}
		// A prompt the source attributes to a person is only counted when
		// it is a prompt: the human marker also rides records that carry
		// injected text rather than anything typed, and those must not
		// speak for who was driving.
		if rec.Kind == KindUserPrompt && rec.HumanTyped {
			res.HumanPrompt = true
		}

		switch rec.Kind {
		case KindTopic:
			if rec.Text != nil {
				res.Topic = rec.Text
			}
		case KindCustomTitle:
			if rec.Text != nil {
				res.CustomTitle = rec.Text
			}
		case KindUserPrompt, KindLastPrompt:
			if rec.Text != nil {
				res.LastPrompt = rec.Text
			}
			if rec.Kind == KindUserPrompt && rec.Text != nil {
				res.Prompts = append(res.Prompts, PromptText{Text: *rec.Text, At: rec.Timestamp})
			}
		case KindCompactionBoundary:
			if rec.Compaction != nil {
				res.Compactions = append(res.Compactions, *rec.Compaction)
			}
		}

		// The tail window drives end-state classification, which cares
		// about the flow of actual conversation turns - user prompts,
		// assistant turns, tool use/results - not sidecar metadata like a
		// topic or a last-prompt trailer, which would otherwise mask the
		// true last conversational event.
		switch rec.Kind {
		case KindUserPrompt, KindAssistantText, KindToolUse, KindToolResult:
			res.Tail = append(res.Tail, rec)
			if len(res.Tail) > tailWindow {
				res.Tail = res.Tail[1:]
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	// A person typing outranks the entrypoint. Claude Code stamps its SDK
	// entrypoint on every record of a session an editor client drove, which
	// by itself reads as automation - and would hide a Neovim/CodeCompanion
	// chat, a real conversation, behind the non-interactive hide rule. Where
	// the two disagree, the prompts win: a script's prompts are not marked
	// human, so nothing that is actually automation is unhidden by this
	// (change show-editor-clients).
	if res.HumanPrompt {
		res.Origin = session.OriginInteractive
	}
	res.EndOffset = offset
	return res, nil
}

// textFromBlocks concatenates the text of "text"-typed content blocks in a
// content-block list, the shape Claude, pi, and omp all use for
// user/assistant message content.
func textFromBlocks(blocks []any, blockType string, textKey string) (string, bool) {
	var out string
	found := false
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := bm["type"].(string); t != blockType {
			continue
		}
		if s, ok := bm[textKey].(string); ok {
			out += s
			found = true
		}
	}
	return out, found
}

func hasBlockType(blocks []any, blockType string) bool {
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := bm["type"].(string); t == blockType {
			return true
		}
	}
	return false
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return &t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	return nil
}
