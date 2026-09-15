// Package refresh is the orchestrator: it calls every adapter for every
// discovered install, drives the transcript reader incrementally, ingests
// each source's tier-2 prompt index, writes everything to LazyRecall's own
// single index through sqlitex, and keeps lineages/orphans up to date. It
// is what "every operation refreshes the index before answering" (design.md
// decision 6) actually runs.
package refresh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rfist/lazyrecall/internal/adapter"
	antigravityadapter "github.com/rfist/lazyrecall/internal/adapter/antigravity"
	claudeadapter "github.com/rfist/lazyrecall/internal/adapter/claude"
	gooseadapter "github.com/rfist/lazyrecall/internal/adapter/goose"
	hermesadapter "github.com/rfist/lazyrecall/internal/adapter/hermes"
	kiloadapter "github.com/rfist/lazyrecall/internal/adapter/kilo"
	ompadapter "github.com/rfist/lazyrecall/internal/adapter/omp"
	opencodeadapter "github.com/rfist/lazyrecall/internal/adapter/opencode"
	piadapter "github.com/rfist/lazyrecall/internal/adapter/pi"
	"github.com/rfist/lazyrecall/internal/gitutil"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/schema"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
	"github.com/rfist/lazyrecall/internal/transcript"
)

// InactivityThreshold is how long a session with no clear terminal marker
// must sit untouched before it is classified abandoned rather than unknown
// (spec session-review, "Session stopped without any terminal marker").
const InactivityThreshold = 3 * time.Hour

// Options controls one refresh pass.
type Options struct {
	// FullRebuild discards all cursors and re-reads every session from the
	// start, instead of only what changed since the last refresh (task 6.5;
	// spec session-index, "Index is missing or discarded").
	FullRebuild bool
}

// SourceStatus reports one (source, install) pair's outcome for this
// refresh, so a caller can show "which agents are available" (spec
// session-index, "Source discovery"). Install is "" for a source no
// discovered install carries at all - the same "unavailable" a single
// install with no root for that source used to report, kept as one row per
// adapter rather than silently dropped, so `sum.Sources` still names every
// known source even when nothing on this machine has it (change
// group-sessions-in-one-index: an adapter is no longer scoped to one
// profile, so its unavailability is no longer either).
type SourceStatus struct {
	Source    string
	Install   string
	Available bool
	Sessions  int
	Err       error
}

// Summary is what one refresh pass produced, across every install it
// covered.
type Summary struct {
	Installs []profile.Profile
	Sources  []SourceStatus
}

// Refresher runs one refresh pass over every given install into one shared
// LazyRecall database (change group-sessions-in-one-index: one index for
// every install, replacing one database per profile).
type Refresher struct {
	Installs    []profile.Profile
	DB          *sqlitex.Runner // read-write, LazyRecall's own database
	SQLite3Path string
	Now         func() time.Time

	// GitResolve is the resolver Refresh asks about a CWD's git identity;
	// nil (the default) means gitutil.Resolve. It exists as a seam for
	// tests to inject a fake or counting resolver instead of shelling out
	// to a real git binary against real directories (perf fix for the
	// group-sessions-in-one-index regression, devdocs/fyi.md).
	GitResolve func(dir string) (gitutil.Info, bool)
}

// installsFor returns the installs, in the order they were given to New,
// that carry data for source - the ones adapter.Discover should actually be
// called against for it.
func (r *Refresher) installsFor(source string) []profile.Profile {
	var out []profile.Profile
	for _, p := range r.Installs {
		if p.Roots[source] != "" {
			out = append(out, p)
		}
	}
	return out
}

// New opens (creating the data directory as needed) the single index and
// returns a Refresher that will refresh it over installs.
func New(installs []profile.Profile, sqlite3Path string) (*Refresher, error) {
	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		return nil, fmt.Errorf("refresh: creating %s: %w", profile.DataDir(), err)
	}
	dbPath := profile.DBPath()
	db := &sqlitex.Runner{BinPath: sqlite3Path, DBPath: dbPath}
	if _, err := schema.Open(db); err != nil {
		return nil, fmt.Errorf("refresh: opening the index: %w", err)
	}
	return &Refresher{Installs: installs, DB: db, SQLite3Path: sqlite3Path, Now: time.Now}, nil
}

func (r *Refresher) adapters() []adapter.Adapter {
	return []adapter.Adapter{
		claudeadapter.New(),
		piadapter.New(),
		ompadapter.New(r.SQLite3Path),
		hermesadapter.New(r.SQLite3Path),
		gooseadapter.New(r.SQLite3Path),
		opencodeadapter.New(r.SQLite3Path),
		kiloadapter.New(r.SQLite3Path),
		antigravityadapter.New(r.SQLite3Path),
	}
}

