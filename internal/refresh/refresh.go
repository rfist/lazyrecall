// Package refresh is the orchestrator: it calls every adapter for the
// active profile, drives the transcript reader incrementally, ingests each
// source's tier-2 prompt index, writes everything to LazyRecall's own database
// through sqlitex, and keeps lineages/orphans up to date. It is what "every
// operation refreshes the index before answering" (design.md decision 6)
// actually runs.
package refresh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lazyrecall/internal/adapter"
	claudeadapter "lazyrecall/internal/adapter/claude"
	hermesadapter "lazyrecall/internal/adapter/hermes"
	ompadapter "lazyrecall/internal/adapter/omp"
	piadapter "lazyrecall/internal/adapter/pi"
	"lazyrecall/internal/gitutil"
	"lazyrecall/internal/profile"
	"lazyrecall/internal/schema"
	"lazyrecall/internal/session"
	"lazyrecall/internal/sqlitex"
	"lazyrecall/internal/transcript"
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

// SourceStatus reports one source's outcome for this refresh, so a caller
// can show "which agents are available" (spec session-index, "Source
// discovery").
type SourceStatus struct {
	Source    string
	Available bool
	Sessions  int
	Err       error
}

// Summary is what one refresh pass produced.
type Summary struct {
	Profile profile.Profile
	Sources []SourceStatus
}

// Refresher runs refresh passes for one profile against one LazyRecall
// database.
type Refresher struct {
	Profile     profile.Profile
	DB          *sqlitex.Runner // read-write, LazyRecall's own database
	SQLite3Path string
	Now         func() time.Time
}

// migrateLegacyDataDir moves a pre-rename ~/.recall to the current data
// directory the first time this version runs (change rename-to-lazyrecall).
// Everything in there - the per-profile databases, and with them the short
// handles, comments, and tags that are LazyRecall's own data and cannot be
// re-derived from any source - would otherwise be orphaned by the rename.
//
// It runs only when the new location does not exist yet, so it can never
// overwrite a live database, and only for the default location: a caller
// that has set LAZYRECALL_HOME or RECALL_HOME has said where its data is,
// and moving something else on top of that would be the opposite of what
// it asked for.
//
// A failure here is reported and then ignored rather than returned. The
// index is a cache of what the sources already hold; the worst case is a
// rebuild on the next refresh, which is not worth refusing to start over.
// (The annotations are the part that cannot be rebuilt - which is why the
// old directory is left untouched on failure, for a later attempt or a
// manual move, instead of being half-moved.)
func migrateLegacyDataDir() {
	if os.Getenv("LAZYRECALL_HOME") != "" || os.Getenv("RECALL_HOME") != "" {
		return
	}
	newDir, oldDir := profile.DataDir(), profile.LegacyDataDir()
	if newDir == oldDir {
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		return
	}
	if fi, err := os.Stat(oldDir); err != nil || !fi.IsDir() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lazyrecall: could not prepare %s (%v); leaving %s in place\n", newDir, err, oldDir)
		return
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		fmt.Fprintf(os.Stderr, "lazyrecall: could not move %s to %s (%v); starting a fresh index\n", oldDir, newDir, err)
		return
	}
	fmt.Fprintf(os.Stderr, "lazyrecall: moved %s to %s\n", oldDir, newDir)
}

// New opens (creating and migrating if needed) the database for p and
// returns a Refresher for it.
func New(p profile.Profile, sqlite3Path string) (*Refresher, error) {
	migrateLegacyDataDir()
	dbPath := profile.DBPath(p)
	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		return nil, fmt.Errorf("refresh: creating %s: %w", profile.DataDir(), err)
	}
	db := &sqlitex.Runner{BinPath: sqlite3Path, DBPath: dbPath}
	if _, err := schema.Open(db); err != nil {
		return nil, fmt.Errorf("refresh: opening database for profile %s: %w", p.Name, err)
	}
	return &Refresher{Profile: p, DB: db, SQLite3Path: sqlite3Path, Now: time.Now}, nil
}

