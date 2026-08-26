// Package sqlitex drives SQLite through the pure-Go driver
// (modernc.org/sqlite), replacing the previous implementation that ran the
// system sqlite3 binary as a subprocess (change
// replace-sqlite-subprocess-with-driver, design.md decision 1). There is
// no external program and no cgo anywhere in this module; the driver is
// compiled into the binary.
//
// Two invariants hold everywhere in this package:
//
//  1. Session content, comment bodies, tag names, and any other text that
//     originated outside this program's own source code is NEVER formatted
//     directly into a SQL statement string. Every such value travels
//     either through a bound parameter (bulk writes, and the ParamFile
//     mechanism for read queries) or through SQLite's own JSON functions.
//     Arbitrary quoting, semicolons, and control characters in a
//     transcript can never change what SQL runs.
//  2. Every batch of statements runs inside exactly one transaction
//     (task 2.1).
//
// The public surface is byte-compatible with the subprocess implementation
// it replaces: Runner's BinPath and TmpDir fields, and the sqlite3Path
// parameters of the packages' constructors, are retained as documented
// no-ops so the pre-change test suite and all callers compile and behave
// unchanged (design.md decision 5; the acceptance criterion is the
// existing test suite passing unchanged).
package sqlitex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Runner drives one database file through the in-process driver.
type Runner struct {
	// BinPath is retained for source compatibility with the pre-driver
	// subprocess implementation (and its tests, which construct Runners
	// with it). It is never consulted: the driver is compiled into the
	// binary, so there is no external program to locate (task 4.1).
	BinPath string
	// DBPath is the database file this Runner operates on. Empty means
	// ":memory:", used only in tests.
	DBPath string
	// ReadOnly opens DBPath as a read-only connection. Used for reading
	// source databases that belong to other agents - LazyRecall must never
	// write to them and must never checkpoint their WAL (task 3.1). It is
	// enforced at connection configuration (mode=ro URI + query_only
	// pragma), not merely by convention.
	ReadOnly bool
	// TmpDir is retained for source compatibility with the subprocess
	// implementation, which wrote temporary NDJSON batch files. The driver
	// binds parameters directly, so no temporary files are created
	// anymore; the field is never consulted.
	TmpDir string

	mu  sync.Mutex
	db  *sql.DB
	pms paramRegistry
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// quoteIdent validates and double-quotes a SQL identifier. Table and column
// names in this package always come from Go source code, never from parsed
// session content, but they are still validated defensively before being
// placed in a SQL string.
func quoteIdent(name string) (string, error) {
	if !identRe.MatchString(name) {
		return "", fmt.Errorf("sqlitex: invalid identifier %q", name)
	}
	return `"` + name + `"`, nil
}

// dsn builds the driver connection string for this Runner.
//
// Read-only connections open with a mode=ro URI so the file can never be
// created, modified, or checkpointed - including its write-ahead log -
// plus a query_only pragma as a second line of defense, exactly as the
// subprocess implementation enforced with -cmd PRAGMA query_only = ON.
// A busy_timeout is set on every connection: a source database has its own
// active writer (task 3.3), and even in WAL mode a reader can transiently
// collide with a writer's commit. Waiting a little is correct here -
// LazyRecall is never the one holding a source's lock, so it is never the one
// causing another connection to wait.
func (r *Runner) dsn() string {
	if r.DBPath == "" {
		return ":memory:"
	}
	if r.ReadOnly {
		return fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)", r.DBPath)
	}
	return fmt.Sprintf("%s?_pragma=busy_timeout(5000)", r.DBPath)
}

// open returns the shared *sql.DB for this Runner, opening it on first
// use. Connections are capped at one per Runner so access is serialised
// exactly as the subprocess implementation serialised it (design.md risk
// "Concurrency differences": keep access serialised where the existing
// code assumed it).
func (r *Runner) open() (*sql.DB, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil {
		return r.db, nil
	}
	db, err := sql.Open("sqlite", r.dsn())
	if err != nil {
		return nil, fmt.Errorf("sqlitex: opening %s: %w", r.DBPath, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitex: opening %s: %w", r.DBPath, err)
	}
	r.db = db
	return db, nil
}

// Close releases the underlying connection. The old subprocess
// implementation had no persistent handle to close, so no caller does this
// today; it exists so long-lived processes can free the file handle.
func (r *Runner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// paramRegistry tracks live ParamFiles' values so Query/Exec can bind them
// as named parameters. Values are bound, never interpolated - a ParamFile
// is the read-path analogue of the write-path bulk parameter binding.
type paramRegistry struct {
	mu      sync.Mutex
	nextID  int
	byID    map[int]map[string]any
	ordered map[int][]string // field names in Ref() call order, per file
}

func (pr *paramRegistry) register(params map[string]any) (int, map[string]any, []string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	id := pr.nextID
	pr.nextID++
	if pr.byID == nil {
		pr.byID = map[int]map[string]any{}
		pr.ordered = map[int][]string{}
	}
	pr.byID[id] = params
	order := make([]string, 0, len(params))
	for k := range params {
		order = append(order, k)
	}
	pr.ordered[id] = order
	return id, params, order
}

func (pr *paramRegistry) unregister(id int) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	delete(pr.byID, id)
	delete(pr.ordered, id)
}

func (pr *paramRegistry) collect() []any {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	var args []any
	for id, params := range pr.byID {
		for field, v := range params {
			args = append(args, sql.Named(fmt.Sprintf("rp%d_%s", id, field), v))
		}
	}
	return args
}

// Query runs a single read-only statement and decodes its rows into dest
// (a pointer to a slice of structs or maps), preserving the JSON decode
// semantics the subprocess implementation had (json tags, NULL into
// pointers, number coercion). External values referenced by ParamFile.Ref
// in the SQL are bound as named parameters by this call.
func (r *Runner) Query(sql string, dest any) error {
	db, err := r.open()
	if err != nil {
		return err
	}
	rows, err := db.Query(sql, r.pms.collect()...)
	if err != nil {
		return fmt.Errorf("sqlitex: query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("sqlitex: scanning row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeDriverValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlitex: iterating rows: %w", err)
	}
	if out == nil {
		out = []map[string]any{} // a zero-row result decodes as [], as before
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("sqlitex: encoding query result: %w", err)
	}
	if err := json.Unmarshal(buf, dest); err != nil {
		return fmt.Errorf("sqlitex: decoding query result: %w", err)
	}
	return nil
}

// normalizeDriverValue maps a driver value to the JSON-friendly shape the
// sqlite3 CLI's -json mode produced: TEXT and BLOB come back as strings,
// everything else keeps its native JSON representation.
func normalizeDriverValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return v
	}
}

