package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rfist/lazyrecall/internal/config"
)

// withEnv sets env vars for the duration of the test and restores them
// afterward, including unsetting ones that were not previously set.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Redirecting HOME is not on its own enough to isolate a test from the
	// machine it runs on: $XDG_CONFIG_HOME outranks $HOME/.config in
	// config.Path, and CI runners set it (GitHub's ubuntu images do, its
	// macOS images and a typical laptop do not). Without this, these tests
	// isolate themselves on some machines and read the real user's config on
	// others - which is how the suite passed locally and on macOS and failed
	// on the first Linux CI run. Neutralised here rather than at fifteen call
	// sites so a new test cannot forget it; a caller that means to exercise
	// XDG itself can still set it explicitly and wins.
	if _, redirectsHome := kv["HOME"]; redirectsHome {
		if _, setsXDG := kv["XDG_CONFIG_HOME"]; !setsXDG {
			kv["XDG_CONFIG_HOME"] = ""
		}
	}
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

// baseSingleInstallEnv points every single_install source at a directory
// that does not exist, so a test asserting about claude alone is not also
// exercising whatever pi/omp/hermes happen to be configured on the machine
// running the suite.
func baseSingleInstallEnv(home string) map[string]string {
	return map[string]string{
		"LAZYRECALL_PI_HOME":     filepath.Join(home, "nope-pi"),
		"LAZYRECALL_OMP_HOME":    filepath.Join(home, "nope-omp"),
		"LAZYRECALL_HERMES_HOME": filepath.Join(home, "nope-hermes"),
	}
}

// TestDiscoverTwoClaudeRootsAndTwoSingleInstallSources covers the core
// contract (change group-sessions-in-one-index): four config roots on the
// machine (two claude, one pi, one omp) must discover as four installs, each
// with exactly one entry in Roots, and a single-install source's install is
// named after the source, never after its root's basename.
func TestDiscoverTwoClaudeRootsAndTwoSingleInstallSources(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, ".claude")
	personal := filepath.Join(home, ".claude-personal")
	mkClaudeRoot(t, work)
	mkClaudeRoot(t, personal)
	piHome := filepath.Join(home, "pi-home")
	ompHome := filepath.Join(home, "omp-home")
	if err := os.MkdirAll(piHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ompHome, 0o755); err != nil {
		t.Fatal(err)
	}

	withEnv(t, map[string]string{
		"HOME":                          home,
		"LAZYRECALL_CONFIG":             "",
		"LAZYRECALL_CLAUDE_CONFIG_DIRS": work + ":" + personal,
		"LAZYRECALL_PI_HOME":            piHome,
		"LAZYRECALL_OMP_HOME":           ompHome,
		"LAZYRECALL_HERMES_HOME":        filepath.Join(home, "nope-hermes"),
	})

	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 4 {
		t.Fatalf("expected 4 installs, got %d: %+v", len(profiles), profiles)
	}

	byName := map[string]Profile{}
	for _, p := range profiles {
		if len(p.Roots) != 1 {
			t.Errorf("install %q has %d roots, want exactly 1: %+v", p.Name, len(p.Roots), p.Roots)
		}
		byName[p.Name] = p
	}
	for _, want := range []string{"claude", "claude-personal", "pi", "omp"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected an install named %q, got %v", want, byName)
		}
	}
	if byName["claude"].Roots["claude"] == byName["claude-personal"].Roots["claude"] {
		t.Error("the two claude installs must not share a config root")
	}
	if byName["pi"].Roots["pi"] != piHome {
		t.Errorf("pi install root = %q, want %q", byName["pi"].Roots["pi"], piHome)
	}
	if byName["omp"].Roots["omp"] != ompHome {
		t.Errorf("omp install root = %q, want %q", byName["omp"].Roots["omp"], ompHome)
	}
}

// TestSingleInstallSourceNeverDuplicatesAcrossInstalls covers the case the
// old primary-profile bundling used to guard: a single_install source with
// only one existing root must produce exactly one install, not one per
// other install on the machine.
func TestSingleInstallSourceNeverDuplicatesAcrossInstalls(t *testing.T) {
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
		t.Fatalf("expected pi to produce exactly one install, got %d", withPi)
	}
}

// TestDiscoverSingleClaudeRootWithNothingSet covers the common fresh-machine
// case: a temp HOME with only a synthetic ~/.claude (a projects/ dir is
// enough for it to count as a real root) must discover exactly one install.
func TestDiscoverSingleClaudeRootWithNothingSet(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	withEnv(t, withHomeAnd(home, baseSingleInstallEnv(home)))
	profiles, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected exactly 1 install, got %d: %+v", len(profiles), profiles)
	}
	if profiles[0].Name != "claude" {
		t.Errorf("got %q, want claude", profiles[0].Name)
	}
}

