package pi

import (
	"os"
	"path/filepath"
	"testing"

	"lazyrecall/internal/profile"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agent", "sessions", "--Users-x-project--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "2026-01-01T00-00-00-000Z_11111111-1111-1111-1111-111111111111.jsonl")
	content := `{"type":"session","version":3,"id":"11111111-1111-1111-1111-111111111111","timestamp":"2026-01-01T00:00:00Z","cwd":"/Users/x/project"}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	p := profile.Profile{Name: "default", PiRoot: root}
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
	if !discovered[0].Session.Resumable {
		t.Error("pi sessions always have a cwd context and should be resumable")
	}
}

func TestDiscoverUnavailableWhenNoRoot(t *testing.T) {
	a := New()
	if _, err := a.Discover(profile.Profile{Name: "x"}); err == nil {
		t.Fatal("expected error when pi root is not configured")
	}
}
