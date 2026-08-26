package profile

import (
	"os"
	"path/filepath"
	"testing"
)

// withEnv sets env vars for the duration of the test and restores them
// afterward, including unsetting ones that were not previously set.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, existed := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if existed {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

func mkClaudeRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverTwoClaudeInstallsAreSeparateProfiles(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, ".claude")
	personal := filepath.Join(home, ".claude-personal")
	mkClaudeRoot(t, work)
	mkClaudeRoot(t, personal)

	withEnv(t, map[string]string{
		"HOME":                      home,
		"RECALL_CLAUDE_CONFIG_DIRS": work + ":" + personal,
		"RECALL_PI_HOME":            "",
		"RECALL_OMP_HOME":           "",
		"RECALL_HERMES_HOME":        "",
	})

	profiles := Discover()
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d: %+v", len(profiles), profiles)
	}

	names := map[string]Profile{}
	for _, p := range profiles {
		names[p.Name] = p
	}
	if _, ok := names["claude"]; !ok {
		t.Errorf("expected a 'claude' profile, got %v", names)
	}
	if _, ok := names["claude-personal"]; !ok {
		t.Errorf("expected a 'claude-personal' profile, got %v", names)
	}
	if names["claude"].ClaudeRoot == names["claude-personal"].ClaudeRoot {
		t.Errorf("the two profiles must not share a config root")
	}
}

func TestResolveRejectsAmbiguityWithoutADefault(t *testing.T) {
	// Two Claude profiles, neither named "personal", and no single-instance
	// sources to break the tie: Resolve must refuse rather than guess.
	home := t.TempDir()
	a := filepath.Join(home, ".claude-alpha")
	b := filepath.Join(home, ".claude-beta")
	mkClaudeRoot(t, a)
	mkClaudeRoot(t, b)
	withEnv(t, map[string]string{
		"HOME":                      home,
		"RECALL_CLAUDE_CONFIG_DIRS": a + ":" + b,
		"RECALL_PI_HOME":            "",
		"RECALL_OMP_HOME":           "",
		"RECALL_HERMES_HOME":        "",
		"RECALL_PROFILE":            "",
	})
	profiles := Discover()
	if _, err := Resolve(profiles, ""); err == nil {
		t.Fatal("expected Resolve to refuse to guess between two ambiguous profiles")
	}
}

func TestResolveHonorsExplicitRequest(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, ".claude-alpha")
	b := filepath.Join(home, ".claude-beta")
	mkClaudeRoot(t, a)
	mkClaudeRoot(t, b)
	withEnv(t, map[string]string{
		"HOME":                      home,
		"RECALL_CLAUDE_CONFIG_DIRS": a + ":" + b,
		"RECALL_PI_HOME":            "",
		"RECALL_OMP_HOME":           "",
		"RECALL_HERMES_HOME":        "",
	})
	profiles := Discover()
	p, err := Resolve(profiles, "claude-beta")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "claude-beta" {
		t.Errorf("got %q", p.Name)
	}
}

func TestSingleInstanceSourcesNeverDuplicateAcrossProfiles(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, ".claude")
	personal := filepath.Join(home, ".claude-personal")
	mkClaudeRoot(t, work)
	mkClaudeRoot(t, personal)
	piHome := filepath.Join(home, "pi-home")
	if err := os.MkdirAll(piHome, 0o755); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"HOME":                      home,
		"RECALL_CLAUDE_CONFIG_DIRS": work + ":" + personal,
		"RECALL_PI_HOME":            piHome,
		"RECALL_OMP_HOME":           "",
		"RECALL_HERMES_HOME":        "",
	})
	profiles := Discover()
	withPi := 0
	for _, p := range profiles {
		if p.PiRoot != "" {
			withPi++
		}
	}
	if withPi != 1 {
		t.Fatalf("expected pi to be bundled into exactly one profile, got %d", withPi)
	}
}

func TestDBPathIsOnePerProfile(t *testing.T) {
	home := t.TempDir()
	withEnv(t, map[string]string{"HOME": home, "RECALL_HOME": ""})
	p1 := Profile{Name: "claude"}
	p2 := Profile{Name: "claude-personal"}
	if DBPath(p1) == DBPath(p2) {
		t.Fatal("distinct profiles must resolve to distinct database files")
	}
}
