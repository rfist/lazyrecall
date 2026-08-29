package refresh

import (
	"encoding/json"
	"time"

	"lazyrecall/internal/adapter"
	"lazyrecall/internal/session"
	"lazyrecall/internal/transcript"
)

// cursorRow is one row from LazyRecall's own cursors table.
type cursorRow struct {
	Source       string  `json:"source"`
	SourceID     string  `json:"source_id"`
	Kind         string  `json:"kind"`
	ByteOffset   *int64  `json:"byte_offset"`
	FileSize     *int64  `json:"file_size"`
	LeadingBytes *string `json:"leading_bytes"`
	DBCursorKey  *string `json:"db_cursor_key"`
}

func (r *Refresher) loadCursors() (map[string]cursorRow, error) {
	var rows []cursorRow
	if err := r.DB.Query(`SELECT source, source_id, kind, byte_offset, file_size, leading_bytes, db_cursor_key FROM cursors;`, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]cursorRow, len(rows))
	for _, row := range rows {
		out[cursorKey(row.Source, row.SourceID)] = row
	}
	return out, nil
}

// loadLineagesByTranscript snapshots every current session's (transcript
// path, lineage id) pair, keyed by the stable transcript path rather than
// the composite session id a full rebuild is about to discard and
// recompute. Called only before a full rebuild clears the sessions table
// (change fix-resume-session-identity, task 4.2) so that a lineage id that
// changes because this rebuild corrected a SourceSessionID can still be
// traced back to the lineage id its comments/tags were filed under.
func (r *Refresher) loadLineagesByTranscript() (map[string]string, error) {
	var rows []struct {
		TranscriptPath *string `json:"transcript_path"`
		LineageID      string  `json:"lineage_id"`
	}
	if err := r.DB.Query(`SELECT transcript_path, lineage_id FROM sessions WHERE transcript_path IS NOT NULL AND transcript_path != '';`, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.TranscriptPath != nil && *row.TranscriptPath != "" {
			out[*row.TranscriptPath] = row.LineageID
		}
	}
	return out, nil
}

// existingSessionRow mirrors enough of the sessions table to let a refresh
// merge new scan results with what is already known, so a value found in
// an earlier pass (e.g. a topic recorded once near the start of a long
// transcript) is not lost just because it falls outside this pass's delta.
type existingSessionRow struct {
	ID              string  `json:"id"`
	SourceSessionID string  `json:"source_session_id"`
	TranscriptPath  *string `json:"transcript_path"`
	Topic           *string `json:"topic"`
	Name            *string `json:"name"`
	LastPrompt      *string `json:"last_prompt"`
	CWD             *string `json:"cwd"`
	GitBranch       *string `json:"git_branch"`
	GitRepoRoot     *string `json:"git_repo_root"`
	GitCommonRoot   *string `json:"git_common_root"`
	EndState        string  `json:"end_state"`
	Origin          string  `json:"origin"`
	HumanPrompt     int64   `json:"human_prompt"`
	Client          *string `json:"client"`
	CompactionCount *int64  `json:"compaction_count"`
	CompactionJSON  *string `json:"compaction_json"`
	MessageCount    *int64  `json:"message_count"`
	LastActivityAt  *int64  `json:"last_activity_at"`
}

// existingSessions is the previous refresh pass's rows, indexed two ways.
// ByID matches how sessions were always looked up: keyed on LazyRecall's own
// composite id, which adapter.Discover computes fresh every pass. ByTranscript
// is the fallback keyed on the transcript's file path, which stays constant
// even across the one pass where a session's SourceSessionID (and therefore
// its composite id) changes because this pass is the first to read the
// source's own recorded identifier instead of a value derived from the file
// name (change fix-resume-session-identity, design.md decision 1) - without
// this fallback, that one pass (and, since adapter.Discover can never itself
// learn the corrected id, every pass after it too) would find no prior row
// for an affected pi/omp session and silently drop its topic/cwd/message
// count/etc. back to unknown.
type existingSessions struct {
	ByID         map[string]*session.Session
	ByTranscript map[string]*session.Session
}

