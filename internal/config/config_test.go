package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsWhenNoFileExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LAZYRECALL_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProfile != "" {
		t.Errorf("DefaultProfile = %q, want empty (program's own rule)", cfg.DefaultProfile)
	}
	if !cfg.Hide.NonInteractive {
		t.Error("Hide.NonInteractive = false, want true")
	}
	if cfg.Hide.MinMessages != 0 {
		t.Errorf("Hide.MinMessages = %d, want 0", cfg.Hide.MinMessages)
	}
	if len(cfg.Hide.Paths) != 0 {
		t.Errorf("Hide.Paths = %v, want empty", cfg.Hide.Paths)
	}
	if cfg.Browse.ShowArchived {
		t.Error("Browse.ShowArchived = true, want false")
	}

	// Default claude roots expand to the pinned home, personal first, the
	// order candidateClaudeRoots in internal/profile probes them.
	want := []string{filepath.Join(home, ".claude-personal"), filepath.Join(home, ".claude")}
	if got := cfg.Sources["claude"].Roots; !reflect.DeepEqual(got, want) {
		t.Errorf("claude roots = %v, want %v", got, want)
	}

	if cfg.Origins["hide.non_interactive"] != OriginDefault {
		t.Errorf("Origins[hide.non_interactive] = %q, want %q", cfg.Origins["hide.non_interactive"], OriginDefault)
	}
	if cfg.Origins["default_profile"] != OriginDefault {
		t.Errorf("Origins[default_profile] = %q, want %q", cfg.Origins["default_profile"], OriginDefault)
	}
}

func TestFileOverridesSelectedValuesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, `
default_profile = "work"

[sources.claude]
roots   = ["/opt/claude"]
resume  = ["claude", "--resume", "{id}"]
env_var = "CLAUDE_CONFIG_DIR"
`)
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q, want work", cfg.DefaultProfile)
	}
	if cfg.Origins["default_profile"] != OriginFile {
		t.Errorf("Origins[default_profile] = %q, want %q", cfg.Origins["default_profile"], OriginFile)
	}
	if got := cfg.Sources["claude"].Roots; len(got) != 1 || got[0] != "/opt/claude" {
		t.Errorf("claude roots = %v, want [/opt/claude]", got)
	}
	if cfg.Origins["sources.claude.roots"] != OriginFile {
		t.Errorf("Origins[sources.claude.roots] = %q, want %q", cfg.Origins["sources.claude.roots"], OriginFile)
	}

	// A source the file never mentions keeps its defaults, expanded like any
	// other root path.
	if got := cfg.Sources["pi"].Roots; len(got) != 1 || got[0] != filepath.Join(home, ".pi") {
		t.Errorf("pi roots = %v, want [%s]", got, filepath.Join(home, ".pi"))
	}
	if got := cfg.Sources["pi"].Resume; len(got) != 3 || got[0] != "pi" || got[1] != "--session" {
		t.Errorf("pi resume = %v, want [pi --session {id}]", got)
	}
	if cfg.Origins["sources.pi.roots"] != OriginDefault {
		t.Errorf("Origins[sources.pi.roots] = %q, want %q", cfg.Origins["sources.pi.roots"], OriginDefault)
	}
}

func TestFileSourceTableReplacesThatSourceEntirely(t *testing.T) {
	// A [sources.X] table is an all-or-nothing override: fields the table
	// omits fall to zero and are still attributed to the file, they do not
	// keep the defaults.
	path := writeConfig(t, "[sources.pi]\nroots = [\"/opt/pi\"]\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources["pi"].Roots; len(got) != 1 || got[0] != "/opt/pi" {
		t.Errorf("pi roots = %v, want [/opt/pi]", got)
	}
	if cfg.Sources["pi"].Resume != nil {
		t.Errorf("pi resume = %v, want nil (replaced by the file)", cfg.Sources["pi"].Resume)
	}
	if cfg.Sources["pi"].EnvVar != "" {
		t.Errorf("pi env_var = %q, want empty (replaced by the file)", cfg.Sources["pi"].EnvVar)
	}
	if cfg.Origins["sources.pi.resume"] != OriginFile {
		t.Errorf("Origins[sources.pi.resume] = %q, want %q", cfg.Origins["sources.pi.resume"], OriginFile)
	}
}