// Refresh runs one pass: enumerate every source, incrementally scan
// whatever changed, ingest tier-2 prompts, and write it all to the
// database in one batch per concern. A source that fails does not stop the
// others (spec session-index, "A source cannot be read"; task 5.6).
func (r *Refresher) Refresh(opts Options) (Summary, error) {
	// Captured before a full rebuild clears the sessions table, so that a
	// session whose SourceSessionID this pass corrects (and whose lineage id
	// therefore changes, since a non-continuing session's lineage root is its
	// own SourceSessionID - design.md decision 8) can have its comments/tags
	// carried from the old lineage id to the new one (change
	// fix-resume-session-identity, task 4.2). Keyed by transcript path - the
	// one thing that does not change across an identifier correction -
	// rather than by the old (about to be wrong) composite id.
	var oldLineageByTranscript map[string]string
	if opts.FullRebuild {
		var err error
		oldLineageByTranscript, err = r.loadLineagesByTranscript()
		if err != nil {
			return Summary{}, err
		}
		if err := r.DB.Exec(`DELETE FROM cursors; DELETE FROM sessions; DELETE FROM prompt_fts;`); err != nil {
			return Summary{}, fmt.Errorf("refresh: clearing index for full rebuild: %w", err)
		}
	}

	cursors, err := r.loadCursors()
	if err != nil {
		return Summary{}, err
	}
	existing, err := r.loadExistingSessions()
	if err != nil {
		return Summary{}, err
	}

	sum := Summary{Installs: r.Installs}
	var sessionRecords []map[string]any
	var promptRecords []map[string]any
	var cursorRecords []map[string]any
	observedLineages := map[string]bool{}
	fallbackPrompts := map[string][]transcript.PromptText{} // adapter.Discover's raw session id -> transcript-extracted prompts
	gitMemo := map[string]gitMemoEntry{}                    // one distinct CWD resolved at most once for this whole pass, across every install/source (gitResolveCached)

	// hermes sessions are collected first so claude/pi/omp lineage linking
	// (task 9.3, "each source's continuation and fork markers") has no
	// cross-source dependency to worry about - every source resolves its
	// own continuation chain independently.
	//
	// Every adapter now runs once per install that carries its source
	// (change group-sessions-in-one-index), instead of once per refresh
	// pass against a single profile: two claude roots are two installs, and
	// each is scanned and written into the same shared index. An adapter
	// with no install at all still gets one "unavailable" status row, by
	// calling it against an empty Profile - the same outcome a.Discover
	// itself reports for a missing root, kept here so sum.Sources still
	// names every known source (spec session-index, "Some agents are not
	// installed").
	for _, a := range r.adapters() {
		installs := r.installsFor(a.Name())
		if len(installs) == 0 {
			status := SourceStatus{Source: a.Name()}
			_, derr := a.Discover(profile.Profile{})
			status.Err = derr
			sum.Sources = append(sum.Sources, status)
			continue
		}

		for _, p := range installs {
			status := SourceStatus{Source: a.Name(), Install: p.Name}
			discovered, derr := a.Discover(p)
			if derr != nil {
				status.Available = false
				status.Err = derr
				sum.Sources = append(sum.Sources, status)
				continue
			}
			status.Available = true

			bySourceID := map[string]session.Session{}
			for _, d := range discovered {
				bySourceID[d.Session.SourceSessionID] = d.Session
			}

			for _, d := range discovered {
				s := d.Session
				rawID := d.Session.ID // adapter.Discover's own composite id, before any correction below - the fallbackPrompts key tier2ForSource looks up by (it walks the same discovered slice, never the corrected s)
				prior := existing.priorFor(s.ID, d.TranscriptPath)

				replaced := false
				if d.TranscriptPath != "" {
					var cursorRow map[string]any
					var prompts []transcript.PromptText
					s, cursorRow, prompts, replaced = r.applyTranscript(s, d, prior, cursors[cursorKey(a.Name(), s.SourceSessionID)])
					cursorRecords = append(cursorRecords, cursorRow)
					if len(prompts) > 0 {
						fallbackPrompts[rawID] = prompts
					}
				}
				// hermes (TranscriptPath == ""): fully re-derived from its own
				// DB every pass by the adapter already - hermes is
				// authoritative about its own end state, nothing more to layer
				// on here.

				// LazyRecall's own composite id is "<source>:<install>:<source
				// session id>" (session.Session.ID doc comment) - recomputed
				// here now that applyTranscript may have corrected
				// SourceSessionID from the file-derived value adapter.Discover
				// supplied to the source's own recorded identifier (change
				// fix-resume-session-identity, design.md decision 1). Everything
				// downstream - the DB primary key, the lineage root, and the
				// resume path's cmd/lazyrecall/main.go, which recovers
				// SourceSessionID by splitting this same composite id - must see
				// the corrected value consistently.
				s.ID = s.Source + ":" + p.Name + ":" + s.SourceSessionID
				s.Profile = p.Name // written out as sessions.install (sessionToRecord)

				// dir_exists: checked opportunistically here so listings never
				// need to stat the filesystem themselves (spec session-search,
				// "Missing working directories are marked").
				if s.CWD != nil {
					exists := dirExists(*s.CWD)
					s.DirExists = &exists
				}

				// Git identity (repo root + the canonical root worktrees
				// share) is resolved once and then cached via the prior-row merge
				// below - it costs a git subprocess call only the first time a
				// session's directory is seen to exist and nothing is known yet
				// (spec session-search, "Grouping by repository and worktree").
				// A replaced transcript is not eligible for that cache: its CWD
				// evidence was re-derived from new bytes, so the old CWD's
				// repository identity is stale rather than a useful fallback.
				if prior != nil && !replaced {
					if s.GitRepoRoot == nil {
						s.GitRepoRoot = prior.GitRepoRoot
					}
					if s.GitCommonRoot == nil {
						s.GitCommonRoot = prior.GitCommonRoot
					}
				}
				switch {
				case s.GitCommonRoot != nil:
					// Already known, either just merged forward above or
					// supplied directly by the source (hermes) - nothing left
					// to ask git.
					s.GitResolved = true
				case prior != nil && !replaced && prior.GitResolved && samePtrString(prior.CWD, s.CWD) && !dirNewlyExists(prior, s.DirExists):
					// Same CWD as last time, and a prior pass already asked
					// git about it and came up with nothing further
					// (git_resolved, schema v9). The two root columns alone
					// can't tell "never asked" from "asked, and this really
					// isn't a git repository" - both read back nil - so
					// without this a non-repo CWD re-ran git on every single
					// session, on every single refresh, forever: 95 calls a
					// pass for one real directory, measured in this fix
					// (devdocs/fyi.md). Git state changes rarely enough that
					// today's rule already accepts this same staleness for a
					// directory that *is* a repo (the merge above); trusting
					// a recorded "no repo" for one whose CWD and dir-exists
					// state have not changed is the same bet.
					s.GitResolved = true
				case s.CWD != nil && s.DirExists != nil && *s.DirExists:
					// Ask git at most once per distinct CWD in this whole
					// pass (gitMemo), not once per session in that CWD.
					if info, ok := r.gitResolveCached(gitMemo, *s.CWD); ok {
						if s.GitRepoRoot == nil {
							// Only fill this in if the source didn't already
							// record it (hermes does) - never substitute a
							// derived value for one the source provided.
							s.GitRepoRoot = &info.RepoRoot
						}
						s.GitCommonRoot = &info.CommonRoot
					}
					s.GitResolved = true
				default:
					// No known CWD, or its directory doesn't exist (yet):
					// leave GitResolved false, so a later pass tries again
					// once the directory appears or a CWD becomes known,
					// rather than caching a determination that was never
					// actually made.
				}

				lineageID := resolveLineage(a.Name(), p.Name, s, bySourceID)
				s.LineageID = lineageID
				observedLineages[lineageID] = true

				sessionRecords = append(sessionRecords, sessionToRecord(s))
				status.Sessions++
			}

			sum.Sources = append(sum.Sources, status)

			// Tier 2: each source's own prompt index first, transcript
			// extraction as the fallback for whatever it doesn't cover (task
			// 6.4).
			prompts, newCursorRows, terr := r.tier2ForSource(a.Name(), p, cursors, discovered, fallbackPrompts)
			if terr == nil {
				promptRecords = append(promptRecords, prompts...)
				cursorRecords = append(cursorRecords, newCursorRows...)
			}
		}
	}

	if err := r.write(sessionRecords, promptRecords, cursorRecords); err != nil {
		return sum, err
	}
	if err := r.migrateLineageAnnotations(oldLineageByTranscript, sessionRecords); err != nil {
		return sum, err
	}
	if err := r.reconcileLineages(observedLineages); err != nil {
		return sum, err
	}
	return sum, nil
}

