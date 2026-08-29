// Package schema defines LazyRecall's own database schema - the index tables
// (sessions, prompt search, per-source cursors) and the annotation tables
// (lineages, comments, tags) - and the rule for evolving it: the index is
// disposable and gets discarded and rebuilt on a schema change; annotations
// are the one thing LazyRecall originates, so they are migrated forward and
// never rebuilt (design.md "Migration Plan"; tasks.md 2.4-2.6).
package schema

import (
	"fmt"

	"lazyrecall/internal/sqlitex"
)

// CurrentVersion is the schema version this build of LazyRecall expects. It is
// a var rather than a const solely so tests can simulate a version bump
// without a second binary.
//
// v2 adds the short session handle (lineages.handle) - see migrations[2]
// and annotationDDL below (change add-readable-session-listing, design.md
// decision 1: the handle lives on the durable lineages table, never on the
// disposable sessions table).
//
// v3 adds sessions.name, the user-chosen session name a source records
// (Claude Code's rename). It is index data read back out of the
// transcripts, so it needs no annotation migration - the version bump
// alone discards and rebuilds the index, and the next refresh repopulates
// it.
//
// v4 adds lineages.archived_at, the archive flag (change
// add-archive-facility). Archiving is a decision the user made, so it
// lives on the durable lineages table like every other annotation - never
// on the disposable sessions table, or the next refresh --full would
// destroy it. NULL means not archived; a Unix-seconds value means archived
// at that time.
//
// v5 adds sessions.origin, who drove the session. It is index data read
// back out of the transcripts (the claude entrypoint field), so it needs
// no annotation migration - the version bump alone discards and rebuilds
// the index, and the next refresh repopulates it.
//
// v6 adds sessions.client, the program a session was driven through
// (change show-editor-clients) - read out of the same transcript field as
// origin, and disposable for the same reason.
var CurrentVersion = 6

// indexDDL creates the tables that are pure cache over the sources: safe to
// drop and rebuild whenever CurrentVersion changes.
const indexDDL = `
CREATE TABLE IF NOT EXISTS sessions (
	id                TEXT PRIMARY KEY,
	source            TEXT NOT NULL,
	source_session_id TEXT NOT NULL,
	lineage_id        TEXT NOT NULL,
	continues_from    TEXT,
	topic             TEXT,
	name              TEXT,
	last_prompt       TEXT,
	cwd               TEXT,
	git_branch        TEXT,
	git_repo_root     TEXT,
	git_common_root   TEXT,
	started_at        INTEGER,
	last_activity_at  INTEGER,
	end_state         TEXT NOT NULL,
	origin            TEXT,
	client            TEXT,
	compaction_count  INTEGER,
	compaction_json   TEXT,
	transcript_path   TEXT,
	message_count     INTEGER,
	resumable         INTEGER NOT NULL DEFAULT 1,
	dir_exists        INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sessions_lineage ON sessions(lineage_id);
CREATE INDEX IF NOT EXISTS idx_sessions_last_activity ON sessions(last_activity_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_end_state ON sessions(end_state);
CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_sessions_git_common_root ON sessions(git_common_root);

CREATE VIRTUAL TABLE IF NOT EXISTS prompt_fts USING fts5(
	session_id UNINDEXED,
	kind UNINDEXED,
	text,
	tokenize = 'porter unicode61'
);

CREATE TABLE IF NOT EXISTS cursors (
	source          TEXT NOT NULL,
	source_id       TEXT NOT NULL, -- session id within that source, or '*' for a source-level cursor
	kind            TEXT NOT NULL, -- 'transcript_offset' | 'db_key'
	byte_offset     INTEGER,
	file_size       INTEGER,
	leading_bytes   TEXT,
	db_cursor_key   TEXT,
	tier2_key       TEXT,          -- last tier-2 (prompt) cursor consumed for this session
	updated_at      INTEGER NOT NULL,
	PRIMARY KEY (source, source_id)
);
`

// annotationDDL creates the tables LazyRecall itself originates. These are
// never dropped by a schema-version rebuild - only migrated forward.
const annotationDDL = `
CREATE TABLE IF NOT EXISTS lineages (
	id          TEXT PRIMARY KEY,
	profile     TEXT NOT NULL,
	orphaned    INTEGER NOT NULL DEFAULT 0,
	handle      INTEGER,
	archived_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_lineages_handle_profile ON lineages(profile, handle);

CREATE TABLE IF NOT EXISTS comments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	lineage_id TEXT NOT NULL,
	body       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_comments_lineage ON comments(lineage_id);

CREATE TABLE IF NOT EXISTS tags (
	lineage_id TEXT NOT NULL,
	tag        TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (lineage_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(tag);
`

