package refresh

import (
	"os"
	"path/filepath"
	"testing"

	"lazyrecall/internal/profile"
)

// The pre-rename data directory holds the short handles, comments, and tags
// that are LazyRecall's own data and cannot be re-derived from any source.
// The rename must move them, not orphan them (change rename-to-lazyrecall).
func TestMigrateMovesTheLegacyDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LAZYRECALL_HOME", "")
	t.Setenv("RECALL_HOME", "")

	old := filepath.Join(home, ".recall")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "p.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateLegacyDataDir()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the legacy directory is still there (%v)", err)
	}
	moved := filepath.Join(home, ".lazyrecall", "p.db")
	if b, err := os.ReadFile(moved); err != nil || string(b) != "db" {
		t.Errorf("the database did not arrive at %s: %v", moved, err)
	}
}

// A live database must never be overwritten by the migration: once the new
// location exists, the old one is somebody else's business.
func TestMigrateNeverOverwritesAnExistingDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LAZYRECALL_HOME", "")
	t.Setenv("RECALL_HOME", "")

	old := filepath.Join(home, ".recall")
	current := filepath.Join(home, ".lazyrecall")
	for _, d := range []string{old, current} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(current, "p.db"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "p.db"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateLegacyDataDir()

	if b, _ := os.ReadFile(filepath.Join(current, "p.db")); string(b) != "live" {
		t.Errorf("the live database was overwritten with %q", string(b))
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the legacy directory was consumed even though the new one existed: %v", err)
	}
}

// A caller that has set the data directory explicitly has said where its
// data is; moving something else on top of that is the opposite of what it
// asked for.
func TestMigrateSkipsWhenTheDataDirWasSetExplicitly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	explicit := filepath.Join(home, "elsewhere")
	t.Setenv("LAZYRECALL_HOME", explicit)

	old := filepath.Join(home, ".recall")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}

	migrateLegacyDataDir()

	if _, err := os.Stat(old); err != nil {
		t.Errorf("the legacy directory was moved despite an explicit LAZYRECALL_HOME: %v", err)
	}
	if _, err := os.Stat(explicit); !os.IsNotExist(err) {
		t.Errorf("the explicit data directory was written to by the migration (%v)", err)
	}
	if got := profile.DataDir(); got != explicit {
		t.Errorf("DataDir() = %q, want the explicit %q", got, explicit)
	}
}

// Nothing to migrate is the ordinary case and must be silent and harmless.
func TestMigrateIsANoOpWithNoLegacyDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LAZYRECALL_HOME", "")
	t.Setenv("RECALL_HOME", "")

	migrateLegacyDataDir()

	if _, err := os.Stat(filepath.Join(home, ".lazyrecall")); !os.IsNotExist(err) {
		t.Errorf("the migration created a data directory with nothing to migrate (%v)", err)
	}
}
