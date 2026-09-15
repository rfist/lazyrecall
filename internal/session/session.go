// Package session defines the uniform session record that every adapter
// projects its source's data into (design.md decision 3), the closed set of
// end states (spec session-review), and the lineage model that keeps
// annotations attached across compaction and continuation (design.md
// decision 8).
package session

import "time"

// EndState is the closed set of terminal states a session can be assigned.
// It is derived from the session's own recorded content, never guessed from
// file modification times alone (spec session-review, "Session end state
// classification").
type EndState string

const (
	// EndStateCompleted: the final exchange ends with a completed agent turn.
	EndStateCompleted EndState = "completed"
	// EndStateDangling: the final recorded exchange is a user prompt with no
	// agent response following it.
	EndStateDangling EndState = "dangling"
	// EndStateInterrupted: the final agent turn initiated a tool operation for
	// which no result was ever recorded.
	EndStateInterrupted EndState = "interrupted"
	// EndStateAbandoned: no terminal marker, and no recorded activity for
	// longer than the configured inactivity threshold.
	EndStateAbandoned EndState = "abandoned"
	// EndStateUnknown: the source does not record enough detail to
	// distinguish between the other states. Never silently upgraded to a
	// more specific state.
	EndStateUnknown EndState = "unknown"
)

// Valid reports whether s is one of the closed set of end states.
func (s EndState) Valid() bool {
	switch s {
	case EndStateCompleted, EndStateDangling, EndStateInterrupted, EndStateAbandoned, EndStateUnknown:
		return true
	}
	return false
}

// NeedsAttention reports whether an end state belongs in the loose-ends
// report (spec session-review, "Loose ends report": dangling, interrupted,
// or abandoned).
func (s EndState) NeedsAttention() bool {
	switch s {
	case EndStateDangling, EndStateInterrupted, EndStateAbandoned:
		return true
	}
	return false
}

// clientLabels maps a source's raw client value to the short name a
// listing shows for it. It is an inference, and deliberately a small and
// separate one: the transcript states an *entrypoint*, and what that
// implies about the program on the other end can be corrected here - in
// one table, in a build, with no reindex - if it ever stops holding.
//
//   - "cli" has no label at all: the source's own terminal is the
//     unremarkable case, and a row that says so for most sessions teaches
//     the reader nothing.
//   - "sdk-ts" is a program embedding Claude Code's TypeScript SDK. In
//     practice that is an editor speaking ACP - Neovim's CodeCompanion
//     here, Zed elsewhere - so it reads as "acp": what the session came
//     through, without claiming which editor it was.
//   - "sdk-cli" is a script driving the CLI.
//
// A value not listed is its own label, so a client this build has never
// heard of is still shown and still filterable rather than silently
// blanked.
var clientLabels = map[string]string{
	"cli":            "",
	"sdk-ts":         "acp",
	"sdk-cli":        "sdk",
	"claude-desktop": "desktop",
}

// ClientLabel is the short name a listing shows for a raw client value,
// and "" for a client not worth showing (the source's own terminal).
func ClientLabel(raw string) string {
	if l, ok := clientLabels[raw]; ok {
		return l
	}
	return raw
}

// ClientRaw is ClientLabel's inverse: the raw client value a label stands
// for, or the argument itself when it is already a raw value (or a name
// this build does not know). Filtering compares both spellings rather than
// rewriting one into the other, so an ambiguous name narrows to more
// sessions, never to none.
func ClientRaw(label string) string {
	for raw, l := range clientLabels {
		if l != "" && l == label {
			return raw
		}
	}
	return label
}

// Origin is who drove a session: a person at a terminal, or a script.
type Origin string

const (
	OriginInteractive Origin = "interactive"
	OriginAutomated   Origin = "automated"
	OriginUnknown     Origin = "unknown"
)

// Valid reports whether o is one of the closed set of origins.
func (o Origin) Valid() bool {
	switch o {
	case OriginInteractive, OriginAutomated, OriginUnknown:
		return true
	}
	return false
}

// CompactionEvent describes one compaction, when the source records enough
// detail to report it (spec session-review, "Compaction detail available").
type CompactionEvent struct {
	// Automatic is true if the compaction was triggered by the agent rather
	// than requested by the user. Absent (nil) when the source records that a
	// compaction happened but not how it was triggered.
	Automatic *bool
	// DroppedTokens is how much context was dropped, when the source records
	// it.
	DroppedTokens *int64
	// At is when the compaction happened, when known.
	At *time.Time
}

// Compaction is a session's compaction record. A session that was never
// compacted, on a source that has the concept, is represented as
// &Compaction{Count: 0}. A source that has no concept of compaction at all
// is represented as a nil *Compaction on the Session (design.md decision 3 /
// spec session-review, "Source does not record compaction": reported as
// absent rather than as false).
type Compaction struct {
	Count  int
	Events []CompactionEvent
}

