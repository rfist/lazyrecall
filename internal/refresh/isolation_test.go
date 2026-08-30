package refresh

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/search"
)

// TestNoOperationReturnsSessionsFromMultipleProfiles refreshes two
// distinct profiles - each with its own synthetic claude session - into
// LazyRecall's own database directory, then asserts that listing one profile's
// database never surfaces the other's session (task 12.3; spec
// session-index, "Profile isolation").
func TestNoOperationReturnsSessionsFromMultipleProfiles(t *testing.T) {
	bin := sqlite3Path(t)
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	home := t.TempDir()
	workRoot := filepath.Join(home, ".claude")
	personalRoot := filepath.Join(home, ".claude-personal")

	writeFile(t, filepath.Join(workRoot, "history.jsonl"), "")
	writeFile(t, filepath.Join(workRoot, "projects", "-work-secret", "w1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"work-only content, must never leak"},"cwd":"/work/secret","timestamp":"2026-01-01T00:00:00Z"}`+"\n")

	writeFile(t, filepath.Join(personalRoot, "history.jsonl"), "")
	writeFile(t, filepath.Join(personalRoot, "projects", "-personal-project", "p1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"personal-only content"},"cwd":"/personal/project","timestamp":"2026-01-01T00:00:00Z"}`+"\n")

	work := profile.Profile{Name: "claude", Roots: map[string]string{"claude": workRoot}}
	personal := profile.Profile{Name: "claude-personal", Roots: map[string]string{"claude": personalRoot}}

	rWork, err := New(work, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rWork.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	rPersonal, err := New(personal, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rPersonal.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	workItems, err := search.List(rWork.DB, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(workItems) != 1 || workItems[0].SessionID != "claude:claude:w1" {
		t.Fatalf("work profile listing = %+v", workItems)
	}
	for _, it := range workItems {
		if it.SessionID == "claude:claude-personal:p1" {
			t.Fatal("personal session leaked into the work profile's listing")
		}
	}

	personalItems, err := search.List(rPersonal.DB, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(personalItems) != 1 || personalItems[0].SessionID != "claude:claude-personal:p1" {
		t.Fatalf("personal profile listing = %+v", personalItems)
	}
	for _, it := range personalItems {
		if it.SessionID == "claude:claude:w1" {
			t.Fatal("work session leaked into the personal profile's listing")
		}
	}

	// Search must be equally isolated, not just List.
	hits, err := search.Search(rPersonal.DB, "work-only", search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("searching the personal profile's database must never surface work content, got %+v", hits)
	}

	// And the two profiles must be genuinely separate files, not a shared
	// database distinguished only by a column.
	if profile.DBPath(work) == profile.DBPath(personal) {
		t.Fatal("work and personal profiles must not share a database file")
	}
}
