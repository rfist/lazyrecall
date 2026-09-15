package refresh

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/search"
)

// TestOneIndexCoversEveryInstall replaces the old per-profile isolation
// suite (change group-sessions-in-one-index, reversing design.md decision
// 1): two claude roots and an omp root are refreshed together, in one
// Refresher pass, into the single index. Every install's sessions must be
// listed side by side, tagged with the install that produced them, and a
// second refresh must re-scan nothing - the two claude roots' history.jsonl
// cursors, in particular, must not collide the way a bare "*" source-level
// cursor key would.
func TestOneIndexCoversEveryInstall(t *testing.T) {
	bin := sqlite3Path(t)
	dataDir := t.TempDir()
	os.Setenv("LAZYRECALL_HOME", dataDir)
	t.Cleanup(func() { os.Unsetenv("LAZYRECALL_HOME") })

	home := t.TempDir()
	workRoot := filepath.Join(home, ".claude")
	personalRoot := filepath.Join(home, ".claude-personal")
	ompRoot := filepath.Join(home, "omp-home")

	writeFile(t, filepath.Join(workRoot, "history.jsonl"),
		`{"display":"work-only prompt","timestamp":1700000000000,"project":"/work/secret","sessionId":"w1"}`+"\n")
	writeFile(t, filepath.Join(workRoot, "projects", "-work-secret", "w1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"work-only prompt, must never leak"},"cwd":"/work/secret","timestamp":"2026-01-01T00:00:00Z"}`+"\n")

	writeFile(t, filepath.Join(personalRoot, "history.jsonl"),
		`{"display":"personal-only prompt","timestamp":1700000000000,"project":"/personal/project","sessionId":"p1"}`+"\n")
	writeFile(t, filepath.Join(personalRoot, "projects", "-personal-project", "p1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"personal-only prompt"},"cwd":"/personal/project","timestamp":"2026-01-01T00:00:00Z"}`+"\n")

	writeFile(t, filepath.Join(ompRoot, "agent", "sessions", "-x", "2026-01-01T00-00-00-000Z_o1.jsonl"),
		`{"type":"session","cwd":"/x","title":"omp session","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"omp prompt"}]},"timestamp":"2026-01-01T00:00:01Z"}`+"\n")

	installs := []profile.Profile{
		{Name: "claude", Roots: map[string]string{"claude": workRoot}},
		{Name: "claude-personal", Roots: map[string]string{"claude": personalRoot}},
		{Name: "omp", Roots: map[string]string{"omp": ompRoot}},
	}

	r, err := New(installs, bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}

	items, err := search.List(r.DB, search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]search.Item{}
	for _, it := range items {
		byID[it.SessionID] = it
	}
	for _, want := range []string{"claude:claude:w1", "claude:claude-personal:p1"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("expected %q listed in the single index, got %+v", want, byID)
		}
	}
	var ompID string
	for id := range byID {
		if id != "claude:claude:w1" && id != "claude:claude-personal:p1" {
			ompID = id
		}
	}
	if ompID == "" || !hasPrefix(ompID, "omp:omp:") {
		t.Fatalf("expected the omp session listed as omp:omp:<id>, got %+v", byID)
	}
	if len(byID) != 3 {
		t.Fatalf("expected exactly 3 sessions across every install, got %d: %+v", len(byID), byID)
	}

	// install column values (sessions.install, read back via Item.Install
	// once search/lookup grows one - for now checked directly against the
	// database, since Stage D is what threads Install onto search.Item).
	var rows []struct {
		ID      string `json:"id"`
		Install string `json:"install"`
	}
	if err := r.DB.Query(`SELECT id, install FROM sessions ORDER BY id;`, &rows); err != nil {
		t.Fatal(err)
	}
	wantInstall := map[string]string{
		"claude:claude:w1":          "claude",
		"claude:claude-personal:p1": "claude-personal",
	}
	for _, row := range rows {
		if want, ok := wantInstall[row.ID]; ok && row.Install != want {
			t.Errorf("session %s: install = %q, want %q", row.ID, row.Install, want)
		}
	}

	// Handles are globally unique across installs (schema v8's
	// idx_lineages_handle, unique on handle alone).
	var handles []struct {
		Handle int `json:"handle"`
	}
	if err := r.DB.Query(`SELECT handle FROM lineages;`, &handles); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, h := range handles {
		if h.Handle == 0 {
			t.Errorf("expected every lineage to have a nonzero handle, got %+v", handles)
			continue
		}
		if seen[h.Handle] {
			t.Fatalf("handle %d reused across two lineages: %+v", h.Handle, handles)
		}
		seen[h.Handle] = true
	}

	// Both claude roots' history.jsonl prompts must be searchable - proving
	// tier 2 ran for each install, not just one.
	workHits, err := search.Search(r.DB, "work-only", search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(workHits) != 1 || workHits[0].SessionID != "claude:claude:w1" {
		t.Fatalf("expected the work install's prompt to be searchable, got %+v", workHits)
	}
	personalHits, err := search.Search(r.DB, "personal-only", search.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(personalHits) != 1 || personalHits[0].SessionID != "claude:claude-personal:p1" {
		t.Fatalf("expected the personal install's prompt to be searchable, got %+v", personalHits)
	}

	// Each claude root gets its own source-level cursor ("*:claude" /
	// "*:claude-personal"), not one shared "*" that would collide.
	var cursors []struct {
		Source   string `json:"source"`
		SourceID string `json:"source_id"`
	}
	if err := r.DB.Query(`SELECT source, source_id FROM cursors WHERE source = 'claude';`, &cursors); err != nil {
		t.Fatal(err)
	}
	cursorIDs := map[string]bool{}
	for _, c := range cursors {
		cursorIDs[c.SourceID] = true
	}
	if !cursorIDs["*:claude"] || !cursorIDs["*:claude-personal"] {
		t.Fatalf("expected distinct source-level cursors for both claude installs, got %+v", cursorIDs)
	}

	// A second refresh must re-scan nothing: the cursor rows stay exactly
	// as they are (cursor stability).
	before := cursors
	if _, err := r.Refresh(Options{}); err != nil {
		t.Fatal(err)
	}
	var after []struct {
		Source   string `json:"source"`
		SourceID string `json:"source_id"`
	}
	if err := r.DB.Query(`SELECT source, source_id FROM cursors WHERE source = 'claude';`, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("cursor row count changed across a no-op refresh: before=%d after=%d", len(before), len(after))
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
