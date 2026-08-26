package profile

import (
	"os"
	"path/filepath"
	"reflect"
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
		"HOME":                          home,
		"LAZYRECALL_CONFIG":             "",
		"LAZYRECALL_CLAUDE_CONFIG_DIRS": work + ":" + personal,
		"LAZYRECALL_PI_HOME":            "",
		"LAZYRECALL_OMP_HOME":           "",
		"LAZYRECALL_HERMES_HOME":        "",
	})

	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
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
	if names["claude"].Roots["claude"] == names["claude-personal"].Roots["claude"] {
		t.Errorf("the two profiles must not share a config root")
	}
}

// TestResolveTwoClaudeRootsWithoutADefaultReturnsTheFirst is the
// regression test for the release-blocking bug: a machine with two Claude
// config roots and none of pi/omp/hermes used to be a hard error
// ("multiple profiles found and none is the default") with no way out from
// inside the tool. With no env, no config file, and nothing to make one
// profile the default, Resolve must still pick a profile.
func TestResolveTwoClaudeRootsWithoutADefaultReturnsTheFirst(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, ".claude")
	b := filepath.Join(home, ".claude-personal")
	mkClaudeRoot(t, a)
	mkClaudeRoot(t, b)
	withEnv(t, map[string]string{
		"HOME":               home,
		"LAZYRECALL_CONFIG":  "",
		"LAZYRECALL_PROFILE": "",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d: %+v", len(profiles), profiles)
	}
	p, err := Resolve(profiles, "")
	if err != nil {
		t.Fatalf("Resolve must succeed with two Claude roots and no default: %v", err)
	}
	// The list is sorted by name, so the first profile is "claude".
	if p.Name != "claude" {
		t.Errorf("got %q, want claude", p.Name)
	}
}

