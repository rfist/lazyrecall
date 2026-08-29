package refresh

import (
	"fmt"
	"sort"

	"lazyrecall/internal/session"
)

var sessionColumns = []string{
	"id", "source", "source_session_id", "lineage_id", "continues_from",
	"topic", "name", "last_prompt", "cwd", "git_branch", "git_repo_root", "git_common_root",
	"started_at", "last_activity_at", "end_state", "origin", "client", "compaction_count",
	"compaction_json", "transcript_path", "message_count", "resumable", "dir_exists",
}

var cursorColumns = []string{
	"source", "source_id", "kind", "byte_offset", "file_size", "leading_bytes", "db_cursor_key", "updated_at",
}

// write commits everything one refresh pass produced in a single
// transaction and a single sqlite3 subprocess (task 2.1). Every value that
// originated outside this program (session text, cwd strings, prompt text)
// travels through sqlitex's NDJSON bulk-load path - never through a
// formatted SQL string.
func (r *Refresher) write(sessionRecords, promptRecords, cursorRecords []map[string]any) error {
	lineageRecords := lineageRecordsFrom(sessionRecords, r.Profile.Name)

	// origin is a closed set (session.Origin): normalize every record here,
	// at the write boundary, so a session whose origin was never determined
	// - or a record built by a path that never merged one - is stored as
	// "unknown", never as an empty string or NULL. A hand-edited or
	// older-build row already in the database is the read path's problem,
	// not a write here.
	for _, rec := range sessionRecords {
		rec["origin"] = originValue(rec)
	}

	handleRecords, err := r.allocateHandles(lineageRecords)
	if err != nil {
		return fmt.Errorf("refresh: allocating lineage handles: %w", err)
	}

	b := r.DB.NewBatch()

	// "id", "profile", "orphaned" only - handle is deliberately never part
	// of this upsert's column list, so ON CONFLICT never touches a handle
	// already assigned (design.md decision 3, "Handles are never reused":
	// once set, a handle is only ever written once, by the BulkUpdate
	// below).
	if err := b.BulkUpsert("lineages", []string{"id"}, []string{"id", "profile", "orphaned"}, lineageRecords); err != nil {
		return fmt.Errorf("refresh: preparing lineages: %w", err)
	}
	if len(handleRecords) > 0 {
		if err := b.BulkUpdate("lineages", "id", []string{"id", "handle"}, handleRecords); err != nil {
			return fmt.Errorf("refresh: preparing lineage handles: %w", err)
		}
	}
	if err := b.BulkUpsert("sessions", []string{"id"}, sessionColumns, sessionRecords); err != nil {
		return fmt.Errorf("refresh: preparing sessions: %w", err)
	}
	if err := b.BulkInsert("prompt_fts", []string{"session_id", "kind", "text"}, promptRecords); err != nil {
		return fmt.Errorf("refresh: preparing prompt index: %w", err)
	}
	if err := b.BulkUpsert("cursors", []string{"source", "source_id"}, cursorColumns, dedupeCursors(cursorRecords)); err != nil {
		return fmt.Errorf("refresh: preparing cursors: %w", err)
	}

	if err := b.Run(); err != nil {
		return fmt.Errorf("refresh: writing to %s: %w", r.DB.DBPath, err)
	}
	return nil
}

// originValue returns the origin to store for one session record: the
// record's own value when it is a member of the closed set, otherwise
// "unknown". The merge logic upstream normally decides the value; this is
// the invariant that keeps whatever reaches the database inside the set.
func originValue(rec map[string]any) string {
	if v, ok := rec["origin"].(string); ok && session.Origin(v).Valid() {
		return v
	}
	return string(session.OriginUnknown)
}

// allocateHandles assigns the next handle(s) to whichever lineages in
// lineageRecords do not already exist in this profile's lineages table -
// "allocated when a lineage is first created, monotonically and without
// reuse" (task 1.2). A lineage already present keeps whatever handle it was
// given the first time it was seen; this function never returns a record
// for it, so the BulkUpdate in write() never touches it again.
//
// New lineages within one refresh pass are assigned in lineage-id order,
// purely for determinism within this pass - task 1.2 requires monotonic,
// unique, never-reused numbering, not any particular ordering among
// lineages discovered in the same refresh.
func (r *Refresher) allocateHandles(lineageRecords []map[string]any) ([]map[string]any, error) {
	existing, maxHandle, err := r.loadLineageHandles()
	if err != nil {
		return nil, err
	}

	var newIDs []string
	seen := map[string]bool{}
	for _, rec := range lineageRecords {
		id, _ := rec["id"].(string)
		if id == "" || seen[id] || existing[id] {
			continue
		}
		seen[id] = true
		newIDs = append(newIDs, id)
	}
	sort.Strings(newIDs)

	out := make([]map[string]any, 0, len(newIDs))
	next := maxHandle + 1
	for _, id := range newIDs {
		out = append(out, map[string]any{"id": id, "handle": next})
		next++
	}
	return out, nil
}

