# Adding a source

A "source" is one coding agent lazyrecall knows how to find sessions for: `claude`, `pi`, `omp`, `hermes`, `goose`, `opencode`, `kilo`, `antigravity`. This guide walks through what it takes to add another one. The most likely next candidate is Codex - JSONL rollout transcripts under `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl`, resumed with `codex resume <id>` - and it is used below as a running example of a transcript-file source, but nothing here implements it.

Read `internal/adapter/adapter.go` and one or two existing adapters before starting; this guide points at the real files rather than repeating them in full.

## 1. What the config alone can do

`internal/config.Config.Sources` is a `map[string]config.Source`, and each entry has exactly four knobs (`internal/config/config.go`):

```go
type Source struct {
    Roots         []string // config roots to scan
    Resume        []string // argv template; "{id}" becomes the session id
    EnvVar        string   // env var set to the install's root when resuming
    SingleInstall bool     // true if this source can only ever have one install
}
```

If the source you want already has an adapter, you can often get what you need purely from `~/.config/lazyrecall/config.toml`: point `roots` somewhere else, change the `resume` argv, add `env_var`, or turn a source off with `roots = []`. Every root also picks up a `LAZYRECALL_<NAME>_HOME` environment override for free (`internal/profile.sourceRoots`) - a hypothetical `codex` source would get `LAZYRECALL_CODEX_HOME` with no extra code.