func (r *Refresher) loadExistingSessions() (existingSessions, error) {
	var rows []existingSessionRow
	if err := r.DB.Query(`SELECT id, source_session_id, transcript_path, topic, name, last_prompt, cwd, git_branch, git_repo_root, git_common_root, end_state, origin, human_prompt, client, compaction_count, compaction_json, message_count, last_activity_at FROM sessions;`, &rows); err != nil {
		return existingSessions{}, err
	}
	out := existingSessions{
		ByID:         make(map[string]*session.Session, len(rows)),
		ByTranscript: make(map[string]*session.Session, len(rows)),
	}
	for _, row := range rows {
		// Normalize on the way in, same closed-set rule as the write
		// boundary: a stored value that is not one of the known origins
		// (an older build, or a hand-edited row) must never be merged back
		// into a live session and re-persisted as-is.
		origin := session.Origin(row.Origin)
		if !origin.Valid() {
			origin = session.OriginUnknown
		}
		s := &session.Session{
			ID:              row.ID,
			SourceSessionID: row.SourceSessionID,
			Topic:           row.Topic,
			Name:            row.Name,
			LastPrompt:      row.LastPrompt,
			CWD:             row.CWD,
			GitBranch:       row.GitBranch,
			GitRepoRoot:     row.GitRepoRoot,
			GitCommonRoot:   row.GitCommonRoot,
			EndState:        session.EndState(row.EndState),
			Origin:          origin,
			HumanPrompt:     row.HumanPrompt != 0,
			Client:          row.Client,
		}
		if row.MessageCount != nil {
			mc := *row.MessageCount
			s.MessageCount = &mc
		}
		if row.LastActivityAt != nil {
			t := time.Unix(*row.LastActivityAt, 0)
			s.LastActivityAt = &t
		}
		if row.CompactionCount != nil {
			s.Compaction = &session.Compaction{Count: int(*row.CompactionCount)}
			if row.CompactionJSON != nil && *row.CompactionJSON != "" {
				var events []session.CompactionEvent
				_ = json.Unmarshal([]byte(*row.CompactionJSON), &events)
				s.Compaction.Events = events
			}
		}
		out.ByID[row.ID] = s
		if row.TranscriptPath != nil && *row.TranscriptPath != "" {
			out.ByTranscript[*row.TranscriptPath] = s
		}
	}
	return out, nil
}

// priorFor resolves the previous pass's row for a newly discovered session:
// preferably by the composite id adapter.Discover just computed, falling
// back to the stable transcript path when that id doesn't match anything -
// which is expected, not an error, on the one pass a source's SourceSessionID
// is corrected and on every pass after it (see existingSessions doc above).
func (e existingSessions) priorFor(id, transcriptPath string) *session.Session {
	if p, ok := e.ByID[id]; ok {
		return p
	}
	if transcriptPath == "" {
		return nil
	}
	return e.ByTranscript[transcriptPath]
}