// Exec runs a single statement or script with no result decoding. The
// driver executes multi-statement scripts (schema DDL, migrations) as a
// sequence of autocommit statements. For write statements that carry only
// program-controlled identifiers - never content originating outside this
// program (values travel through parameters or ParamFile.Ref instead).
func (r *Runner) Exec(sql string) error {
	db, err := r.open()
	if err != nil {
		return err
	}
	_, err = db.Exec(sql, r.pms.collect()...)
	return err
}

// Batch accumulates work to run together inside one transaction (task
// 2.1). Build it with NewBatch, add work with Exec / BulkInsert /
// BulkUpsert / BulkUpdate, then call Run.
type Batch struct {
	r     *Runner
	steps []batchStep
}

type batchStep struct {
	raw      string // literal SQL (schema statements, fixed updates)
	stmt     string // parameterised statement for bulk operations
	argsList [][]any
}

// NewBatch starts a batch of writes against this Runner's database.
func (r *Runner) NewBatch() *Batch {
	return &Batch{r: r}
}

// Exec appends a literal SQL statement that carries no external content -
// schema DDL, fixed-shape queries, statements built entirely from
// identifiers this package validated. It must not be used to embed session
// content, user-typed comment/tag text, or any other value that did not
// originate in this program's own source.
func (b *Batch) Exec(sql string) *Batch {
	b.steps = append(b.steps, batchStep{raw: sql})
	return b
}

// BulkInsert inserts records into table via parameterised statements (task
// 2.2). Each record supplies exactly the given columns; columns absent
// from a given record are inserted as SQL NULL, matching the "absent
// distinguishable from empty" rule in the session model.
func (b *Batch) BulkInsert(table string, columns []string, records []map[string]any) error {
	if len(records) == 0 {
		return nil
	}
	qTable, qCols, err := quoteColumns(table, columns)
	if err != nil {
		return err
	}
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = "?"
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);", qTable, qCols, strings.Join(placeholders, ", "))
	b.steps = append(b.steps, batchStep{stmt: stmt, argsList: recordsToArgs(columns, records)})
	return nil
}

// BulkUpsert inserts records into table, updating any row whose keyColumns
// already match rather than failing on the conflict. keyColumns must name a
// unique index or primary key on table (it may be composite - e.g. the
// cursors table's (source, source_id)). Used for the sessions and cursors
// tables, which a refresh writes to repeatedly for the same identity
// across runs.
func (b *Batch) BulkUpsert(table string, keyColumns []string, columns []string, records []map[string]any) error {
	if len(records) == 0 {
		return nil
	}
	qTable, qCols, err := quoteColumns(table, columns)
	if err != nil {
		return err
	}
	isKey := map[string]bool{}
	qKeys := make([]string, len(keyColumns))
	for i, k := range keyColumns {
		qk, err := quoteIdent(k)
		if err != nil {
			return err
		}
		qKeys[i] = qk
		isKey[k] = true
	}
	var setClauses []string
	for _, c := range columns {
		if isKey[c] {
			continue
		}
		qc, err := quoteIdent(c)
		if err != nil {
			return err
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = excluded.%s", qc, qc))
	}
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = "?"
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s;",
		qTable, qCols, strings.Join(placeholders, ", "), strings.Join(qKeys, ", "), strings.Join(setClauses, ", "))
	b.steps = append(b.steps, batchStep{stmt: stmt, argsList: recordsToArgs(columns, records)})
	return nil
}