func withHomeAnd(home string, extra map[string]string) map[string]string {
	out := map[string]string{"HOME": home, "LAZYRECALL_CONFIG": ""}
	for k, v := range extra {
		out[k] = v
	}
	return out
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

// TestTwoInstallsColliddingOnNameIsAnError covers the deliberate hard-error
// path: two single_install sources configured under the same source name
// would otherwise silently orphan one of them's handles/comments/tags.
func TestTwoInstallsCollidingOnNameIsAnError(t *testing.T) {
	home := t.TempDir()
	rootA := filepath.Join(home, "a")
	rootB := filepath.Join(home, "b")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	// A single_install source configured with two roots that both exist:
	// both would be named "pi", which must fail rather than silently drop
	// one.
	writeConfigFile(t, home, "[sources.pi]\nroots = [\""+rootA+"\", \""+rootB+"\"]\nsingle_install = true\n")
	withEnv(t, map[string]string{
		"HOME":                          home,
		"LAZYRECALL_CLAUDE_CONFIG_DIRS": "",
		"LAZYRECALL_PI_HOME":            "",
		"LAZYRECALL_OMP_HOME":           filepath.Join(home, "nope-omp"),
		"LAZYRECALL_HERMES_HOME":        filepath.Join(home, "nope-hermes"),
	})
	if _, err := Discover(); err == nil {
		t.Fatal("expected an error when two installs would collide on the same name")
	}
}

// TestProfileNameDoesNotDependOnHowRootsWereConfigured is the regression
// test for the same defect DBPath's old per-profile scheme guarded against:
// an install's name must depend only on what is actually on the machine,
// never on how the roots happened to be written down in the config file.
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

	base := withHomeAnd(home, map[string]string{
		"LAZYRECALL_CLAUDE_CONFIG_DIRS": "",
		"LAZYRECALL_PI_HOME":            filepath.Join(home, "nope-pi"),
		"LAZYRECALL_OMP_HOME":           filepath.Join(home, "nope-omp"),
		"LAZYRECALL_HERMES_HOME":        filepath.Join(home, "nope-hermes"),
	})
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
		t.Errorf("install names differ by how the roots were configured: defaults gave %v, an explicit single-root config gave %v",
			fromDefaults, fromExplicitConfig)
	}
	if len(fromDefaults) != 1 || fromDefaults[0] != "claude" {
		t.Errorf("expected exactly the install \"claude\", got %v", fromDefaults)
	}
}

func TestDBPathIsOneFileForEveryInstall(t *testing.T) {
	home := t.TempDir()
	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": ""})
	want := filepath.Join(home, ".lazyrecall", "index.db")
	if got := DBPath(); got != want {
		t.Errorf("DBPath() = %q, want %q", got, want)
	}
}

// The pre-rename RECALL_HOME fallback was removed (this project has one
// user, so no backward-compatibility code); RECALL_HOME alone must now be
// ignored and DataDir() must stay on the default.
func TestDataDirResolutionOrder(t *testing.T) {
	home := t.TempDir()

	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": "", "RECALL_HOME": ""})
	if got, want := DataDir(), filepath.Join(home, ".lazyrecall"); got != want {
		t.Errorf("default DataDir() = %q, want %q", got, want)
	}

	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": "", "RECALL_HOME": "/legacy"})
	if got, want := DataDir(), filepath.Join(home, ".lazyrecall"); got != want {
		t.Errorf("with only RECALL_HOME set, DataDir() = %q, want %q (RECALL_HOME must be ignored)", got, want)
	}

	withEnv(t, map[string]string{"HOME": home, "LAZYRECALL_HOME": "/current", "RECALL_HOME": "/legacy"})
	if got := DataDir(); got != "/current" {
		t.Errorf("LAZYRECALL_HOME must be honoured, got %q", got)
	}
}

// TestLabelResolvesConfiguredNameElseFallsBackToInstallName covers
// Profile.Label: a root with a configured label uses it, matched after
// filepath.Clean on both sides so a trailing slash in the config does not
// silently fail to match; a root with none falls back to the install name.
func TestLabelResolvesConfiguredNameElseFallsBackToInstallName(t *testing.T) {
	cfg := config.Config{
		Labels: map[string]string{"/home/u/.claude-personal/": "ccp"},
	}
	labeled := Profile{Name: "claude-personal", Roots: map[string]string{"claude": "/home/u/.claude-personal"}}
	if got := labeled.Label(cfg); got != "ccp" {
		t.Errorf("Label() = %q, want the configured label ccp", got)
	}

	unlabeled := Profile{Name: "claude", Roots: map[string]string{"claude": "/home/u/.claude"}}
	if got := unlabeled.Label(cfg); got != "claude" {
		t.Errorf("Label() = %q, want the install name as a fallback", got)
	}
}

// TestConfiguredLabelDoesNotFallBackToInstallName covers the P1 bug
// resolveAgentFilter's fix depends on: with no [labels] table at all, an
// install named the same as its own source (e.g. "claude") must not read as
// having a configured label of itself just because Profile.Label falls back
// to the install name - ConfiguredLabel is the strict form that reports
// "no" instead of the fallback, which is what lets a caller tell "this arg
// is a real configured label" apart from "this arg happens to equal the
// install's own name".
func TestConfiguredLabelDoesNotFallBackToInstallName(t *testing.T) {
	unlabeled := Profile{Name: "claude", Roots: map[string]string{"claude": "/home/u/.claude"}}
	if label, ok := unlabeled.ConfiguredLabel(config.Config{}); ok || label != "" {
		t.Errorf("ConfiguredLabel() = (%q, %v) with no [labels] configured, want (\"\", false)", label, ok)
	}

	cfg := config.Config{Labels: map[string]string{"/home/u/.claude-personal/": "ccp"}}
	if label, ok := unlabeled.ConfiguredLabel(cfg); ok || label != "" {
		t.Errorf("ConfiguredLabel() = (%q, %v) for a root with no label of its own, want (\"\", false)", label, ok)
	}

	labeled := Profile{Name: "claude-personal", Roots: map[string]string{"claude": "/home/u/.claude-personal"}}
	if label, ok := labeled.ConfiguredLabel(cfg); !ok || label != "ccp" {
		t.Errorf("ConfiguredLabel() = (%q, %v), want (\"ccp\", true)", label, ok)
	}
}
