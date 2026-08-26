package sqlitex

import (
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
