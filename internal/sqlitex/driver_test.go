package sqlitex

import (
	"testing"
)

// TestDriverCapabilitiesFTS5AndJSON pins the gates change
// replace-sqlite-subprocess-with-driver, design.md decision 1 required the
// implementer to verify before committing: the pure-Go driver provides FTS5
// (the search index depends on it) and the JSON functions. These are build-
// time fixed capabilities, so this is a build-time assertion, not a runtime
// preflight check (design.md decision 4) - it exists to fail loudly if a
// future driver swap ever loses them.
func TestDriverCapabilitiesFTS5AndJSON(t *testing.T) {
	r := &Runner{DBPath: t.TempDir() + "/cap.db"}

	// FTS5: virtual table creation, MATCH, and the auxiliary functions the
	// search path actually calls (snippet, bm25).
	if err := r.Exec(`CREATE VIRTUAL TABLE fts USING fts5(session_id UNINDEXED, kind UNINDEXED, text, tokenize='porter unicode61');`); err != nil {
		t.Fatalf("FTS5 virtual table unavailable: %v", err)
	}
	if err := r.Exec(`INSERT INTO fts (session_id, kind, text) VALUES ('s1', 'prompt', 'flaky retry loop');`); err != nil {
		t.Fatalf("FTS5 insert: %v", err)
	}
	var rows []struct {
		ID   string  `json:"session_id"`
		Snip string  `json:"snip"`
		Rank float64 `json:"rank"`
	}
	if err := r.Query(`SELECT session_id, snippet(fts, 2, '>>>', '<<<', ' ... ', 10) AS snip, bm25(fts) AS rank FROM fts WHERE fts MATCH '"flaky retry"';`, &rows); err != nil {
		t.Fatalf("FTS5 MATCH/snippet/bm25: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "s1" {
		t.Fatalf("FTS5 MATCH returned %+v, want the inserted row", rows)
	}

	// JSON functions: json_each (the bulk-load path relied on it; the
	// write path no longer does, but reads and migrations still may) and
	// json_extract.
	if err := r.Exec(`CREATE TABLE j (id INTEGER PRIMARY KEY, payload TEXT); INSERT INTO j (payload) VALUES (json_object('a', 1));`); err != nil {
		t.Fatalf("JSON1 json_object: %v", err)
	}
	var jrows []struct {
		A int `json:"a"`
	}
	if err := r.Query(`SELECT json_extract(payload, '$.a') AS a FROM j;`, &jrows); err != nil {
		t.Fatalf("JSON1 json_extract: %v", err)
	}
	if len(jrows) != 1 || jrows[0].A != 1 {
		t.Fatalf("json_extract returned %+v, want a=1", jrows)
	}
	var counts []struct {
		N int `json:"n"`
	}
	if err := r.Query(`SELECT count(*) AS n FROM json_each('[1,2,3]');`, &counts); err != nil {
		t.Fatalf("JSON1 json_each: %v", err)
	}
	if len(counts) != 1 || counts[0].N != 3 {
		t.Fatalf("json_each count = %+v, want 3", counts)
	}
}