func TestResolveHonorsExplicitRequest(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, ".claude-alpha")
	b := filepath.Join(home, ".claude-beta")
	mkClaudeRoot(t, a)
	mkClaudeRoot(t, b)
	withEnv(t, map[string]string{
		"HOME":                          home,
		"LAZYRECALL_CONFIG":             "",
		"LAZYRECALL_CLAUDE_CONFIG_DIRS": a + ":" + b,
		"LAZYRECALL_PI_HOME":            "",
		"LAZYRECALL_OMP_HOME":           "",
		"LAZYRECALL_HERMES_HOME":        "",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(profiles, "claude-beta")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "claude-beta" {
		t.Errorf("got %q", p.Name)
	}
}

// TestResolveRequestedProfileThatDoesNotExistStillErrors pins down the one
// case that must not silently fall through to the first-profile step: a
// name the user asked for explicitly (the --profile flag) that matches no
// discovered profile.
func TestResolveRequestedProfileThatDoesNotExistStillErrors(t *testing.T) {
	home := t.TempDir()
	mkClaudeRoot(t, filepath.Join(home, ".claude"))
	mkClaudeRoot(t, filepath.Join(home, ".claude-personal"))
	withEnv(t, map[string]string{
		"HOME":               home,
		"LAZYRECALL_CONFIG":  "",
		"LAZYRECALL_PROFILE": "",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(profiles, "ghost"); err == nil {
		t.Fatal("expected an error for a requested profile that does not exist")
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
		"HOME":                          home,
		"LAZYRECALL_CONFIG":             "",
		"LAZYRECALL_CLAUDE_CONFIG_DIRS": work + ":" + personal,
		"LAZYRECALL_PI_HOME":            piHome,
		"LAZYRECALL_OMP_HOME":           "",
		"LAZYRECALL_HERMES_HOME":        "",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	withPi := 0
	for _, p := range profiles {
		if p.Roots["pi"] != "" {
			withPi++
		}
	}
	if withPi != 1 {
		t.Fatalf("expected pi to be bundled into exactly one profile, got %d", withPi)
	}
}

// writeConfigFile writes a config file into the temp HOME's expected
// location (~/.config/lazyrecall/config.toml), so config.Load picks it up
// exactly as it would on a real machine.
func writeConfigFile(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "lazyrecall")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveConfigDefaultProfileBeatsBuiltinRule covers the new third
// resolution step: the config file's default_profile must win over the
// built-in primary rule (which would otherwise pick claude-personal here,
// because it is the profile bundling pi).
func TestResolveConfigDefaultProfileBeatsBuiltinRule(t *testing.T) {
	home := t.TempDir()
	mkClaudeRoot(t, filepath.Join(home, ".claude"))
	mkClaudeRoot(t, filepath.Join(home, ".claude-personal"))
	if err := os.MkdirAll(filepath.Join(home, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfigFile(t, home, "default_profile = \"claude\"\n")
	withEnv(t, map[string]string{
		"HOME":               home,
		"LAZYRECALL_CONFIG":  "",
		"LAZYRECALL_PROFILE": "",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(profiles, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "claude" {
		t.Errorf("got %q, want claude (the config file's default_profile)", p.Name)
	}
}

// TestResolveEnvProfileBeatsConfigDefaultProfile covers the second
// resolution step: LAZYRECALL_PROFILE wins over the config file's
// default_profile, never the other way around.
func TestResolveEnvProfileBeatsConfigDefaultProfile(t *testing.T) {
	home := t.TempDir()
	mkClaudeRoot(t, filepath.Join(home, ".claude"))
	mkClaudeRoot(t, filepath.Join(home, ".claude-personal"))
	if err := os.MkdirAll(filepath.Join(home, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfigFile(t, home, "default_profile = \"claude\"\n")
	withEnv(t, map[string]string{
		"HOME":               home,
		"LAZYRECALL_CONFIG":  "",
		"LAZYRECALL_PROFILE": "claude-personal",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(profiles, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "claude-personal" {
		t.Errorf("got %q, want claude-personal (the LAZYRECALL_PROFILE value)", p.Name)
	}
}

// TestDiscoverSingleClaudeRootResolvesWithNothingSet covers the common
// fresh-machine case: a temp HOME with only a synthetic ~/.claude (a
// projects/ dir is enough for it to count as a real root) must discover
// exactly one profile and resolve it with nothing set.
func TestDiscoverSingleClaudeRootResolvesWithNothingSet(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"HOME":               home,
		"LAZYRECALL_CONFIG":  "",
		"LAZYRECALL_PROFILE": "",
	})
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected exactly 1 profile, got %d: %+v", len(profiles), profiles)
	}
	if profiles[0].Name != "claude" {
		t.Errorf("got %q, want claude", profiles[0].Name)
	}
	p, err := Resolve(profiles, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "claude" {
		t.Errorf("got %q, want claude", p.Name)
	}
}

func TestDBPathIsOnePerProfile(t *testing.T) {
	home := t.TempDir()
	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": ""})
	p1 := Profile{Name: "claude"}
	p2 := Profile{Name: "claude-personal"}
	if DBPath(p1) == DBPath(p2) {
		t.Fatal("distinct profiles must resolve to distinct database files")
	}
}

// The data directory's name changed with the rename, and the pre-rename
// environment variable stays honoured so an environment that still sets it
// cannot silently start indexing into a second, empty database (change
// rename-to-lazyrecall).
func TestDataDirResolutionOrder(t *testing.T) {
	home := t.TempDir()

	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": "", "RECALL_HOME": ""})
	if got, want := DataDir(), filepath.Join(home, ".lazyrecall"); got != want {
		t.Errorf("default DataDir() = %q, want %q", got, want)
	}

	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": "", "RECALL_HOME": "/legacy"})
	if got := DataDir(); got != "/legacy" {
		t.Errorf("with only RECALL_HOME set, DataDir() = %q, want /legacy", got)
	}

	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": "/current", "RECALL_HOME": "/legacy"})
	if got := DataDir(); got != "/current" {
		t.Errorf("LAZYRECALL_HOME must win over RECALL_HOME, got %q", got)
	}

	withEnv(t, map[string]string{"HOME": home})
	if got, want := LegacyDataDir(), filepath.Join(home, ".recall"); got != want {
		t.Errorf("LegacyDataDir() = %q, want %q", got, want)
	}
}

func TestEnvProfilePrefersTheCurrentVariable(t *testing.T) {
	withEnv(t, map[string]string{"LAZYRECALL_PROFILE": "", "RECALL_PROFILE": "old"})
	if got := envProfile(); got != "old" {
		t.Errorf("with only RECALL_PROFILE set, envProfile() = %q, want old", got)
	}
	withEnv(t, map[string]string{"LAZYRECALL_PROFILE": "new", "RECALL_PROFILE": "old"})
	if got := envProfile(); got != "new" {
		t.Errorf("LAZYRECALL_PROFILE must win, got %q", got)
	}
}

// A profile's name is its database filename (DBPath), so a name that depends
// on how the roots were *written down* rather than on what is actually on the
// machine would silently move a user to a different, empty database - losing
// every short handle, comment, and tag, which are the only data LazyRecall
// originates and cannot rebuild from any source.
//
// This is a regression test for exactly that: discovery is run twice against
// one synthetic machine, once with no config file and once with a config that
// names the single root explicitly, and the profile names must be identical.
// Before single_install became an explicit config field, the second form
// produced a profile called "default" instead of "claude".
func TestProfileNameDoesNotDependOnHowRootsWereConfigured(t *testing.T) {
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}

	names := func() []string {
		t.Helper()
		profiles, err := Discover()
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(profiles))
		for i, p := range profiles {
			out[i] = p.Name
		}
		return out
	}

	base := map[string]string{
		"HOME": home, "LAZYRECALL_CONFIG": "", "LAZYRECALL_CLAUDE_CONFIG_DIRS": "",
		"LAZYRECALL_PI_HOME":     filepath.Join(home, "nope-pi"),
		"LAZYRECALL_OMP_HOME":    filepath.Join(home, "nope-omp"),
		"LAZYRECALL_HERMES_HOME": filepath.Join(home, "nope-hermes"),
	}
	withEnv(t, base)
	fromDefaults := names()

	cfgPath := filepath.Join(home, "config.toml")
	cfg := "[sources.claude]\nroots = [\"" + claudeRoot + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"HOME": home, "LAZYRECALL_CONFIG": cfgPath, "LAZYRECALL_CLAUDE_CONFIG_DIRS": "",
		"LAZYRECALL_PI_HOME":     filepath.Join(home, "nope-pi"),
		"LAZYRECALL_OMP_HOME":    filepath.Join(home, "nope-omp"),
		"LAZYRECALL_HERMES_HOME": filepath.Join(home, "nope-hermes"),
	})
	fromExplicitConfig := names()

	if !reflect.DeepEqual(fromDefaults, fromExplicitConfig) {
		t.Errorf("profile names differ by how the roots were configured: defaults gave %v, an explicit single-root config gave %v",
			fromDefaults, fromExplicitConfig)
	}
	if len(fromDefaults) != 1 || fromDefaults[0] != "claude" {
		t.Errorf("expected exactly the profile \"claude\", got %v", fromDefaults)
	}
}