func (r *Refresher) adapters() []adapter.Adapter {
	return []adapter.Adapter{
		claudeadapter.New(),
		piadapter.New(),
		ompadapter.New(r.SQLite3Path),
		hermesadapter.New(r.SQLite3Path),
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

	sum := Summary{Profile: r.Profile}
	var sessionRecords []map[string]any
	var promptRecords []map[string]any
	var cursorRecords []map[string]any
	observedLineages := map[string]bool{}
	fallbackPrompts := map[string][]transcript.PromptText{} // adapter.Discover's raw session id -> transcript-extracted prompts

	// hermes sessions are collected first so claude/pi/omp lineage linking
	// (task 9.3, "each source's continuation and fork markers") has no
	// cross-source dependency to worry about - every source resolves its
	// own continuation chain independently.
	for _, a := range r.adapters() {
		status := SourceStatus{Source: a.Name()}
		discovered, derr := a.Discover(r.Profile)
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

			if d.TranscriptPath != "" {
				var cursorRow map[string]any
				var prompts []transcript.PromptText
				s, cursorRow, prompts = r.applyTranscript(s, d, prior, cursors[cursorKey(a.Name(), s.SourceSessionID)])
				cursorRecords = append(cursorRecords, cursorRow)
				if len(prompts) > 0 {
					fallbackPrompts[rawID] = prompts
				}
			}
			// hermes (TranscriptPath == ""): fully re-derived from its own
			// DB every pass by the adapter already - hermes is
			// authoritative about its own end state, nothing more to layer
			// on here.

			// LazyRecall's own composite id is "<source>:<profile>:<source
			// session id>" (session.Session.ID doc comment) - recomputed
			// here now that applyTranscript may have corrected
			// SourceSessionID from the file-derived value adapter.Discover
			// supplied to the source's own recorded identifier (change
			// fix-resume-session-identity, design.md decision 1). Everything
			// downstream - the DB primary key, the lineage root, and the
			// resume path's cmd/lazyrecall/main.go, which recovers
			// SourceSessionID by splitting this same composite id - must see
			// the corrected value consistently.
			s.ID = s.Source + ":" + r.Profile.Name + ":" + s.SourceSessionID

			// dir_exists: checked opportunistically here so listings never
			// need to stat the filesystem themselves (spec session-search,
			// "Missing working directories are marked").
			if s.CWD != nil {
				exists := dirExists(*s.CWD)
				s.DirExists = &exists
			}

			// Git identity (repo root + the canonical root worktrees
			// share) is resolved once and then cached forever via the
			// prior-row merge below - it costs a git subprocess call only
			// the first time a session's directory is seen to exist and
			// nothing is known yet (spec session-search, "Grouping by
			// repository and worktree").
			if prior != nil {
				if s.GitRepoRoot == nil {
					s.GitRepoRoot = prior.GitRepoRoot
				}
				if s.GitCommonRoot == nil {
					s.GitCommonRoot = prior.GitCommonRoot
				}
			}
			if s.GitCommonRoot == nil && s.CWD != nil && s.DirExists != nil && *s.DirExists {
				if info, ok := gitutil.Resolve(*s.CWD); ok {
					if s.GitRepoRoot == nil {
						// Only fill this in if the source didn't already
						// record it (hermes does) - never substitute a
						// derived value for one the source provided.
						s.GitRepoRoot = &info.RepoRoot
					}
					s.GitCommonRoot = &info.CommonRoot
				}
			}

			lineageID := resolveLineage(a.Name(), r.Profile.Name, s, bySourceID)
			s.LineageID = lineageID
			observedLineages[lineageID] = true

			sessionRecords = append(sessionRecords, sessionToRecord(s))
			status.Sessions++
		}

		sum.Sources = append(sum.Sources, status)

		// Tier 2: each source's own prompt index first, transcript
		// extraction as the fallback for whatever it doesn't cover (task
		// 6.4).
		prompts, newCursorRows, terr := r.tier2ForSource(a.Name(), cursors, discovered, fallbackPrompts)
		if terr == nil {
			promptRecords = append(promptRecords, prompts...)
			cursorRecords = append(cursorRecords, newCursorRows...)
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
		"end_state":         string(s.EndState),
		"resumable":         boolToInt(s.Resumable),
	}
	if s.ContinuesFrom != nil {
		rec["continues_from"] = *s.ContinuesFrom
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
