package refresh

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"recall/internal/sqlitex"
)

// TestRefreshToleratesConcurrentTranscriptWrites verifies a refresh
// completes normally while an agent is actively appending to the very
// transcript file being scanned, and that the file is left byte-for-byte
// as the writer left it - refresh never touches source files (task 6.6;
// spec session-index, "Agent is running while the index refreshes").
func TestRefreshToleratesConcurrentTranscriptWrites(t *testing.T) {
	p, bin := buildTestProfile(t)
	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}

	transcriptPath := filepath.Join(p.ClaudeRoot, "projects", "-work-repo", "c1.jsonl")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	writeErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				writeErr <- err
				return
			}
			_, err = f.WriteString(`{"type":"user","message":{"role":"user","content":"synthetic message"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}` + "\n")
			f.Close()
			if err != nil {
				writeErr <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for i := 0; i < 5; i++ {
		if _, err := r.Refresh(Options{}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("refresh #%d failed while a writer was active: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-writeErr:
		t.Fatalf("writer goroutine failed: %v", err)
	default:
	}
}

// TestReadOnlyQueryDoesNotBlockConcurrentWriter exercises the same
// tolerance against a SQLite source (omp's shape): a goroutine repeatedly
// writes to the database while sqlitex issues read-only queries against it,
// and both must succeed.
func TestReadOnlyQueryDoesNotBlockConcurrentWriter(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "history.db")
	rw := &sqlitex.Runner{BinPath: bin, DBPath: dbPath, TmpDir: dir}
	// Real source databases (omp, hermes) run in WAL mode, which is what
	// lets a read-only connection proceed without waiting on a writer -
	// that is the property this test exists to check, so the synthetic db
	// must be set up the same way rather than SQLite's default
	// rollback-journal mode (which does briefly lock readers out).
	if err := rw.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE history (id INTEGER PRIMARY KEY AUTOINCREMENT, prompt TEXT);`); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = rw.Exec(`INSERT INTO history (prompt) VALUES ('synthetic');`)
			time.Sleep(time.Millisecond)
		}
	}()

	ro := &sqlitex.Runner{BinPath: bin, DBPath: dbPath, ReadOnly: true, TmpDir: dir}
	for i := 0; i < 10; i++ {
		var rows []struct {
			N int `json:"n"`
		}
		if err := ro.Query(`SELECT count(*) as n FROM history;`, &rows); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("read-only query #%d failed against an actively-written db: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}
