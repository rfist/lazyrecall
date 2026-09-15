package sqlitex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	return &Runner{
		BinPath: bin,
		DBPath:  filepath.Join(dir, "test.db"),
		TmpDir:  dir,
	}
}

func TestBulkInsertNeverInterpolatesContent(t *testing.T) {
	r := testRunner(t)
	if err := r.Exec(`CREATE TABLE t (id TEXT PRIMARY KEY, body TEXT, n INTEGER);`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	adversarial := []string{
		`'; DROP TABLE t; --`,
		`"); DROP TABLE t; --`,
		"line1\nline2\ttabbed",
		`unicode: héllo 日本語 🎉`,
		`backslashes: \n \" \\ \/`,
		``,
	}

	b := r.NewBatch()
	records := make([]map[string]any, len(adversarial))
	for i, s := range adversarial {
		records[i] = map[string]any{"id": string(rune('a' + i)), "body": s, "n": i}
	}
	if err := b.BulkInsert("t", []string{"id", "body", "n"}, records); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := b.Run(); err != nil {
		t.Fatalf("run batch: %v", err)
	}

	var rows []struct {
		ID   string `json:"id"`
		Body string `json:"body"`
		N    int    `json:"n"`
	}
	if err := r.Query(`SELECT id, body, n FROM t ORDER BY n;`, &rows); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != len(adversarial) {
		t.Fatalf("got %d rows, want %d", len(rows), len(adversarial))
	}
	for i, row := range rows {
		if row.Body != adversarial[i] {
			t.Errorf("row %d: got body %q, want %q", i, row.Body, adversarial[i])
		}
	}

	// The table must still exist - the injection attempts above must never
	// have executed as SQL.
	var count []struct {
		C int `json:"c"`
	}
	if err := r.Query(`SELECT count(*) as c FROM t;`, &count); err != nil {
		t.Fatalf("table t was dropped or query failed: %v", err)
	}
	if count[0].C != len(adversarial) {
		t.Fatalf("row count changed unexpectedly: %d", count[0].C)
	}
}

func TestBulkUpdate(t *testing.T) {
	r := testRunner(t)
	if err := r.Exec(`CREATE TABLE t (id TEXT PRIMARY KEY, body TEXT);`); err != nil {
		t.Fatalf("create: %v", err)
	}
	b := r.NewBatch()
	if err := b.BulkInsert("t", []string{"id", "body"}, []map[string]any{
		{"id": "1", "body": "old"},
		{"id": "2", "body": "old2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	b2 := r.NewBatch()
	if err := b2.BulkUpdate("t", "id", []string{"id", "body"}, []map[string]any{
		{"id": "1", "body": "new; DROP TABLE t;--"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b2.Run(); err != nil {
		t.Fatal(err)
	}

	var rows []struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	if err := r.Query(`SELECT id, body FROM t ORDER BY id;`, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].Body != "new; DROP TABLE t;--" {
		t.Errorf("got %q", rows[0].Body)
	}
	if rows[1].Body != "old2" {
		t.Errorf("unrelated row changed: %q", rows[1].Body)
	}
}

func TestReadOnlyRunnerRefusesWrites(t *testing.T) {
	dir := t.TempDir()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dbPath := filepath.Join(dir, "src.db")
	rw := &Runner{BinPath: bin, DBPath: dbPath, TmpDir: dir}
	if err := rw.Exec(`CREATE TABLE t (id INTEGER); INSERT INTO t VALUES (1);`); err != nil {
		t.Fatalf("seed db: %v", err)
	}

	ro := &Runner{BinPath: bin, DBPath: dbPath, ReadOnly: true, TmpDir: dir}
	if err := ro.Exec(`INSERT INTO t VALUES (2);`); err == nil {
		t.Fatal("expected write against read-only runner to fail")
	}

	b := ro.NewBatch()
	if err := b.Run(); err == nil {
		t.Fatal("expected batch.Run against read-only runner to refuse")
	}
	_ = os.Getenv("unused") // keep os import if trimmed later
}

func TestBulkInsertOmitsAbsentColumnsAsNull(t *testing.T) {
	r := testRunner(t)
	if err := r.Exec(`CREATE TABLE t (id TEXT PRIMARY KEY, maybe TEXT);`); err != nil {
		t.Fatal(err)
	}
	b := r.NewBatch()
	if err := b.BulkInsert("t", []string{"id", "maybe"}, []map[string]any{
		{"id": "1"}, // "maybe" deliberately absent, not "" and not null-valued
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID    string  `json:"id"`
		Maybe *string `json:"maybe"`
	}
	if err := r.Query(`SELECT id, maybe FROM t;`, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].Maybe != nil {
		t.Errorf("expected absent field to load as NULL, got %v", *rows[0].Maybe)
	}
}

// The following cover FTS5PrefixTerms, which fixed the reported bug that
// searching "sketch" found nothing even though a prompt contained
// "sketchybar" - FTS5Phrase's whole-word, adjacent-order phrase match can
// never match a short word as a prefix of a longer token.

func TestFTS5PrefixTermsBuildsQuotedPrefixTermPerWord(t *testing.T) {
	if got, want := FTS5PrefixTerms("fix retry"), `"fix"* "retry"*`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFTS5PrefixTermsDoublesEmbeddedQuotesLikeFTS5Phrase(t *testing.T) {
	if got, want := FTS5PrefixTerms(`foo"bar`), `"foo""bar"*`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFTS5PrefixTermsEmptyOrWhitespaceKeepsPriorBehaviour covers "Empty or
// whitespace-only input keeps today's behaviour": with nothing but
// whitespace to split on, FTS5PrefixTerms falls back to FTS5Phrase on the
// original (untrimmed) input rather than producing an empty term list.
func TestFTS5PrefixTermsEmptyOrWhitespaceKeepsPriorBehaviour(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got, want := FTS5PrefixTerms(in), FTS5Phrase(in); got != want {
			t.Errorf("FTS5PrefixTerms(%q) = %q, want %q (FTS5Phrase's existing behaviour)", in, got, want)
		}
	}
}

func fts5TestTable(t *testing.T, r *Runner) {
	t.Helper()
	if err := r.Exec(`CREATE VIRTUAL TABLE fts USING fts5(id UNINDEXED, text, tokenize='porter unicode61');`); err != nil {
		t.Fatalf("create fts5 table: %v", err)
	}
}

func fts5Query(t *testing.T, r *Runner, matchQuery string) []string {
	t.Helper()
	pf, err := r.WriteParams(map[string]any{"q": matchQuery})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var rows []struct {
		ID string `json:"id"`
	}
	q := fmt.Sprintf(`SELECT id FROM fts WHERE fts MATCH %s ORDER BY id;`, pf.Ref("q"))
	if err := r.Query(q, &rows); err != nil {
		t.Fatalf("query: %v", err)
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

// TestFTS5PrefixTermsMatchesShortWordAgainstLongerToken is the exact repro
// of the reported bug: "sketch" must match an indexed token like
// "sketchybar".
func TestFTS5PrefixTermsMatchesShortWordAgainstLongerToken(t *testing.T) {
	r := testRunner(t)
	fts5TestTable(t, r)
	b := r.NewBatch()
	if err := b.BulkInsert("fts", []string{"id", "text"}, []map[string]any{
		{"id": "1", "text": "let's install sketchybar for the status bar"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	ids := fts5Query(t, r, FTS5PrefixTerms("sketch"))
	if len(ids) != 1 {
		t.Fatalf("got %v, want [\"1\"] (prefix match of \"sketch\" against \"sketchybar\")", ids)
	}
}

// TestFTS5PrefixTermsMultiWordAnyOrderAllRequired covers "fix retry"
// matching a row containing "fixing" ... "retries" in that or the opposite
// order, while a row missing either word is excluded.
func TestFTS5PrefixTermsMultiWordAnyOrderAllRequired(t *testing.T) {
	r := testRunner(t)
	fts5TestTable(t, r)
	b := r.NewBatch()
	if err := b.BulkInsert("fts", []string{"id", "text"}, []map[string]any{
		{"id": "both-forward", "text": "please fix the flaky retries"},
		{"id": "both-reversed", "text": "after several retries, fixing the issue"},
		{"id": "only-fix", "text": "please fix the test"},
		{"id": "only-retry", "text": "retry the deploy"},
		{"id": "neither", "text": "totally unrelated text"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
	ids := fts5Query(t, r, FTS5PrefixTerms("retry fix"))
	if len(ids) != 2 || ids[0] != "both-forward" || ids[1] != "both-reversed" {
		t.Fatalf("got %v, want [both-forward both-reversed] - both words required, order-independent", ids)
	}
}

// TestFTS5PrefixTermsSyntaxCharactersDontError covers FTS5 query-syntax
// characters and operators typed by a user: quoting each word (as
// FTS5Phrase already does) must keep the built MATCH query from ever
// failing to parse, regardless of what punctuation or bare operator
// keywords appear in it. (That the quoting keeps them literal rather than
// parsed as operators is what TestFTS5PrefixTermsBuildsQuotedPrefixTermPerWord
// and TestFTS5PrefixTermsDoublesEmbeddedQuotesLikeFTS5Phrase pin directly;
// a bare "OR" still legitimately prefix-matches a row containing a word
// like "ordinary", which is correct prefix behaviour, not operator
// parsing.)
func TestFTS5PrefixTermsSyntaxCharactersDontError(t *testing.T) {
	r := testRunner(t)
	fts5TestTable(t, r)
	b := r.NewBatch()
	if err := b.BulkInsert("fts", []string{"id", "text"}, []map[string]any{
		{"id": "1", "text": "an unrelated filler row"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	adversarial := []string{
		`say "hello"`, `star*wars`, `(parenthetical)`, `col:on`, `care^t`,
		`hy-phen`, "AND", "OR", "NOT", "NEAR(x y)",
	}
	for _, in := range adversarial {
		pf, err := r.WriteParams(map[string]any{"q": FTS5PrefixTerms(in)})
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		q := fmt.Sprintf(`SELECT id FROM fts WHERE fts MATCH %s;`, pf.Ref("q"))
		if err := r.Query(q, &rows); err != nil {
			t.Errorf("input %q caused a query error (FTS5 syntax leaked through): %v", in, err)
		}
		pf.Close()
	}
}