func TestLoadExpandsTildeAndEnvVarsInPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MY_DATA", "data")
	path := writeConfig(t, "[sources.pi]\nroots = [\"~/pi\", \"$MY_DATA/pi\", \"~/$MY_DATA/sub\"]\n\n[hide]\npaths = [\"~/tmp/*\", \"$MY_DATA/tmp/*\"]\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wantRoots := []string{filepath.Join(home, "pi"), "data/pi", filepath.Join(home, "data", "sub")}
	if got := cfg.Sources["pi"].Roots; !reflect.DeepEqual(got, wantRoots) {
		t.Errorf("pi roots = %v, want %v", got, wantRoots)
	}
	wantPaths := []string{filepath.Join(home, "tmp", "*"), filepath.Join("data", "tmp", "*")}
	if got := cfg.Hide.Paths; !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("hide paths = %v, want %v", got, wantPaths)
	}
}

func TestHidePathsNormaliseDoubleGlob(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, "[hide]\npaths = [\"/tmp/**/x\", \"~/scratch/**\"]\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// SQLite GLOB's '*' already crosses '/', so '**' would be a meaningless
	// doubling; it is collapsed to '*' on load.
	want := []string{"/tmp/*/x", filepath.Join(home, "scratch", "*")}
	if got := cfg.Hide.Paths; !reflect.DeepEqual(got, want) {
		t.Errorf("hide paths = %v, want %v", got, want)
	}
}

func TestLoadMalformedFileReturnsErrorWithLocation(t *testing.T) {
	path := writeConfig(t, "min_messages = \n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for a malformed config file")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not name the config file %q", msg, path)
	}
	if !strings.Contains(msg, "line 1") {
		t.Errorf("error %q does not carry a line number", msg)
	}
}

func TestPrecedenceFlagOverridesEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, "default_profile = \"a\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)
	t.Setenv("LAZYRECALL_PROFILE", "b")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProfile != "a" {
		t.Errorf("file layer DefaultProfile = %q, want a", cfg.DefaultProfile)
	}

	cfg.ApplyEnv()
	if cfg.DefaultProfile != "b" || cfg.Origins["default_profile"] != OriginEnv {
		t.Errorf("after ApplyEnv: DefaultProfile = %q, origin %q; want b, %q",
			cfg.DefaultProfile, cfg.Origins["default_profile"], OriginEnv)
	}

	cfg.DefaultProfile = "c"
	cfg.Set("default_profile", OriginFlag)
	if cfg.DefaultProfile != "c" || cfg.Origins["default_profile"] != OriginFlag {
		t.Errorf("after Set: DefaultProfile = %q, origin %q; want c, %q",
			cfg.DefaultProfile, cfg.Origins["default_profile"], OriginFlag)
	}
}

func TestPathResolutionOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LAZYRECALL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	if got, want := Path(), filepath.Join(home, ".config", "lazyrecall", "config.toml"); got != want {
		t.Errorf("with nothing set, Path() = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if got, want := Path(), filepath.Join(home, "xdg", "lazyrecall", "config.toml"); got != want {
		t.Errorf("with XDG_CONFIG_HOME set, Path() = %q, want %q", got, want)
	}

	t.Setenv("LAZYRECALL_CONFIG", filepath.Join(home, "explicit.toml"))
	if got, want := Path(), filepath.Join(home, "explicit.toml"); got != want {
		t.Errorf("with LAZYRECALL_CONFIG set, Path() = %q, want %q", got, want)
	}
}

// single_install decides whether a source names a profile of its own or is
// bundled into the primary one, and a profile's name is its database
// filename - so it is stated per source, never inferred. See
// TestProfileNameDoesNotDependOnHowRootsWereConfigured in internal/profile
// for the failure this prevents.
func TestSingleInstallDefaultsAndOverride(t *testing.T) {
	t.Setenv("LAZYRECALL_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"claude": false, "pi": true, "omp": true, "hermes": true,
	} {
		if got := cfg.Sources[name].SingleInstall; got != want {
			t.Errorf("%s single_install = %v, want %v", name, got, want)
		}
	}

	path := writeConfig(t, "[sources.pi]\nroots = [\"/tmp/pi\"]\nsingle_install = false\n")
	t.Setenv("LAZYRECALL_CONFIG", path)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources["pi"].SingleInstall {
		t.Error("the file set single_install = false for pi but it stayed true")
	}
	if got := cfg.Origins["sources.pi.single_install"]; got != OriginFile {
		t.Errorf("origin for sources.pi.single_install = %q, want %q", got, OriginFile)
	}
	if got := cfg.Origins["sources.claude.single_install"]; got != OriginDefault {
		t.Errorf("origin for an untouched source = %q, want %q", got, OriginDefault)
	}
}