// applyTranscript incrementally scans d's transcript, merges the result
// into s (falling back to prior's already-known values for anything this
// pass's delta didn't touch), and returns the updated session, the cursor
// row to persist, and the raw prompts extracted (for tier 2 fallback).
func (r *Refresher) applyTranscript(s session.Session, d adapter.Discovered, prior *session.Session, cur cursorRow) (session.Session, map[string]any, []transcript.PromptText) {
	// The cursor key must stay whatever adapter.Discover computed this
	// session's SourceSessionID to be - the same value refresh.go used to
	// look cur up in the first place - regardless of what this function may
	// go on to correct SourceSessionID to below. Using the corrected value
	// here instead would write the cursor under a key next pass's lookup
	// (which again starts from adapter.Discover's raw value) can never find,
	// forcing a full re-scan, and a doubled MessageCount, on every pass
	// forever (change fix-resume-session-identity).
	cursorSourceID := s.SourceSessionID

	fromOffset := int64(0)
	if cur.ByteOffset != nil {
		fromOffset = *cur.ByteOffset
		if sizeShrank(cur, d.FileSize) || leadingBytesChanged(cur, d.TranscriptPath) {
			fromOffset = 0 // task 6.2: rewrite detected, re-read in full
		}
	}

	vocab, ok := transcript.VocabFor(s.Source)
	if !ok {
		return s, nil, nil
	}

	res, err := transcript.Scan(d.TranscriptPath, fromOffset, vocab)
	if err != nil {
		// Can't read it this pass (e.g. transient permission error, or the
		// agent rewrote it between stat and open). Leave everything as it
		// was and retry next refresh - never fail the whole run over one
		// session (task 5.6).
		if prior != nil {
			mergePrior(&s, prior)
		}
		s.TranscriptPath = &d.TranscriptPath
		return s, cursorRecord(s.Source, cursorSourceID, fromOffset, d.FileSize, ""), nil
	}

	if res.CWD != nil {
		s.CWD = res.CWD
	} else if prior != nil {
		s.CWD = prior.CWD
	}
	if res.GitBranch != nil {
		s.GitBranch = res.GitBranch
	} else if prior != nil {
		s.GitBranch = prior.GitBranch
	}
	if res.Topic != nil {
		s.Topic = res.Topic
	} else if prior != nil {
		s.Topic = prior.Topic
	}
	if res.CustomTitle != nil {
		s.Name = res.CustomTitle
	} else if prior != nil {
		s.Name = prior.Name
	}
	if res.LastPrompt != nil {
		s.LastPrompt = res.LastPrompt
	} else if prior != nil {
		s.LastPrompt = prior.LastPrompt
	}

	// HumanPrompt is sticky: true the moment any scan - this one or an
	// earlier one, via prior - ever saw a prompt the source attributed to
	// a person, and never cleared once set. It is stored and folded
	// forward rather than recomputed from a *previously stored Origin*
	// (the pre-fix rule this replaces): Origin also defaults to
	// interactive for entrypoints this build has never classified as
	// automated (claudeOrigin's allowlist-of-automated fallback, e.g.
	// plain "cli"), so a rule keyed on "was the prior row already
	// Interactive" could not distinguish a person's transcript from a
	// terminal session that simply hadn't been touched by automation yet
	// - and so kept the terminal session "interactive" forever after
	// automation started appending to it, while a --full rebuild, folding
	// every record fresh with no human marker anywhere, correctly called
	// it automated (audit finding on 9feca0a: incremental and full
	// rebuild disagreed about the same session). Persisting the actual
	// evidence - a human-marked prompt - instead of a derived conclusion
	// makes the two paths agree by construction: a full rebuild sees
	// every record in one pass, so HumanPrompt comes out true whenever any
	// prompt anywhere in the transcript was human, exactly what folding
	// this stored flag forward across incremental passes also converges
	// to.
	s.HumanPrompt = res.HumanPrompt || (prior != nil && prior.HumanPrompt)

	// Origin: this pass's scan either named one (the strongest signal any
	// of its records carried - transcript.Scan already folded them, so one
	// automated record decides), or a prior pass did, or the session stays
	// unknown (sources that record no entrypoint field, or a delta whose
	// records never carried one - the field rides the first record, so an
	// incremental scan past it sees it only via prior).
	if res.Origin != session.OriginUnknown {
		s.Origin = res.Origin
	} else if prior != nil {
		s.Origin = prior.Origin
	}
	// A stored human prompt outranks whatever the entrypoint says, full
	// stop. This is deliberately keyed on HumanPrompt above, not on
	// whatever Origin was just derived to two paragraphs up - see
	// HumanPrompt's own comment for why that distinction is the entire
	// fix: a later delta of an editor-client chat carries only the
	// agent's own records, all stamped with the SDK entrypoint, and would
	// otherwise re-hide a conversation the user is still having (change
	// show-editor-clients). Nothing legitimately turns from a person
	// typing into a script, so once true this can only ever push Origin
	// toward Interactive, never away from it.
	if s.HumanPrompt {
		s.Origin = session.OriginInteractive
	}

	// Client: first-known wins, never the latest. The client answers
	// "where was this conversation held", and a conversation that
	// migrates between frontends - an editor chat later resumed in the
	// source's own terminal, say - is best identified by where it
	// started, not by wherever the most recent incremental delta happens
	// to have been driven through. The rule this replaces ("res.Client
	// when this pass has one, else prior") claimed a delta "either sees
	// [the client] throughout or not at all" - true of any *one* pass in
	// isolation, but false of the session across passes whenever it
	// resumes from a different frontend, and it let a later delta's
	// client silently overwrite an earlier, already-known one: an
	// incremental refresh would show wherever the session was most
	// recently touched, while a --full rebuild - which always starts its
	// single scan at the transcript's first byte - would show wherever it
	// started, the same disagreement-by-refresh-path as the origin bug
	// above (audit finding on 9feca0a). A client already known, from
	// prior, is therefore never replaced; only a session with no client
	// yet takes the one this pass found.
	if prior != nil && prior.Client != nil {
		s.Client = prior.Client
	} else if res.Client != nil {
		s.Client = res.Client
	}

	// The source's own recorded identifier, when this pass's delta included
	// it (only ever the transcript's first line, so only a from-scratch or
	// full-rebuild pass actually sees it) or a prior pass already recorded
	// it. Falls back to whatever adapter.Discover set (task 1.3: sources
	// that record no identifier of their own, or a pass that has not yet
	// read the record carrying it, keep the file-derived value - a known,
	// recorded weakness rather than an assumption).
	if res.SourceID != nil && *res.SourceID != "" {
		s.SourceSessionID = *res.SourceID
	} else if prior != nil && prior.SourceSessionID != "" {
		s.SourceSessionID = prior.SourceSessionID
	}

	sourceSupportsCompaction := s.Source == "claude" || s.Source == "omp"
	if sourceSupportsCompaction {
		var events []session.CompactionEvent
		if prior != nil && prior.Compaction != nil {
			events = append(events, prior.Compaction.Events...)
		}
		for _, ci := range res.Compactions {
			events = append(events, session.CompactionEvent{Automatic: ci.Automatic, DroppedTokens: ci.DroppedTokens})
		}
		s.Compaction = &session.Compaction{Count: len(events), Events: events}
	}

	priorCount := int64(0)
	if prior != nil && prior.MessageCount != nil {
		priorCount = *prior.MessageCount
	}
	total := priorCount + res.RecordCount
	s.MessageCount = &total

	if len(res.Tail) > 0 {
		lastTS := lastTimestamp(res.Tail)
		if lastTS != nil {
			s.LastActivityAt = lastTS
		}
		inactive := s.LastActivityAt != nil && r.Now().Sub(*s.LastActivityAt) > InactivityThreshold
		s.EndState = transcript.ClassifyEndState(res.Tail, inactive)
	} else if prior != nil {
		s.EndState = prior.EndState
		s.LastActivityAt = prior.LastActivityAt
	} else {
		s.EndState = session.EndStateUnknown
	}

	s.TranscriptPath = &d.TranscriptPath

	hash, _ := leadingBytesHash(d.TranscriptPath, 256)
	return s, cursorRecord(s.Source, cursorSourceID, res.EndOffset, d.FileSize, hash), res.Prompts
}

