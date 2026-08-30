package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/profile"
)

// Synthetic fixtures only - no real session content.

func TestDiscoverFindsTranscriptsWithoutDecodingDirNames(t *testing.T) {
	root := t.TempDir()
	// Deliberately ambiguous encoded directory name: cannot be reversed to
	// a real path (both '/' and '.' collapse to '-').
	dir := filepath.Join(root, "projects", "-Users-name-x--config-y")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "11111111-1111-1111-1111-111111111111.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"user","message":{"role":"user","content":"hi"},"cwd":"/Users/name/x.config/y","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": root}}
	discovered, err := a.Discover(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 {
		t.Fatalf("got %d sessions, want 1", len(discovered))
	}
	d := discovered[0]
	if d.Session.SourceSessionID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("session id = %q", d.Session.SourceSessionID)
	}
	if d.TranscriptPath != transcript {
		t.Errorf("transcript path = %q", d.TranscriptPath)
	}
	// The adapter itself must never have tried to decode the directory
	// name into a cwd - that only happens later, from transcript content.
	if d.Session.CWD != nil {
		t.Errorf("adapter must not set CWD from directory encoding, got %v", d.Session.CWD)
	}
}

// TestUnreadableProjectsSubdirIsSkippedNotFatal covers spec session-index,
// "A source cannot be read": one unreadable subdirectory under projects/
// must not stop the rest of the source's sessions from being discovered.
func TestUnreadableProjectsSubdirIsSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	goodDir := filepath.Join(root, "projects", "-work-repo")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "22222222-2222-2222-2222-222222222222.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"still readable"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	badDir := filepath.Join(root, "projects", "-unreadable")
	if err := os.MkdirAll(badDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(badDir, 0o755) }) // let t.TempDir() clean up

	a := New()
	p := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": root}}
	discovered, err := a.Discover(p)
	if err != nil {
		t.Fatalf("one unreadable subdir must not fail discovery for the rest of the source: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Session.SourceSessionID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("got %+v", discovered)
	}
}

func TestDiscoverUnavailableWhenNoRoot(t *testing.T) {
	a := New()
	_, err := a.Discover(profile.Profile{Name: "x"})
	if err == nil {
		t.Fatal("expected an error when no Claude root is configured")
	}
}

func TestPromptsSinceIncremental(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "history.jsonl")
	line1 := `{"display":"/exit","pastedContents":{},"timestamp":1000,"project":"/x","sessionId":"s1"}` + "\n"
	if err := os.WriteFile(path, []byte(line1), 0o644); err != nil {
		t.Fatal(err)
	}
	prompts, off, err := PromptsSince(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0].Text != "/exit" {
		t.Fatalf("got %+v", prompts)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line2 := `{"display":"do the thing","pastedContents":{},"timestamp":2000,"project":"/x","sessionId":"s1"}` + "\n"
	f.WriteString(line2)
	f.Close()

	prompts2, _, err := PromptsSince(root, off)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts2) != 1 || prompts2[0].Text != "do the thing" {
		t.Fatalf("incremental read got %+v, want only the new line", prompts2)
	}
}