// gitMemoEntry is one distinct CWD's git identity, resolved at most once
// per refresh pass (gitResolveCached's memo) rather than once per session -
// the per-pass half of the fix; git_resolved, folded forward through
// existingSessions/session.Session, is the cross-pass half (devdocs/fyi.md).
type gitMemoEntry struct {
	info gitutil.Info
	ok   bool
}

// gitResolveCached resolves dir's git identity through r.GitResolve
// (defaulting to gitutil.Resolve), consulting and populating memo first so
// that every session sharing a CWD within one pass - 95 of them, for one
// real, non-repo directory measured in this fix - costs at most one git
// subprocess call instead of one per session.
func (r *Refresher) gitResolveCached(memo map[string]gitMemoEntry, dir string) (gitutil.Info, bool) {
	if e, ok := memo[dir]; ok {
		return e.info, e.ok
	}
	resolve := gitutil.Resolve
	if r.GitResolve != nil {
		resolve = r.GitResolve
	}
	info, ok := resolve(dir)
	memo[dir] = gitMemoEntry{info: info, ok: ok}
	return info, ok
}

// samePtrString reports whether two optional strings hold the same value:
// both nil, or both non-nil and equal.
func samePtrString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// dirNewlyExists reports whether curExists says a directory exists now that
// prior's own last check did not (nil counts as "didn't"). A prior "not a
// git repository" result must not be trusted forward across that
// transition: the directory could be a freshly created repo, and prior's
// git_resolved says only that git was asked about *some* state of this CWD,
// not this one.
func dirNewlyExists(prior *session.Session, curExists *bool) bool {
	now := curExists != nil && *curExists
	was := prior != nil && prior.DirExists != nil && *prior.DirExists
	return now && !was
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func cursorKey(source, sourceSessionID string) string { return source + "\x00" + sourceSessionID }

// leadingBytesHash summarizes the first bytes of a file so rewrite
// detection (task 6.2) doesn't have to keep the raw bytes around.
func leadingBytesHash(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && read == 0 {
		return "", nil
	}
	sum := sha256.Sum256(buf[:read])
	return hex.EncodeToString(sum[:]), nil
}

func sessionToRecord(s session.Session) map[string]any {
	rec := map[string]any{
		"id":                s.ID,
		"source":            s.Source,
		"source_session_id": s.SourceSessionID,
		"lineage_id":        s.LineageID,
		"install":           s.Profile,
		"end_state":         string(s.EndState),
		"origin":            string(s.Origin),
		"human_prompt":      boolToInt(s.HumanPrompt),
		"resumable":         boolToInt(s.Resumable),
		"git_resolved":      boolToInt(s.GitResolved),
	}
	if s.ContinuesFrom != nil {
		rec["continues_from"] = *s.ContinuesFrom
	}
	if s.Client != nil {
		rec["client"] = *s.Client
	}
	if s.Topic != nil {
		rec["topic"] = *s.Topic
	}
	if s.Name != nil {
		rec["name"] = *s.Name
	}
	if s.LastPrompt != nil {
		rec["last_prompt"] = *s.LastPrompt
	}
	if s.CWD != nil {
		rec["cwd"] = *s.CWD
	}
	if s.GitBranch != nil {
		rec["git_branch"] = *s.GitBranch
	}
	if s.GitRepoRoot != nil {
		rec["git_repo_root"] = *s.GitRepoRoot
	}
	if s.GitCommonRoot != nil {
		rec["git_common_root"] = *s.GitCommonRoot
	}
	if s.StartedAt != nil {
		rec["started_at"] = s.StartedAt.Unix()
	}
	if s.LastActivityAt != nil {
		rec["last_activity_at"] = s.LastActivityAt.Unix()
	}
	if s.Compaction != nil {
		rec["compaction_count"] = s.Compaction.Count
		if b, err := json.Marshal(s.Compaction.Events); err == nil {
			rec["compaction_json"] = string(b)
		}
	}
	if s.TranscriptPath != nil {
		rec["transcript_path"] = *s.TranscriptPath
	}
	if s.MessageCount != nil {
		rec["message_count"] = *s.MessageCount
	}
	if s.DirExists != nil {
		rec["dir_exists"] = boolToInt(*s.DirExists)
	}
	return rec
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ConversationReaderFor returns the reader for a source that keeps its
// conversations in a database rather than in transcript files, so a display
// caller can show the exchange without knowing which sources those are.
//
// It lives here rather than in the display layer because this package
// already owns the list of adapters that exist; asking it is what keeps a
// new database-backed source from having to be registered in two places.
// The second return is false for the file-backed sources - whose
// conversations internal/transcript reads instead - and for antigravity,
// whose per-conversation detail cannot be decoded at all.
func ConversationReaderFor(source, sqlite3Path string) (adapter.ConversationReader, bool) {
	r := &Refresher{SQLite3Path: sqlite3Path}
	for _, a := range r.adapters() {
		if a.Name() != source {
			continue
		}
		cr, ok := a.(adapter.ConversationReader)
		return cr, ok
	}
	return nil, false
}