func mergePrior(s *session.Session, prior *session.Session) {
	if s.Topic == nil {
		s.Topic = prior.Topic
	}
	if s.Name == nil {
		s.Name = prior.Name
	}
	if s.LastPrompt == nil {
		s.LastPrompt = prior.LastPrompt
	}
	if s.CWD == nil {
		s.CWD = prior.CWD
	}
	if s.GitBranch == nil {
		s.GitBranch = prior.GitBranch
	}
	if prior.SourceSessionID != "" {
		s.SourceSessionID = prior.SourceSessionID
	}
	if s.Client == nil {
		s.Client = prior.Client
	}
	s.EndState = prior.EndState
	s.Origin = prior.Origin
	s.HumanPrompt = prior.HumanPrompt
	s.Compaction = prior.Compaction
	s.MessageCount = prior.MessageCount
	s.LastActivityAt = prior.LastActivityAt
}

// sizeShrank is the cheap first rewrite check (task 6.2): a file that is
// now smaller than it was at the last cursor cannot possibly be the same
// file, append-only.
func sizeShrank(cur cursorRow, currentSize int64) bool {
	return cur.FileSize != nil && currentSize < *cur.FileSize
}

// leadingBytesChanged is the second rewrite check: even a same-or-larger
// file may have been rewritten from scratch (e.g. a source compacting its
// own transcript in place). Comparing a hash of the first bytes catches
// that without hashing the whole file on every refresh.
func leadingBytesChanged(cur cursorRow, path string) bool {
	if cur.LeadingBytes == nil || *cur.LeadingBytes == "" {
		return false // no prior hash recorded (e.g. this session predates the check): nothing to compare against
	}
	hash, err := leadingBytesHash(path, 256)
	if err != nil {
		return false // can't read it, let Scan itself surface the error
	}
	return hash != *cur.LeadingBytes
}

func lastTimestamp(tail []transcript.Record) *time.Time {
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i].Timestamp != nil {
			return tail[i].Timestamp
		}
	}
	return nil
}

func cursorRecord(source, sourceID string, byteOffset, fileSize int64, leadingBytes string) map[string]any {
	rec := map[string]any{
		"source":      source,
		"source_id":   sourceID,
		"kind":        "transcript_offset",
		"byte_offset": byteOffset,
		"file_size":   fileSize,
		"updated_at":  time.Now().Unix(),
	}
	if leadingBytes != "" {
		rec["leading_bytes"] = leadingBytes
	}
	return rec
}

// resolveLineage walks a session's continuation chain, within the sessions
// this same source just discovered, to the root and derives a stable
// lineage id from it (design.md decision 8; task 9.3).
func resolveLineage(sourceName, profileName string, s session.Session, bySourceID map[string]session.Session) string {
	cur := s
	visited := map[string]bool{}
	for cur.ContinuesFrom != nil && !visited[cur.SourceSessionID] {
		visited[cur.SourceSessionID] = true
		parent, ok := bySourceID[*cur.ContinuesFrom]
		if !ok {
			break
		}
		cur = parent
	}
	return session.LineageID(sourceName, profileName, cur.SourceSessionID)
}