// BulkUpdate updates existing rows in table, matching each record to an
// existing row by keyColumn (task 2.2). Every record must include keyColumn
// among its values.
func (b *Batch) BulkUpdate(table, keyColumn string, columns []string, records []map[string]any) error {
	if len(records) == 0 {
		return nil
	}
	qTable, err := quoteIdent(table)
	if err != nil {
		return err
	}
	qKey, err := quoteIdent(keyColumn)
	if err != nil {
		return err
	}
	var setClauses []string
	argCols := []string{}
	for _, c := range columns {
		if c == keyColumn {
			continue
		}
		qc, err := quoteIdent(c)
		if err != nil {
			return err
		}
		setClauses = append(setClauses, qc+" = ?")
		argCols = append(argCols, c)
	}
	stmt := fmt.Sprintf("UPDATE %s SET %s WHERE %s = ?;", qTable, strings.Join(setClauses, ", "), qKey)

	argsList := make([][]any, 0, len(records))
	for _, rec := range records {
		args := make([]any, 0, len(argCols)+1)
		for _, c := range argCols {
			args = append(args, rec[c]) // absent -> nil -> NULL, matching the NDJSON path
		}
		args = append(args, rec[keyColumn])
		argsList = append(argsList, args)
	}
	b.steps = append(b.steps, batchStep{stmt: stmt, argsList: argsList})
	return nil
}

// quoteColumns validates and quotes a table name and column list together.
func quoteColumns(table string, columns []string) (string, string, error) {
	qTable, err := quoteIdent(table)
	if err != nil {
		return "", "", err
	}
	qCols := make([]string, len(columns))
	for i, c := range columns {
		qc, err := quoteIdent(c)
		if err != nil {
			return "", "", err
		}
		qCols[i] = qc
	}
	return qTable, strings.Join(qCols, ", "), nil
}

// recordsToArgs lays out each record's values in column order for a
// parameterised statement. A column absent from a record binds NULL,
// exactly as the NDJSON path did (an absent key never reached the SQL, so
// json_each produced NULL for it).
func recordsToArgs(columns []string, records []map[string]any) [][]any {
	argsList := make([][]any, 0, len(records))
	for _, rec := range records {
		args := make([]any, len(columns))
		for i, c := range columns {
			if v, ok := rec[c]; ok {
				args[i] = v
			}
		}
		argsList = append(argsList, args)
	}
	return argsList
}

// Run executes every accumulated step in one transaction. A failure rolls
// the whole batch back.
func (b *Batch) Run() error {
	if b.r.ReadOnly {
		return fmt.Errorf("sqlitex: refusing to run a write batch against a read-only Runner")
	}
	if len(b.steps) == 0 {
		return nil
	}
	db, err := b.r.open()
	if err != nil {
		return err
	}
	// LevelSerializable maps to BEGIN IMMEDIATE in the driver, the same
	// immediate-write-lock semantics the subprocess implementation used.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("sqlitex: beginning batch transaction: %w", err)
	}
	for _, step := range b.steps {
		if step.raw != "" {
			if _, err := tx.Exec(step.raw); err != nil {
				tx.Rollback()
				return fmt.Errorf("sqlitex: batch statement: %w", err)
			}
			continue
		}
		for _, args := range step.argsList {
			if _, err := tx.Exec(step.stmt, args...); err != nil {
				tx.Rollback()
				return fmt.Errorf("sqlitex: batch statement: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitex: committing batch: %w", err)
	}
	return nil
}

// ParamFile carries external values (search text, filter values typed by
// the user) into a read query, the read-path analogue of bulk parameter
// binding: values are bound as named parameters by the Query/Exec call,
// never interpolated into the SQL string. Reference a field with Ref
// inside a WHERE clause; call Close when the query is done. No temporary
// file is involved (task 2.3: the subprocess's temp-file mechanism is
// gone).
type ParamFile struct {
	owner *Runner
	id    int
}

// WriteParams registers params for named-parameter binding by subsequent
// Query/Exec calls on the same Runner.
func (r *Runner) WriteParams(params map[string]any) (*ParamFile, error) {
	id, _, _ := r.pms.register(params)
	return &ParamFile{owner: r, id: id}, nil
}

// Ref returns the named-parameter token for one field of this param file -
// safe to embed literally in a WHERE clause string, since the value itself
// never appears in the SQL text and the token is a plain identifier the
// driver binds. Field names are validated identifiers (letters, digits,
// underscore).
func (p *ParamFile) Ref(field string) string {
	if !identRe.MatchString(field) {
		panic("sqlitex: invalid param field " + field)
	}
	return fmt.Sprintf(":rp%d_%s", p.id, field)
}

// Close unregisters this file's parameters. Safe to call on a nil
// *ParamFile.
func (p *ParamFile) Close() {
	if p == nil {
		return
	}
	p.owner.pms.unregister(p.id)
}

// FTS5Phrase quotes text as a single FTS5 phrase literal, so a search
// string typed by a user - which may contain FTS5 query-syntax characters
// like quotes, hyphens, or "AND"/"NOT" - is matched literally rather than
// parsed as an FTS5 query expression. Verified by hand against adversarial
// input (embedded quotes, "; DROP TABLE", bare FTS5 operators).
func FTS5Phrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