// loadLineageHandles reads which lineage ids already exist for this
// profile, and the highest handle currently in use, so new handles are
// allocated above it (task 1.2/2.1; design.md decision 2: the counter is
// per profile).
func (r *Refresher) loadLineageHandles() (map[string]bool, int, error) {
	var rows []struct {
		ID     string `json:"id"`
		Handle *int   `json:"handle"`
	}
	q := "SELECT id, handle FROM lineages WHERE profile = " + quoteStringLiteral(r.Profile.Name) + ";"
	if err := r.DB.Query(q, &rows); err != nil {
		return nil, 0, err
	}
	existing := make(map[string]bool, len(rows))
	max := 0
	for _, row := range rows {
		existing[row.ID] = true
		if row.Handle != nil && *row.Handle > max {
			max = *row.Handle
		}
	}
	return existing, max, nil
}

func lineageRecordsFrom(sessionRecords []map[string]any, profileName string) []map[string]any {
	seen := map[string]bool{}
	var out []map[string]any
	for _, rec := range sessionRecords {
		id, _ := rec["lineage_id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, map[string]any{"id": id, "profile": profileName, "orphaned": 0})
	}
	return out
}

// dedupeCursors keeps the last record for each (source, source_id) pair -
// BulkUpsert's single INSERT...SELECT statement cannot itself apply two
// updates to the same key within one call.
func dedupeCursors(records []map[string]any) []map[string]any {
	type key struct{ source, id string }
	last := map[key]int{}
	for i, rec := range records {
		if rec == nil {
			continue
		}
		s, _ := rec["source"].(string)
		id, _ := rec["source_id"].(string)
		last[key{s, id}] = i
	}
	out := make([]map[string]any, 0, len(last))
	for _, i := range last {
		out = append(out, records[i])
	}
	return out
}

// reconcileLineages marks every lineage in this profile that was not
// observed during this refresh pass as orphaned, and any lineage that was
// observed as no longer orphaned - without ever deleting a lineage row, so
// its annotations are retained (spec session-annotations, "Annotated
// session disappears from its source"; task 9.4).
func (r *Refresher) reconcileLineages(observed map[string]bool) error {
	ids := make([]map[string]any, 0, len(observed))
	for id := range observed {
		ids = append(ids, map[string]any{"id": id})
	}

	b := r.DB.NewBatch()
	b.Exec(fmt.Sprintf("UPDATE lineages SET orphaned = 1 WHERE profile = %s;", quoteStringLiteral(r.Profile.Name)))
	if len(ids) > 0 {
		if err := b.BulkUpdate("lineages", "id", []string{"id", "orphaned"}, unorphanRecords(ids)); err != nil {
			return err
		}
	}
	return b.Run()
}

// migrateLineageAnnotations carries comments/tags forward from a session's
// old lineage id to its new one, for every session whose lineage id changed
// as a side effect of this rebuild correcting its SourceSessionID (change
// fix-resume-session-identity, design.md "Annotations attach to lineages,
// not to source identifiers" - true only if something makes it true across
// the one rebuild where a lineage's own derivation input changes; this is
// that something. Matched by transcript path, the one thing a corrected
// identifier does not change). oldByTranscript is nil/empty on every normal
// (non-full-rebuild) pass, and on a full rebuild that corrects nothing -
// both cases fall through as a no-op immediately.
func (r *Refresher) migrateLineageAnnotations(oldByTranscript map[string]string, sessionRecords []map[string]any) error {
	if len(oldByTranscript) == 0 {
		return nil
	}

	pairs := map[string]string{} // old lineage id -> new lineage id
	for _, rec := range sessionRecords {
		tp, _ := rec["transcript_path"].(string)
		if tp == "" {
			continue
		}
		newLineage, _ := rec["lineage_id"].(string)
		oldLineage, ok := oldByTranscript[tp]
		if !ok || oldLineage == "" || oldLineage == newLineage {
			continue
		}
		pairs[oldLineage] = newLineage
	}
	if len(pairs) == 0 {
		return nil
	}

	b := r.DB.NewBatch()
	for old, new := range pairs {
		qOld, qNew := quoteStringLiteral(old), quoteStringLiteral(new)
		b.Exec(fmt.Sprintf("UPDATE comments SET lineage_id = %s WHERE lineage_id = %s;", qNew, qOld))
		// tags is keyed (lineage_id, tag) (schema.go annotationDDL): move
		// whatever doesn't already exist under the new lineage id, then
		// drop anything left under the old one - a genuine duplicate, where
		// the new (live) lineage already carries that same tag.
		b.Exec(fmt.Sprintf(
			"UPDATE tags SET lineage_id = %s WHERE lineage_id = %s AND tag NOT IN (SELECT tag FROM tags WHERE lineage_id = %s);",
			qNew, qOld, qNew,
		))
		b.Exec(fmt.Sprintf("DELETE FROM tags WHERE lineage_id = %s;", qOld))
	}
	return b.Run()
}

// unorphanRecords clears orphaned back to 0 for every lineage observed in
// this refresh pass.
func unorphanRecords(ids []map[string]any) []map[string]any {
	out := make([]map[string]any, len(ids))
	for i, rec := range ids {
		out[i] = map[string]any{"id": rec["id"], "orphaned": 0}
	}
	return out
}

func quoteStringLiteral(s string) string {
	// r.Profile.Name is program-controlled (derived from a config
	// directory basename at Discover time, validated indirectly by
	// filesystem lookups) - not arbitrary session content - but it is
	// still escaped defensively rather than trusted.
	out := "'"
	for _, c := range s {
		if c == '\'' {
			out += "''"
		} else {
			out += string(c)
		}
	}
	return out + "'"
}
