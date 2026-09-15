package config

import (
	"fmt"
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
}

func TestFileOverridesSelectedValuesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, `
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

// TestSetRecordsFlagProvenance covers Set in isolation now that
// default_profile - the only field an env layer ever wrote - is gone: Set
// still exists purely so a flag override's provenance can be recorded
// against whatever key a caller applies one to.
func TestSetRecordsFlagProvenance(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hide.MinMessages = 5
	cfg.Set("hide.min_messages", OriginFlag)
	if cfg.Hide.MinMessages != 5 || cfg.Origins["hide.min_messages"] != OriginFlag {
		t.Errorf("after Set: MinMessages = %d, origin %q; want 5, %q",
			cfg.Hide.MinMessages, cfg.Origins["hide.min_messages"], OriginFlag)
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

// TestGroupsKeepFileOrder covers change group-sessions-in-one-index: groups
// are read from an ordered part of the file structure that TOML itself does
// not order (a map of tables), so Load must recover declaration order from
// toml.MetaData.Keys() rather than however Go happens to range over the
// decoded map. Three groups are declared out of alphabetical order so a
// sort-by-name bug would be caught.
func TestGroupsKeepFileOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, `
[groups.work]
paths = ["~/code", "~/work"]

[groups.aaa-personal]
paths = ["~/personal"]

[groups.misc]
paths = ["~/scratch"]
`)
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Groups) != 3 {
		t.Fatalf("len(Groups) = %d, want 3: %+v", len(cfg.Groups), cfg.Groups)
	}
	wantNames := []string{"work", "aaa-personal", "misc"}
	for i, want := range wantNames {
		if cfg.Groups[i].Name != want {
			t.Errorf("Groups[%d].Name = %q, want %q (declaration order, not alphabetical)", i, cfg.Groups[i].Name, want)
		}
	}
}

// TestGroupPathsAreExpandedAndCleaned covers change
// group-sessions-in-one-index: group paths are matched against a session's
// cwd as directory prefixes, so - like every other configured path - they
// are tilde/env-expanded at load, and additionally filepath.Clean-ed so a
// trailing slash or a "./" doesn't defeat a later prefix comparison.
func TestGroupPathsAreExpandedAndCleaned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MY_DATA", "data")
	path := writeConfig(t, `
[groups.work]
paths = ["~/code/", "$MY_DATA/work//sub", "~/$MY_DATA/x/./y"]
`)
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Groups) != 1 {
		t.Fatalf("len(Groups) = %d, want 1", len(cfg.Groups))
	}
	want := []string{
		filepath.Join(home, "code"),
		filepath.Join("data", "work", "sub"),
		filepath.Join(home, "data", "x", "y"),
	}
	if got := cfg.Groups[0].Paths; !reflect.DeepEqual(got, want) {
		t.Errorf("group paths = %v, want %v", got, want)
	}
}

// TestLabelsKeysAreExpandedAndCleaned covers change
// group-sessions-in-one-index (follow-up fix): labels live in a top-level
// [labels] table, keyed by config root, so the keys go through the same
// tilde/env expansion as sources.roots - otherwise a label configured
// against "~/.claude" would never match a root that Load has already
// expanded to an absolute path - and are filepath.Clean-ed, so a trailing
// slash in the config doesn't silently fail to match either.
func TestLabelsKeysAreExpandedAndCleaned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, `
[labels]
"~/.claude" = "cc"
"~/.claude-personal/" = "ccp"
`)
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		filepath.Join(home, ".claude"):          "cc",
		filepath.Join(home, ".claude-personal"): "ccp",
	}
	if got := cfg.Labels; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if cfg.Origins["labels"] != OriginFile {
		t.Errorf("Origins[labels] = %q, want %q", cfg.Origins["labels"], OriginFile)
	}
}

// TestLabelsSurviveConfigWithNoSourcesTable is the regression this follow-up
// fix exists for: a config with [labels] and [groups.*] but no [sources.*]
// table at all must still discover the default claude installs with their
// labels intact. Before the fix, the obvious way to write labels -
// `[sources.claude]\nlabels = {...}` - replaced the source's Roots with the
// zero value (a [sources.X] table in the file owns that source entirely),
// silently dropping every claude install from discovery. This test proves
// the top-level table doesn't touch Sources at all.
func TestLabelsSurviveConfigWithNoSourcesTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, `
[labels]
"~/.claude" = "cc"
"~/.claude-personal" = "ccp"