const metaDDL = `
CREATE TABLE IF NOT EXISTS schema_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// migrations lists annotation-table migrations to apply in order, keyed by
// the version being migrated *to*. There are none yet - the schema starts
// at version 1 - but the mechanism exists so a future version bump can
// carry annotations forward without rebuilding them (design.md: "Schema
// changes to the index are handled by discarding and rebuilding it.
// Annotations are versioned and migrated forward; they are never
// rebuilt.").
var migrations = map[int]string{
	// v2 (change add-readable-session-listing, tasks 1.1/1.3): adds the
	// short handle to the durable lineages table and backfills it for
	// lineages that already existed, oldest lineage taking the lowest
	// handle (design.md "Migration Plan"). This runs before the index
	// tables are dropped and rebuilt (see the case below), so the v1
	// `sessions` table - specifically its started_at column - is still
	// present here and is what "oldest" is computed from. Handles are
	// allocated per profile (design.md decision 2), never across profiles,
	// via ROW_NUMBER() partitioned by profile; a lineage with no matching
	// session row (e.g. already orphaned) sorts last within its profile
	// rather than being skipped, and lineage id is the final tiebreaker for
	// full determinism.
	2: `
ALTER TABLE lineages ADD COLUMN handle INTEGER;
CREATE UNIQUE INDEX IF NOT EXISTS idx_lineages_handle_profile ON lineages(profile, handle);
WITH ordered AS (
	SELECT
		l.id AS lineage_id,
		l.profile AS profile,
		ROW_NUMBER() OVER (
			PARTITION BY l.profile
			ORDER BY COALESCE(MIN(s.started_at), 9223372036854775807), l.id
		) AS rn
	FROM lineages l
	LEFT JOIN sessions s ON s.lineage_id = l.id
	WHERE l.handle IS NULL
	GROUP BY l.id, l.profile
)
UPDATE lineages
SET handle = (SELECT rn FROM ordered WHERE ordered.lineage_id = lineages.id)
WHERE id IN (SELECT lineage_id FROM ordered);
`,
	// v4 (change add-archive-facility): adds the archive timestamp to the
	// durable lineages table - a Unix-seconds value meaning "archived at
	// this time", NULL meaning not archived. Archive state is the user's
	// decision, so it must survive an index rebuild exactly like every
	// other annotation; a boolean on the disposable sessions table would
	// be wiped by the next refresh --full.
	4: `
ALTER TABLE lineages ADD COLUMN archived_at INTEGER;
`,
}

// Open ensures the database at r.DBPath exists with the current schema,
// applying migrations or discarding-and-rebuilding the index tables as
// needed (task 2.6), and returns the version now in effect.
func Open(r *sqlitex.Runner) (int, error) {
	if err := r.Exec(metaDDL); err != nil {
		return 0, fmt.Errorf("schema: creating schema_meta: %w", err)
	}

	stored, err := storedVersion(r)
	if err != nil {
		return 0, err
	}

	switch {
	case stored == 0:
		// Fresh database: create everything at the current version.
		if err := r.Exec(annotationDDL); err != nil {
			return 0, fmt.Errorf("schema: creating annotation tables: %w", err)
		}
		if err := r.Exec(indexDDL); err != nil {
			return 0, fmt.Errorf("schema: creating index tables: %w", err)
		}
		if err := setVersion(r, CurrentVersion); err != nil {
			return 0, err
		}
		return CurrentVersion, nil

	case stored == CurrentVersion:
		// Already current. Still safe to (re-)ensure both DDLs, since every
		// statement is IF NOT EXISTS.
		if err := r.Exec(annotationDDL); err != nil {
			return 0, err
		}
		if err := r.Exec(indexDDL); err != nil {
			return 0, err
		}
		return CurrentVersion, nil

	case stored < CurrentVersion:
		// Migrate annotations forward, one version at a time; never touch
		// their rows directly.
		for v := stored + 1; v <= CurrentVersion; v++ {
			if mig, ok := migrations[v]; ok {
				if err := r.Exec(mig); err != nil {
					return 0, fmt.Errorf("schema: migrating annotations to v%d: %w", v, err)
				}
			}
		}
		if err := r.Exec(annotationDDL); err != nil {
			return 0, err
		}
		// The index is disposable: drop and rebuild rather than migrate.
		if err := r.Exec(`DROP TABLE IF EXISTS sessions; DROP TABLE IF EXISTS prompt_fts; DROP TABLE IF EXISTS cursors;`); err != nil {
			return 0, fmt.Errorf("schema: discarding stale index: %w", err)
		}
		if err := r.Exec(indexDDL); err != nil {
			return 0, err
		}
		if err := setVersion(r, CurrentVersion); err != nil {
			return 0, err
		}
		return CurrentVersion, nil

	default:
		return 0, fmt.Errorf("schema: database at %s has schema version %d, newer than this build (%d); use a newer lazyrecall binary", r.DBPath, stored, CurrentVersion)
	}
}

func storedVersion(r *sqlitex.Runner) (int, error) {
	var rows []struct {
		Value string `json:"value"`
	}
	if err := r.Query(`SELECT value FROM schema_meta WHERE key = 'schema_version';`, &rows); err != nil {
		return 0, fmt.Errorf("schema: reading schema_meta: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	var v int
	if _, err := fmt.Sscanf(rows[0].Value, "%d", &v); err != nil {
		return 0, fmt.Errorf("schema: parsing stored schema version %q: %w", rows[0].Value, err)
	}
	return v, nil
}

func setVersion(r *sqlitex.Runner, v int) error {
	sql := fmt.Sprintf(`INSERT INTO schema_meta (key, value) VALUES ('schema_version', '%d')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, v)
	return r.Exec(sql)
}
