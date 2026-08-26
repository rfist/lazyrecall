package omp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"lazyrecall/internal/profile"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agent", "sessions", "-dotfiles-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "2026-01-01T00-00-00-000Z_11111111-1111-1111-1111-111111111111.jsonl")
	content := `{"type":"session","version":3,"id":"11111111-1111-1111-1111-111111111111","timestamp":"2026-01-01T00:00:00Z","cwd":"/dotfiles/project","title":"synthetic test title","titleSource":"auto"}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New("sqlite3")
	p := profile.Profile{Name: "default", OmpRoot: root}
	discovered, err := a.Discover(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 {
		t.Fatalf("got %d, want 1", len(discovered))
	}
	if discovered[0].TranscriptPath != transcript {
		t.Errorf("path = %q", discovered[0].TranscriptPath)
	}
}

func TestPromptsSinceReadOnly(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "agent", "history.db")
	seed := exec.Command(bin, dbPath, `
CREATE TABLE history (id INTEGER PRIMARY KEY AUTOINCREMENT, prompt TEXT NOT NULL, created_at INTEGER, cwd TEXT, session_id TEXT);
INSERT INTO history (prompt, created_at, cwd, session_id) VALUES ('synthetic prompt one', 1000, '/x', 's1');
INSERT INTO history (prompt, created_at, cwd, session_id) VALUES ('synthetic prompt two', 2000, '/x', 's1');
`)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding omp history.db: %v: %s", err, out)
	}

	a := New(bin)
	p := profile.Profile{Name: "default", OmpRoot: root}
	prompts, cursor, err := a.PromptsSince(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if cursor != 2 {
		t.Errorf("cursor = %d, want 2", cursor)
	}

	prompts2, _, err := a.PromptsSince(p, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts2) != 0 {
		t.Errorf("expected no new prompts past the cursor, got %d", len(prompts2))
	}
}