What config **cannot** do is invent a source that has no compiled-in adapter. `internal/profile.Discover` will happily read an unrecognized `[sources.foo]` table and turn an existing `foo` root into an "install" (`profile.Profile`), but `internal/refresh.Refresher.adapters()` is a fixed list of concrete adapters (see [Registration](#2-the-adapter-interface-and-how-its-wired-in) below); nothing walks that install's directory, so it never produces a session. A genuinely new source always needs Go code.

**The gotcha to know before writing any config:** a `[sources.X]` table in the file replaces that source's entire configuration when it appears at all - it does not merge field by field with the defaults. Writing

```toml
[sources.claude]
env_var = "CLAUDE_CONFIG_DIR"
```

silently zeroes `roots` and `resume` too, because the table is now file-owned and the file didn't mention them. This is also why per-install `labels` live in their own top-level `[labels]` table rather than nested under `[sources.claude]` - see `Config.Labels`'s doc comment in `internal/config/config.go` for the incident that caused that split.

## 2. The adapter interface, and how it's wired in

`internal/adapter/adapter.go` defines the whole boundary:

```go
type Adapter interface {
    Name() string
    Discover(p profile.Profile) ([]Discovered, error)
}
```

- **`Name()`** is the source name used everywhere else: the config key, the composite session id prefix, the `--agent` filter value.
- **`Discover(p)`** enumerates every session this source has for one install (`profile.Profile`, a config root plus which source it belongs to) and returns a `[]Discovered`. It never parses transcript content - that's `internal/transcript`'s job for file-backed sources, or the adapter's own SQL for database-backed ones.
- A source absent on this machine (no root, or the root has no data) returns `nil, &adapter.Unavailable{Reason: "..."}`. The refresh pipeline treats this as informational, not fatal, and keeps going with every other source (`internal/refresh/refresh.go`'s `Refresh`, one `SourceStatus` row per source/install pair).

`Discovered` is the one thing an adapter hands back per session:

```go
type Discovered struct {
    Session        session.Session
    TranscriptPath string // "" for a source with no transcript file (hermes, goose, ...)
    FileSize       int64  // meaningless when TranscriptPath is ""
}
```

Fields the adapter can't cheaply know are left nil on `Session`; for a transcript-file source the refresh pipeline fills them in from a `transcript.Scan` pass. A database-backed adapter has no such second pass, so it must build a complete `session.Session` itself.

There's a second, optional interface for sources whose conversation content lives somewhere other than a file:

```go
type ConversationReader interface {
    Conversation(p profile.Profile, sourceSessionID string, limits transcript.ConversationLimits) (turns []transcript.Turn, dropped int, err error)
}
```

This is what the Transcript tab calls for `hermes`, `goose`, `opencode`, and `kilo`. `antigravity` deliberately does not implement it - see [antigravity](#antigravity-listing-only) below.

**Registration is not automatic.** A new adapter needs to be added in a small, fixed set of places:

1. `internal/config/config.go`'s `defaultSources()` - the source's default `Roots`, `Resume` template, `EnvVar` (if any), `SingleInstall`.
2. `internal/refresh/refresh.go`'s `Refresher.adapters()` - construct and append your `adapter.Adapter`. This is also where `ConversationReaderFor` looks sources up, so implementing `ConversationReader` needs no separate registration.
3. `internal/refresh/tier2.go`'s `tier2ForSource` - add a `case "yourSource":` if the source has its own prompt/message index to read incrementally (skip this if you're relying entirely on transcript-extracted prompts, the way `pi` does).
4. If it's a transcript-file source, `internal/transcript/transcript.go`'s `VocabFor` switch.
5. `cmd/lazyrecall/main.go`'s `defaultConfigFile` constant - a commented-out `[sources.yourSource]` block, purely so `lazyrecall config init` documents it. Not functionally required (the built-in default already applies without it), but every existing source has one.

`cmd/lazyrecall/main.go` otherwise stays adapter-agnostic: resume, `--agent` filtering, and labels all work generically off `config.Config` and `profile.Profile` once the registration points above are done.

## 3. The two shapes of source

### Transcript-file sources: claude, pi, omp

These sources write one line-delimited JSON file per session. `internal/transcript` is the single shared reader; adapters never parse transcript content themselves (`internal/transcript/transcript.go`'s package doc). The pieces:

- **`Vocab`** (`transcript.go`) is the interface each source implements: `Classify(raw map[string]any) (Record, bool)` turns one parsed JSON line into a normalized `Record`, or reports `ok=false` for a line this program doesn't recognize (never an error - transcripts are allowed to contain record types nobody's seen yet).
- **`Record`** carries a `Kind` (`KindUserPrompt`, `KindAssistantText`, `KindToolUse`, `KindToolResult`, `KindSessionMeta`, `KindTopic`, `KindCustomTitle`, `KindLastPrompt`, `KindCompactionBoundary`) plus whatever fields are relevant: text, timestamp, cwd/branch, origin, client, tool names, compaction detail.
- **`Scan(path, fromOffset, vocab)`** reads a file (or a byte range of one, for incremental refresh) and folds every classified record into a bounded-memory `Result`: first-seen CWD/branch/source-id, last-seen topic/custom-title/last-prompt, every prompt's text (for tier-2 search fallback), a bounded tail window of the last ~12 conversational records (for end-state classification), and the byte offset to resume from next time.
- **`transcript.ClassifyEndState(tail, inactive)`** (`classify.go`) derives the closed `session.EndState` set from the last non-metadata record in that tail: a trailing user prompt is `dangling`, a trailing tool call with no result is `interrupted`, a cleanly finished assistant turn is `completed`, and anything else is `unknown` (or `abandoned` if `inactive` and there's no other signal). This is the one thing new vocab code does not have to invent - it comes free from the shared record kinds, as long as `Classify` sets `Kind`/`StopReason` correctly.
- **`Conversation(path, vocab, limits)`** (`conversation.go`) is the same idea for the Transcript tab: read turns in file order, keep the most recent `limits.MaxTurns`, truncate any one turn to `limits.MaxTurnBytes`. It runs through the same `Vocab`, so the index and the tab can never disagree about what a record is.

Look at `internal/transcript/claude.go` for the fullest example: it maps six record types, tracks Claude's "entrypoint" field into `Origin`/`Client`, and - worth copying the *pattern* of, even if Codex's own harness never injects anything - filters harness-injected blocks (`isInjectedBlock`) so a `<system-reminder>` or `<command-message>` block a script wrote never gets indexed as something the user typed. `pi.go` and `omp.go` are much smaller and a better size reference for a first pass; `omp.go` also shows how a source that already has some of its own history index (see below) still needs a vocab, because the transcript remains the source of truth for identity, cwd, topic, and end state.

For the **adapter** side of a transcript-file source, look at `internal/adapter/claude/claude.go` or the much smaller `internal/adapter/pi/pi.go`: walk the directory tree, find every transcript file, and build one `session.Session` per file with `SourceSessionID` set from the file's own name and `EndState: session.EndStateUnknown` (the real end state comes from the first `transcript.Scan` pass). Never decode a session's working directory out of a directory-naming scheme - Claude's `projects/<encoded-cwd>/` directories exist only to *find* files; the cwd itself always comes from parsed transcript content (`claude.go`'s package doc, and `transcript.ClassifyEndState`'s sibling `ExtractWorkingDirectory`).

Codex's own layout - `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` - is a three-level nested walk rather than claude's one level (`projects/<dir>/*.jsonl`) or pi/omp's one level (`agent/sessions/<dir>/*.jsonl`), so `Discover` would need an extra loop over year/month/day, but the rest of the adapter is the same shape: enumerate files, don't interpret the path, hand the transcript off to a new `codexVocab` for content.

Incremental scanning (task the refresh pipeline handles, not something an adapter writes) works via a byte-offset cursor per `(source, sourceSessionID)`, persisted in the `cursors` table and applied in `internal/refresh/pipeline.go`'s `applyTranscript`. It also detects a transcript that was rewritten from scratch rather than appended to (file shrank, or its leading bytes changed) and re-reads from zero in that case (`transcriptRewritten`, `sizeShrank`, `leadingBytesChanged`) - nothing a new adapter needs to implement, but useful to know if a session's counts ever look doubled.

### Database-backed sources: hermes, goose, opencode, kilo, antigravity

These sources keep everything in their own SQLite database and write no transcript file at all - hermes established the shape (`internal/adapter/hermes/hermes.go`'s package doc), and goose/opencode/antigravity followed it. For these, `Discovered.TranscriptPath` is always `""`, and the adapter's `Discover` builds a complete `session.Session` in one pass, with no separate transcript-scanning step afterward.

Everything goes through `internal/sqlitex.Runner` with `ReadOnly: true`. **Lazyrecall never writes to another agent's database** - this is enforced at the connection level (`mode=ro` + `query_only` pragma in `sqlitex.go`), not just by convention, so don't reach for a writable `Runner` for a source's own DB under any circumstance. Note also that despite the `SQLite3Path`/`BinPath` field names surviving on `Runner` and on adapter constructors like `hermes.New(sqlite3Path string)`, they're vestigial: lazyrecall used to shell out to a `sqlite3` binary and now drives a pure-Go, in-process driver (`modernc.org/sqlite`) instead, so every call site in this codebase passes `""` for it (see `cmd/lazyrecall/main.go`'s `refresh.New(installs, "")`). Keep accepting the parameter for consistency with the other adapters; don't try to point it at a real binary.

The pattern per adapter:

- **`Discover`** runs one query (often with a `WITH last_msg AS (...)` CTE to pull in the session's last message/role/finish-reason) and maps rows straight to `session.Session`. End-state classification is source-specific and reads whatever completion signal that source actually records - never inferred from timestamps (`session.EndState`'s own doc comment: "never guessed from file modification times alone"). The four existing sources sit at different points on that spectrum: `hermes` has an explicit `finish_reason` column, `opencode` has an explicit `finish` field (`stop`/`tool-calls`/`error`/`aborted`) reached via `json_extract`, `goose` has to infer from the *type of the last content block* rather than role alone (a `toolResponse` block rides under role `"user"` in its schema - confirmed against real data before writing that adapter, see `goose.go`'s package doc), and `antigravity` has no per-message signal at all, so it reports `EndStateUnknown` except for its one unambiguous `killed` flag.
- **`PromptsSince(p, cursor)`** (not part of the `Adapter` interface - a plain method the source's own file calls directly from `internal/refresh/tier2.go`) is the tier-2 full-text search feed: query rows newer than a cursor, return prompt text plus a new cursor. `pi` has no index to query and relies entirely on transcript-extracted prompts as a fallback; `antigravity` has neither an index nor a transcript to fall back to, so its sessions are simply not full-text searchable by prompt content (still browsable and filterable by title/preview).
- **`Conversation(p, sourceSessionID, limits)`** implements `adapter.ConversationReader` for the Transcript tab. Build `[]transcript.Turn` directly from the source's own message/part rows, then call `transcript.LimitTurns(turns, limits)` - the same turn-budget and empty-turn logic `transcript.Conversation` applies to a file, factored out so a hermes/goose/opencode session reads with exactly the same rules as a claude one (see `conversation.go`'s doc comment on `LimitTurns`).

`internal/adapter/opencode/opencode.go` is the fullest example of this shape (it also documents Kilo's relationship to it, next). `internal/adapter/hermes/hermes.go` is a good second read since its schema is flatter (plain text columns, not JSON blobs to `json_extract`).

#### Kilo reuses the OpenCode adapter

Kilo ships OpenCode's schema and CLI flags verbatim under a different name and database file. Rather than duplicate the queries, `internal/adapter/opencode.Adapter` takes its source name and database filename as fields:

```go
func New(sqlite3Path string) *Adapter { return &Adapter{name: "opencode", dbFile: "opencode.db", ...} }
func NewFor(name, dbFile, sqlite3Path string) *Adapter { return &Adapter{name: name, dbFile: dbFile, ...} }
```

and `internal/adapter/kilo/kilo.go` is nothing but `opencode.NewFor("kilo", "kilo.db", sqlite3Path)`. If a future source turns out to share another source's schema exactly, this is the pattern to follow rather than a second copy of the same SQL.

#### Antigravity: listing-only

`internal/adapter/antigravity/antigravity.go` discovers sessions from one clean summary table (`conversation_summaries.db`) but does **not** implement `ConversationReader` or `PromptsSince`: its actual per-conversation detail lives in a second database whose content is a protobuf-framed BLOB with no available `.proto` schema to decode (confirmed by hex-dumping a real row - see the package doc). This is the right shape to copy for any future source whose detail store turns out to be undecodable: implement `Discover` for the listing, skip the rest, and the UI already degrades correctly - `internal/cli/browse.go`'s Transcript tab checks `transcript.VocabFor` first, then `refresh.ConversationReaderFor`, and shows a "this agent stores its conversations in a format lazyrecall cannot decode" message when neither exists, with no special-casing needed elsewhere.

## 4. Identity

- **Composite session id**: `"<source>:<install>:<sourceSessionID>"` (`session.Session.ID`'s doc comment). `internal/refresh/refresh.go`'s `Refresh` recomputes this after applying a transcript scan, because `SourceSessionID` itself can be corrected mid-pass (next point) - anything downstream (the DB primary key, the lineage root, `session.InstallFromID`/`sourceSessionIDFrom` splitting it back apart for resume) must see the corrected value.
- **`SourceSessionID`** should be the identifier the source itself would recognize for `resume`, not a storage detail. For pi and omp, the transcript's own leading `"session"` record carries an `id` field that is the *real* session id - the file name (`<timestamp>_<uuid>.jsonl`) is not something either tool's own `--session`/`--resume` flag understands, it just happens to look like one. `transcript.Result.SourceID` (set from `Record.SourceID`) carries this back to `applyTranscript`, which corrects `s.SourceSessionID` from it once the transcript has actually been read. If your source's transcript records its own id, follow this pattern rather than deriving an id purely from the file name.
- **Installs**: a `profile.Profile` is one discovered config root. `SingleInstall: true` (`config.Source`) means the install is always named after the source itself (`hermes`, `goose`, ...); otherwise it's named after the root's own directory basename (`claude`, `claude-personal`). This is a config field precisely because it must never be *inferred* from how many roots happen to be configured - doing so once made a profile's name (and so its data) depend on how the config file was written, silently orphaning annotations (see `internal/profile/profile.go`'s `installName` doc comment).
- **Lineages**: `resolveLineage` (`internal/refresh/pipeline.go`) walks a session's `ContinuesFrom` chain to the root, within that pass's own discovered sessions for that one source, and `session.LineageID(source, install, rootSourceSessionID)` hashes the result into a stable id annotations attach to. A source that records no continuation relationship at all just gets `rootSourceSessionID == SourceSessionID` - each session is its own lineage.
- **What the resume template receives**: `{id}` in `config.Source.Resume` is replaced with `SourceSessionID` alone (`internal/resume/resume.go`'s `agentCommand`), element-wise, never through a shell - so a template like `["codex", "resume", "{id}"]` is safe even if a session id somehow contained shell metacharacters. If `EnvVar` is set, `internal/profile.Profile.Root()` for that session's install is exported under that name before the agent starts.

## 5. Step-by-step checklist

1. Create `internal/adapter/<source>/<source>.go` with `New(...) *Adapter`, `Name() string`, and `Discover(p profile.Profile) ([]adapter.Discovered, error)`.
2. Register it: `internal/refresh/refresh.go`'s `adapters()`, `internal/config/config.go`'s `defaultSources()`, and `internal/refresh/tier2.go`'s `tier2ForSource` if it has its own prompt index.
3. Transcript-file source: add a `<source>Vocab` to `internal/transcript` and a case in `VocabFor`. Database-backed source: write the `Discover` query plus `classifyEndState` for that source's own completion signal.
4. If sessions should be readable in the Transcript tab and it's database-backed, implement `adapter.ConversationReader`'s `Conversation` method, ending with `transcript.LimitTurns`.
5. If the source has its own message/prompt index, implement `PromptsSince(p, cursor)` for tier-2 full-text search; otherwise transcript-extracted prompts (transcript-file sources only) are the fallback, and a DB-backed source with neither is simply not prompt-searchable (document that, as `antigravity`'s package doc does).
6. Write tests. For a transcript-file source, follow `internal/transcript/transcript_test.go` / `internal/adapter/claude/claude_test.go` / `internal/adapter/pi/pi_test.go`: write synthetic single-line-per-record `.jsonl` fixtures with `os.WriteFile` under `t.TempDir()` (every existing test file says so explicitly: "Synthetic fixtures only - no real session content"). For a database-backed source, follow `internal/adapter/opencode/opencode_test.go` or `internal/adapter/goose/goose_test.go`: build a throwaway database with `sqlitex.Runner{DBPath: ...}.Exec(ddl)` against the real schema, insert synthetic rows, then call `Discover`/`PromptsSince`/`Conversation` against it and assert on the result. Cover at minimum: `classifyEndState`'s cases, `Discover` producing the right `CWD`/`Topic`/`ContinuesFrom`/`Resumable`, and `Conversation` filtering out the record types that carry no displayable text.
7. Add a row to the README's [Supported sources](../README.md#supported-sources) table, and update the four `sources.<agent>.*` default rows in [Configuration](../README.md#configuration).
8. Optional: add a commented `[sources.<source>]` block to `cmd/lazyrecall/main.go`'s `defaultConfigFile`, so `lazyrecall config init` documents the new source too.

## 6. Pitfalls

- **A `[sources.X]` table replaces the whole source, not just the keys it sets.** Covered above, but it's the mistake most likely to recur: writing only `env_var` under an existing `[sources.claude]` table silently drops `roots` and `resume` back to their zero value.
- **Per-install labels belong in a top-level `[labels]` table, never nested under `[sources.<name>]`**, for the same reason - it was tried once and it made every claude install vanish from discovery the moment a label was configured (see `Config.Labels`'s doc comment).
- **A single-install source's one install is named exactly like the source itself, on purpose.** `hermes`'s only install is named `"hermes"`. This looks like a naming collision but isn't one in practice - `--agent` and label resolution check a configured label first, then a source name, then a discovered install's own name, in that explicit order (`profile.Label`/`ConfiguredLabel`) - but it's worth knowing when testing a new single-install source's `--agent` filtering.
- **A session with no working directory is not resumable, and that's fine.** `session.Session.Resumable` should be `false` whenever `CWD` is nil/empty (hermes's chat-platform-origin sessions are the existing example) - don't force a placeholder cwd just to make a session resumable.
- **Cursors are keyed per install, not globally.** `internal/refresh/tier2.go`'s source-level cursor key is `"*:" + install`, specifically because two installs of the same source sharing one cursor would each corrupt the other's incremental scan the moment there's more than one install of that source on a machine.
- **A transcript the index has a path for can simply be gone later** - most agents periodically clean up their own old transcripts. Nothing an adapter needs to code defensively around: `internal/cli/browse.go`'s Transcript tab already turns `os.IsNotExist` on a read into "the agent cleaned it up since the last refresh" rather than an error.
- **End state must come from the source's own recorded signal, never inferred from mtimes or recency.** When a source (like antigravity, mostly) simply doesn't record enough to tell, report `session.EndStateUnknown` rather than guessing - it's one of the closed `session.EndState` values for exactly this reason.