[groups.work]
paths = ["~/code"]
`)
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(home, ".claude-personal"), filepath.Join(home, ".claude")}
	if got := cfg.Sources["claude"].Roots; !reflect.DeepEqual(got, want) {
		t.Errorf("claude roots = %v, want the untouched defaults %v (a [labels] table must not disturb [sources.*])", got, want)
	}
	wantLabels := map[string]string{
		filepath.Join(home, ".claude"):          "cc",
		filepath.Join(home, ".claude-personal"): "ccp",
	}
	if got := cfg.Labels; !reflect.DeepEqual(got, wantLabels) {
		t.Errorf("labels = %v, want %v", got, wantLabels)
	}
}

// TestEmptyLabelValueIsConfigError covers the validation rule: a label with
// no value is rejected rather than silently falling back to the install's
// own name.
func TestEmptyLabelValueIsConfigError(t *testing.T) {
	path := writeConfig(t, "[labels]\n\"~/.claude\" = \"\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected a config error for an empty label value")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the config file", err.Error())
	}
}

// TestDuplicateLabelIsConfigError covers the validation rule: two roots
// mapping to the same label would make that label ambiguous as a --agent
// value, so it is a hard error naming both roots.
func TestDuplicateLabelIsConfigError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, "[labels]\n\"~/.claude\" = \"cc\"\n\"~/.claude-personal\" = \"cc\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected a config error for two roots sharing a label")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q does not name the config file", msg)
	}
	claude, personal := filepath.Join(home, ".claude"), filepath.Join(home, ".claude-personal")
	if !strings.Contains(msg, claude) || !strings.Contains(msg, personal) {
		t.Errorf("error %q does not name both roots (%q, %q)", msg, claude, personal)
	}
}

// TestGroupsAndLabelsDefaultEmpty covers change group-sessions-in-one-index:
// with no config file, there are no groups and no labels, and
// Browse.DefaultGroup is "" (All) - the zero-config behavior the plan
// requires to look exactly like today.
func TestGroupsAndLabelsDefaultEmpty(t *testing.T) {
	t.Setenv("LAZYRECALL_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Groups) != 0 {
		t.Errorf("Groups = %+v, want empty", cfg.Groups)
	}
	if cfg.Labels != nil {
		t.Errorf("labels = %v, want nil", cfg.Labels)
	}
	if cfg.Browse.DefaultGroup != "" {
		t.Errorf("Browse.DefaultGroup = %q, want empty", cfg.Browse.DefaultGroup)
	}
	if cfg.Origins["browse.default_group"] != OriginDefault {
		t.Errorf("Origins[browse.default_group] = %q, want %q", cfg.Origins["browse.default_group"], OriginDefault)
	}
}

// TestBrowseDefaultGroupFromFile covers change group-sessions-in-one-index:
// browse.default_group is read like any other file-set value, with its
// origin recorded as file.
func TestBrowseDefaultGroupFromFile(t *testing.T) {
	path := writeConfig(t, "[browse]\ndefault_group = \"work\"\n\n[groups.work]\npaths = [\"~/x\"]\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browse.DefaultGroup != "work" {
		t.Errorf("Browse.DefaultGroup = %q, want work", cfg.Browse.DefaultGroup)
	}
	if cfg.Origins["browse.default_group"] != OriginFile {
		t.Errorf("Origins[browse.default_group] = %q, want %q", cfg.Origins["browse.default_group"], OriginFile)
	}
}

// TestInvalidDefaultGroupIsConfigError is the P1 regression test for a
// review finding: browse.default_group used to be applied unvalidated
// (unlike --group, which always went through search.ValidateGroup), so a
// typo like "typo" reached the browser, resolved to nothing, and opened on
// an empty view with exit 0 - no error naming the file or the bad value.
func TestInvalidDefaultGroupIsConfigError(t *testing.T) {
	path := writeConfig(t, "[browse]\ndefault_group = \"typo\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected a config error for an invalid default_group, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the config file", err.Error())
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error %q does not name the offending value", err.Error())
	}
}

// TestDefaultGroupAcceptsEveryValidValue covers every value
// browse.default_group may take: empty and its explicit spelling "all" (both
// mean no group filter and normalize to ""), the two built-in views, and a
// group actually declared in the same file.
func TestDefaultGroupAcceptsEveryValidValue(t *testing.T) {
	cases := []struct {
		value string
		want  string // cfg.Browse.DefaultGroup after Load, post-normalization
	}{
		{"", ""},
		{"all", ""},
		{"archive", "archive"},
		{"unknown", "unknown"},
		{"work", "work"},
	}
	for _, c := range cases {
		path := writeConfig(t, fmt.Sprintf("[browse]\ndefault_group = %q\n\n[groups.work]\npaths = [\"~/x\"]\n", c.value))
		t.Setenv("LAZYRECALL_CONFIG", path)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("default_group = %q: unexpected error: %v", c.value, err)
		}
		if cfg.Browse.DefaultGroup != c.want {
			t.Errorf("default_group = %q: Browse.DefaultGroup = %q, want %q", c.value, cfg.Browse.DefaultGroup, c.want)
		}
	}
}

// TestReservedGroupNameIsConfigError covers change
// group-sessions-in-one-index: "all", "archive" and "unknown" are filter
// values Filter.Group already gives a fixed meaning, so a configured group
// of the same name (in any case) would be unreachable and is a hard error
// naming the file and the offending group.
func TestReservedGroupNameIsConfigError(t *testing.T) {
	for _, name := range []string{"all", "archive", "unknown", "Archive", "UNKNOWN"} {
		path := writeConfig(t, "[groups."+name+"]\npaths = [\"~/x\"]\n")
		t.Setenv("LAZYRECALL_CONFIG", path)

		_, err := Load()
		if err == nil {
			t.Fatalf("group name %q: expected a config error, got nil", name)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("group name %q: error %q does not name the config file", name, err.Error())
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("group name %q: error %q does not name the offending group", name, err.Error())
		}
	}
}

// TestEmptyGroupNameIsConfigError covers the same rule for an empty group
// name.
func TestEmptyGroupNameIsConfigError(t *testing.T) {
	path := writeConfig(t, "[groups.\"\"]\npaths = [\"~/x\"]\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected a config error for an empty group name, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the config file", err.Error())
	}
}

// TestGroupColorAcceptsNamesBrightVariantsAndHex covers every accepted
// [groups.<name>].color spelling (change per-group-colors): the 8 plain
// ANSI names, their bright- variants, a hex triplet, and case-insensitivity
// on both - each normalizes to its lowercased form and records
// groups.<name>.color as file-origin.
func TestGroupColorAcceptsNamesBrightVariantsAndHex(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"blue", "blue"},
		{"BLUE", "blue"},
		{"Blue", "blue"},
		{"bright-blue", "bright-blue"},
		{"Bright-Blue", "bright-blue"},
		{"red", "red"},
		{"bright-white", "bright-white"},
		{"#3355ff", "#3355ff"},
		{"#3355FF", "#3355ff"},
	}
	for _, c := range cases {
		path := writeConfig(t, "[groups.work]\npaths = [\"~/code\"]\ncolor = \""+c.configured+"\"\n")
		t.Setenv("LAZYRECALL_CONFIG", path)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("color %q: unexpected error: %v", c.configured, err)
		}
		if len(cfg.Groups) != 1 {
			t.Fatalf("color %q: len(Groups) = %d, want 1", c.configured, len(cfg.Groups))
		}
		if got := cfg.Groups[0].Color; got != c.want {
			t.Errorf("color %q: Groups[0].Color = %q, want %q", c.configured, got, c.want)
		}
		if cfg.Origins["groups.work.color"] != OriginFile {
			t.Errorf("color %q: Origins[groups.work.color] = %q, want %q", c.configured, cfg.Origins["groups.work.color"], OriginFile)
		}
	}
}

// TestGroupWithNoColorLeavesItEmpty covers the common case: a [groups.*]
// table with no color key at all leaves Color empty, the "no color
// configured, keep today's styling" state - not an error and not a
// fabricated default.
func TestGroupWithNoColorLeavesItEmpty(t *testing.T) {
	path := writeConfig(t, "[groups.work]\npaths = [\"~/code\"]\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Groups) != 1 {
		t.Fatalf("len(Groups) = %d, want 1", len(cfg.Groups))
	}
	if cfg.Groups[0].Color != "" {
		t.Errorf("Groups[0].Color = %q, want empty", cfg.Groups[0].Color)
	}
	if cfg.Origins["groups.work.color"] != OriginFile {
		t.Errorf("Origins[groups.work.color] = %q, want %q (the table itself is file-owned)", cfg.Origins["groups.work.color"], OriginFile)
	}
}

// TestInvalidGroupColorIsConfigError covers the validation rule: a color
// that is neither a recognized ANSI name (or bright- variant) nor a
// "#rrggbb" hex triplet is a hard error naming the config file, the
// offending group, and the bad value - the same shape
// TestReservedGroupNameIsConfigError already expects of a group-name
// mistake, so an invalid color reads like every other config mistake.
func TestInvalidGroupColorIsConfigError(t *testing.T) {
	for _, bad := range []string{"maroon", "#zzzzzz", "#fff", "#3355ff00", "bright-orange"} {
		path := writeConfig(t, "[groups.work]\npaths = [\"~/code\"]\ncolor = \""+bad+"\"\n")
		t.Setenv("LAZYRECALL_CONFIG", path)

		_, err := Load()
		if err == nil {
			t.Fatalf("color %q: expected a config error, got nil", bad)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("color %q: error %q does not name the config file", bad, err.Error())
		}
		if !strings.Contains(err.Error(), "work") {
			t.Errorf("color %q: error %q does not name the offending group", bad, err.Error())
		}
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("color %q: error %q does not name the offending value", bad, err.Error())
		}
	}
}

// ---------------------------------------------------------------------
// Archive/Unknown view colors (change archive-unknown-colors)
// ---------------------------------------------------------------------

// TestArchiveAndUnknownColorTablesAreAccepted covers the color-only escape
// hatch from the ordinary reserved-name rejection: [groups.archive] and
// [groups.unknown] may set color, land in Config.ArchiveColor/UnknownColor
// with the usual color normalization, are attributed to the file, and never
// appear in Config.Groups - they are not real groups and must never show up
// as an extra row or a valid --group value.
func TestArchiveAndUnknownColorTablesAreAccepted(t *testing.T) {
	path := writeConfig(t, "[groups.archive]\ncolor = \"red\"\n\n[groups.unknown]\ncolor = \"WHITE\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ArchiveColor != "red" {
		t.Errorf("ArchiveColor = %q, want %q", cfg.ArchiveColor, "red")
	}
	if cfg.UnknownColor != "white" {
		t.Errorf("UnknownColor = %q, want %q (normalized lowercase)", cfg.UnknownColor, "white")
	}
	if len(cfg.Groups) != 0 {
		t.Errorf("Groups = %+v, want empty - archive/unknown are not real groups", cfg.Groups)
	}
	if cfg.Origins["groups.archive.color"] != OriginFile {
		t.Errorf("Origins[groups.archive.color] = %q, want %q", cfg.Origins["groups.archive.color"], OriginFile)
	}
	if cfg.Origins["groups.unknown.color"] != OriginFile {
		t.Errorf("Origins[groups.unknown.color] = %q, want %q", cfg.Origins["groups.unknown.color"], OriginFile)
	}
}

// TestArchiveOrUnknownPathsIsConfigError covers the restriction that a
// [groups.archive]/[groups.unknown] table may set only color: paths would
// imply the built-in view claims sessions by working directory the way a
// real group does, which it does not - archive/unknown are query-time
// views, not something a session is ever filed under by path.
func TestArchiveOrUnknownPathsIsConfigError(t *testing.T) {
	for _, name := range []string{"archive", "unknown"} {
		path := writeConfig(t, "[groups."+name+"]\npaths = [\"~/x\"]\n")
		t.Setenv("LAZYRECALL_CONFIG", path)

		_, err := Load()
		if err == nil {
			t.Fatalf("group %q: expected a config error for a paths key, got nil", name)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("group %q: error %q does not name the config file", name, err.Error())
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("group %q: error %q does not name the offending table", name, err.Error())
		}
	}
}

// TestArchiveOrUnknownInvalidColorIsConfigError covers the validation rule
// applying to these tables' color exactly as it does to a real group's.
func TestArchiveOrUnknownInvalidColorIsConfigError(t *testing.T) {
	path := writeConfig(t, "[groups.archive]\ncolor = \"maroon\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected a config error for an invalid archive color, got nil")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "maroon") {
		t.Errorf("error %q does not name the file and the offending value", err.Error())
	}
}

// TestGroupsAllStillReservedArchiveUnknownCaseInsensitive covers the two
// ends of the reservation rule after the archive/unknown color-only
// exception was carved out: "all" has no such exception and is rejected
// exactly as before, and archive/unknown are recognized case-insensitively
// (an upper-case [groups.Archive] table behaves like [groups.archive]).
func TestGroupsAllStillReservedArchiveUnknownCaseInsensitive(t *testing.T) {
	path := writeConfig(t, "[groups.all]\ncolor = \"red\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)
	_, err := Load()
	if err == nil {
		t.Fatal("expected [groups.all] to still be a config error, got nil")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "all") {
		t.Errorf("error %q does not name the file and %q", err.Error(), "all")
	}

	path = writeConfig(t, "[groups.Archive]\ncolor = \"red\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error for [groups.Archive]: %v", err)
	}
	if cfg.ArchiveColor != "red" {
		t.Errorf("ArchiveColor = %q, want %q (case-insensitive table name)", cfg.ArchiveColor, "red")
	}
	if len(cfg.Groups) != 0 {
		t.Errorf("Groups = %+v, want empty", cfg.Groups)
	}
}

// TestExistingWorkGroupConfigUnaffectedByArchiveUnknownColors is the
// regression check: an existing config with only [groups.work] color still
// parses exactly as before - archive/unknown stay unset, and Groups still
// holds just the one configured group.
func TestExistingWorkGroupConfigUnaffectedByArchiveUnknownColors(t *testing.T) {
	path := writeConfig(t, "[groups.work]\ncolor = \"yellow\"\n")
	t.Setenv("LAZYRECALL_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Groups) != 1 || cfg.Groups[0].Name != "work" || cfg.Groups[0].Color != "yellow" {
		t.Fatalf("Groups = %+v, want [{work yellow}]", cfg.Groups)
	}
	if cfg.ArchiveColor != "" || cfg.UnknownColor != "" {
		t.Errorf("ArchiveColor/UnknownColor = %q/%q, want both empty", cfg.ArchiveColor, cfg.UnknownColor)
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
