package refresh

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"recall/internal/profile"
)

// TestNoSourceIsEverWritten builds a profile with synthetic data across all
// four sources (claude transcript + history.jsonl, pi transcript, omp
// transcript + history.db, hermes state.db) and asserts every one of those
// files is byte-for-byte and mtime-for-mtime unchanged after a full refresh
// followed by an incremental one (task 12.2; spec session-index, "Sources
// are read only").
func TestNoSourceIsEverWritten(t *testing.T) {
	bin := sqlite3Path(t)
	home := t.TempDir()
	dataDir := t.TempDir()
	os.Setenv("RECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("RECALL_HOME") })

	claudeRoot := filepath.Join(home, ".claude-personal")
	writeFile(t, filepath.Join(claudeRoot, "history.jsonl"),
		`{"display":"synthetic write-safety prompt","pastedContents":{},"timestamp":1700000000000,"project":"/x","sessionId":"c1"}`+"\n")
	writeFile(t, filepath.Join(claudeRoot, "projects", "-x", "c1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"synthetic write-safety prompt"},"cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	piRoot := filepath.Join(home, "pi-home")
	writeFile(t, filepath.Join(piRoot, "agent", "sessions", "--x--", "2026-01-01T00-00-00-000Z_p1.jsonl"),
		`{"type":"session","cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"synthetic pi prompt"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	ompRoot := filepath.Join(home, "omp-home")
	writeFile(t, filepath.Join(ompRoot, "agent", "sessions", "-x", "2026-01-01T00-00-00-000Z_o1.jsonl"),
		`{"type":"session","cwd":"/x","title":"synthetic omp title","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"synthetic omp prompt"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")
	ompDBPath := filepath.Join(ompRoot, "agent", "history.db")
	if err := os.MkdirAll(filepath.Dir(ompDBPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seedOmp := exec.Command(bin, ompDBPath, `
CREATE TABLE history (id INTEGER PRIMARY KEY AUTOINCREMENT, prompt TEXT, created_at INTEGER, cwd TEXT, session_id TEXT);
INSERT INTO history (prompt, created_at, cwd, session_id) VALUES ('synthetic omp prompt', 1000, '/x', 'o1');
`)
	if out, err := seedOmp.CombinedOutput(); err != nil {
		t.Fatalf("seeding omp db: %v: %s", err, out)
	}

	hermesRoot := filepath.Join(home, "hermes-home")
	hermesDBPath := filepath.Join(hermesRoot, "state.db")
	if err := os.MkdirAll(hermesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	seedHermes := exec.Command(bin, hermesDBPath, `
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, title TEXT, parent_session_id TEXT, started_at REAL, ended_at REAL, end_reason TEXT, message_count INTEGER, cwd TEXT, git_branch TEXT, git_repo_root TEXT, last_activity_at REAL);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT, content TEXT, finish_reason TEXT, tool_calls TEXT, timestamp REAL);
INSERT INTO sessions (id, source, title, started_at, cwd) VALUES ('h1', 'cli', 'synthetic hermes title', 1700000000, '/x');
INSERT INTO messages (session_id, role, content, finish_reason, timestamp) VALUES ('h1', 'user', 'synthetic hermes prompt', NULL, 1700000000);
INSERT INTO messages (session_id, role, content, finish_reason, timestamp) VALUES ('h1', 'assistant', 'synthetic hermes reply', 'stop', 1700000001);
`)
	if out, err := seedHermes.CombinedOutput(); err != nil {
		t.Fatalf("seeding hermes db: %v: %s", err, out)
	}

	p := profile.Profile{Name: "write-safety", ClaudeRoot: claudeRoot, PiRoot: piRoot, OmpRoot: ompRoot, HermesRoot: hermesRoot}

	sources := collectSourceFiles(t, home)
	if len(sources) == 0 {
		t.Fatal("test setup produced no source files to check")
	}
	before := snapshot(t, sources)

	r, err := New(p, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatalf("full refresh: %v", err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatalf("incremental refresh: %v", err)
	}
	if _, err := r.Refresh(Options{FullRebuild: true}); err != nil {
		t.Fatalf("full rebuild refresh: %v", err)
	}

	after := snapshot(t, sources)
	for path, b := range before {
		a, ok := after[path]
		if !ok {
			t.Errorf("source file disappeared: %s", path)
			continue
		}
		if a.hash != b.hash {
			t.Errorf("source file content changed: %s", path)
		}
		if a.modTime != b.modTime {
			t.Errorf("source file mtime changed (was touched): %s", path)
		}
	}
	if len(after) != len(before) {
		t.Errorf("source file count changed: before=%d after=%d (a file was created or deleted)", len(before), len(after))
	}
}

func collectSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type fileState struct {
	hash    string
	modTime int64
}

func snapshot(t *testing.T, paths []string) map[string]fileState {
	t.Helper()
	out := make(map[string]fileState, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		sum := sha256.Sum256(data)
		out[p] = fileState{hash: string(sum[:]), modTime: fi.ModTime().Truncate(time.Millisecond).UnixNano()}
	}
	return out
}