// Session is the uniform record every adapter projects its source's data
// into (spec session-index, "Uniform session record"). Optional fields are
// pointers so that "the source did not record this" (nil) is distinguishable
// from "the source recorded an empty value" (non-nil pointing at a zero
// value). No field is ever populated with a placeholder, guess, or derived
// value standing in for data the source did not provide.
type Session struct {
	// ID is LazyRecall's own identifier for this session, stable across
	// refreshes: "<source>:<profile>:<source session id>".
	ID string

	// Source is the adapter that produced this record: "claude", "pi",
	// "omp", or "hermes".
	Source string

	// Profile names the install this session was read from (e.g.
	// "claude-work", "claude-personal", "omp" - see internal/profile).
	// Despite the field's historical name it no longer marks an isolated
	// profile boundary now that one index holds every install together
	// (change group-sessions-in-one-index): it decides which config root a
	// session resumes into and is written out as sessions.install, not
	// which database it lives in.
	Profile string

	// SourceSessionID is the identifier the source itself uses. It is not
	// guaranteed stable across compaction or continuation - LineageID is
	// what survives that.
	SourceSessionID string

	// LineageID groups this session with any it continues from or forks
	// into. Annotations attach to the lineage, not to SourceSessionID
	// (design.md decision 8).
	LineageID string

	// ContinuesFrom is the SourceSessionID this session continues, when the
	// source records a continuation or compaction relationship. Nil when
	// this session starts its own lineage.
	ContinuesFrom *string

	// Topic is an agent-written title or topic for the session, when the
	// source records one.
	Topic *string

	// Name is a title the user gave the session inside the source tool
	// (Claude Code's session rename), when the source records one. It is
	// kept separate from Topic rather than folded into it: a name was
	// chosen by a person and an agent-derived topic must never overwrite
	// it, so display prefers Name and falls back to Topic.
	Name *string

	// LastPrompt is a preview of what the session was last doing - the most
	// recent user prompt or equivalent, when available.
	LastPrompt *string

	// CWD is the working directory, read from the session's own recorded
	// content - never decoded from a source's directory-naming scheme
	// (spec session-index, "Authoritative working directory").
	CWD *string

	// GitBranch is the git branch recorded inside the session, when present.
	GitBranch *string

	// GitRepoRoot is CWD's own worktree root, when it can be determined.
	// For a linked worktree this is that worktree's own path, not the main
	// repository's.
	GitRepoRoot *string

	// GitCommonRoot is the canonical repository identity shared by a
	// repository's main worktree and every worktree linked to it. Grouping
	// by repository groups by this field, so worktrees nest under one
	// entry instead of appearing as unrelated locations (spec
	// session-search, "Grouping by repository and worktree").
	GitCommonRoot *string

	// StartedAt is when the session began, when recorded.
	StartedAt *time.Time

	// LastActivityAt is the most recent recorded activity in the session.
	// Sessions are ordered by this field, most recent first.
	LastActivityAt *time.Time

	// EndState is this session's classification from the closed set above.
	EndState EndState

	// Origin is who drove the session - a person at a terminal or a script
	// - when the source records enough to tell.
	Origin Origin

	// HumanPrompt reports whether any prompt ever recorded for this session
	// was one the source attributed to a person (Claude Code's origin.kind
	// "human" - transcript.Result.HumanPrompt, folded across every pass
	// that has ever scanned this session). It is stored - not derived from
	// a previously *computed* Origin - because Origin also comes out
	// interactive by default for entrypoints this build has never
	// classified as automated (claudeOrigin's allowlist-of-automated
	// fallback, e.g. entrypoint "cli"), and a rule of "stay interactive if
	// the prior row was" would then keep a plain terminal session
	// interactive forever after automation starts appending to it, purely
	// because of what an earlier, unrelated pass's default happened to
	// conclude. HumanPrompt records the one fact that actually matters -
	// a person typed here - so an incremental refresh (reading the
	// persisted flag) and a full rebuild (which sees every record and
	// recomputes it fresh) always agree (audit fix for the 9feca0a defect
	// where they didn't). Sticky once true: a human-marked prompt earlier
	// in the transcript does not stop being evidence just because it falls
	// behind an incremental pass's cursor.
	HumanPrompt bool

	// Client is the program the session was driven through, as the source
	// named it, when the source names it at all: Claude Code's own terminal
	// ("cli") or something embedding it, such as an editor's ACP client
	// ("sdk-ts" - Neovim's CodeCompanion, for one). It answers "where was I
	// when I had this conversation", which Source cannot: an editor chat and
	// a terminal chat are both claude sessions, resumed the same way, and so
	// this is a property of the session rather than a source of its own
	// (change show-editor-clients). ClientLabel below names it for display.
	Client *string

	// Compaction is nil when the source has no concept of compaction at
	// all; otherwise it reports how many times (possibly zero) this session
	// was compacted.
	Compaction *Compaction

	// TranscriptPath is the on-disk transcript this session was read from,
	// when the source keeps transcripts. Sources that keep everything in a
	// database (hermes) leave this nil.
	TranscriptPath *string

	// MessageCount is the number of messages/turns, when the source
	// records or cheaply provides it.
	MessageCount *int64

	// Resumable is false for sessions that have no working directory to
	// resume into (e.g. hermes sessions whose source is a chat platform).
	Resumable bool

	// DirExists reports whether CWD still exists on this machine, as of the
	// last time it was checked. Nil until checked.
	DirExists *bool

	// GitResolved reports whether a refresh has already asked git about
	// this session's CWD - regardless of what it found. GitRepoRoot and
	// GitCommonRoot alone cannot distinguish "never asked" from "asked,
	// and this directory really isn't a git repository": both read back
	// nil either way. Without this flag a non-repo CWD had no cached
	// outcome to fall back on and was re-resolved (one git subprocess
	// call) on every single refresh, forever (perf fix for the
	// group-sessions-in-one-index regression, devdocs/fyi.md). False
	// until a refresh has actually checked - never a guess.
	GitResolved bool
}

// Lineage is the identity annotations attach to, independent of any single
// source session id (design.md decision 8). A lineage is never deleted
// because its sessions vanished from their source; it is marked orphaned
// and its annotations are retained (spec session-annotations, "Annotated
// session disappears from its source").
type Lineage struct {
	ID       string
	Profile  string
	Orphaned bool
}
