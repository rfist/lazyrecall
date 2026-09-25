package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rfist/lazyrecall/internal/annotate"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/schema"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// newTestFlagSet builds the same FlagSet configuration every real
// subcommand parses with: the global flags (json/no-refresh) plus the
// filter flags (agent/repo/tag/since), plus a synthetic value-taking
// "label" flag standing in for a generic option (--profile served this
// purpose before change group-sessions-in-one-index removed it) - the
// interleaving behaviour under test does not depend on which flag it is.
func newTestFlagSet() (*flag.FlagSet, *string, *string, *string, *string) {
	fs := flag.NewFlagSet("lazyrecall", flag.ContinueOnError)
	labelFlag := fs.String("label", "", "a generic value-taking flag, for testing interleaving")
	fs.Bool("json", false, "machine-readable JSON output")
	fs.Bool("no-refresh", false, "skip the automatic refresh")
	agent := fs.String("agent", "", "filter by agent")
	repo := fs.String("repo", "", "filter by repository root or working directory")
	tag := fs.String("tag", "", "filter by tag")
	fs.Int("since", 0, "only sessions active in the last N days")
	return fs, labelFlag, agent, repo, tag
}

func parse(t *testing.T, args []string) ([]string, *string, *string, *string, *string) {
	t.Helper()
	fs, label, agent, repo, tag := newTestFlagSet()
	positionals, err := parseInterleaved(fs, args)
	if err != nil {
		t.Fatalf("parseInterleaved(%q) error: %v", args, err)
	}
	return positionals, label, agent, repo, tag
}

// The following cover change fix-cli-usability-defects, tasks 1.1-1.4:
// options are accepted before, after, and between positional arguments and
// interpreted identically in every order; the end-of-options marker keeps
// dash-leading text as text.

func TestOptionAfterPositionalIsNotAbsorbed(t *testing.T) {
	positionals, label, _, _, _ := parse(t, []string{"buddy", "--label=work"})
	if got := *label; got != "work" {
		t.Errorf("label = %q, want the option to take effect", got)
	}
	if !reflect.DeepEqual(positionals, []string{"buddy"}) {
		t.Errorf("positionals = %v, want the phrase alone without the option text", positionals)
	}
}

func TestOptionBeforePositionalBehavesIdentically(t *testing.T) {
	before, beforeLabel, _, _, _ := parse(t, []string{"--label=work", "buddy"})
	after, afterLabel, _, _, _ := parse(t, []string{"buddy", "--label=work"})
	if *beforeLabel != *afterLabel || *beforeLabel != "work" {
		t.Errorf("label before=%q after=%q, want identical and set", *beforeLabel, *afterLabel)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(before, []string{"buddy"}) {
		t.Errorf("positionals before=%v after=%v, want identical", before, after)
	}
}

func TestOptionBetweenPositionals(t *testing.T) {
	// A value-taking flag between two positionals, in a command that takes
	// more than one positional (comment add SESSION_ID TEXT...).
	positionals, _, agent, _, _ := parse(t, []string{"add", "--agent=pi", "sess-1", "note text"})
	if got := *agent; got != "pi" {
		t.Errorf("agent = %q, want the between-positionals option to take effect", got)
	}
	if !reflect.DeepEqual(positionals, []string{"add", "sess-1", "note text"}) {
		t.Errorf("positionals = %v, want all three preserved in order", positionals)
	}
}

func TestEveryOrderProducesIdenticalResult(t *testing.T) {
	// Task 1.4: all six permutations of [phrase, --agent, --repo] must
	// produce the same positional list and the same flag values.
	perms := [][]string{
		{"--agent=pi", "--repo=/x", "phrase"},
		{"--agent=pi", "phrase", "--repo=/x"},
		{"phrase", "--agent=pi", "--repo=/x"},
		{"phrase", "--repo=/x", "--agent=pi"},
		{"--repo=/x", "phrase", "--agent=pi"},
		{"--repo=/x", "--agent=pi", "phrase"},
	}
	var want []string
	var wantAgent, wantRepo string
	for i, args := range perms {
		positionals, _, agent, repo, _ := parse(t, args)
		if i == 0 {
			want, wantAgent, wantRepo = positionals, *agent, *repo
			continue
		}
		if !reflect.DeepEqual(positionals, want) {
			t.Errorf("order %v: positionals = %v, want %v", args, positionals, want)
		}
		if *agent != wantAgent || *repo != wantRepo {
			t.Errorf("order %v: agent=%q repo=%q, want agent=%q repo=%q", args, *agent, *repo, wantAgent, wantRepo)
		}
	}
}

func TestEndOfOptionsMarkerKeepsDashTextAsText(t *testing.T) {
	// Task 1.3: after "--" everything is positional, including text that
	// begins with a dash and would otherwise parse as an option.
	positionals, label, _, _, _ := parse(t, []string{"--", "--label=work"})
	if got := *label; got != "" {
		t.Errorf("label = %q, want unset - text after -- must not be parsed as an option", got)
	}
	if !reflect.DeepEqual(positionals, []string{"--label=work"}) {
		t.Errorf("positionals = %v, want the dash-leading text preserved", positionals)
	}
}

func TestEndOfOptionsMarkerMidArgs(t *testing.T) {
	positionals, label, agent, _, _ := parse(t, []string{"--agent=pi", "--", "--label=work", "buddy"})
	if got := *agent; got != "pi" {
		t.Errorf("agent = %q, want the option before -- to take effect", got)
	}
	if got := *label; got != "" {
		t.Errorf("label = %q, want text after -- to stay text", got)
	}
	if !reflect.DeepEqual(positionals, []string{"--label=work", "buddy"}) {
		t.Errorf("positionals = %v, want everything after -- preserved in order", positionals)
	}
}

func TestUnknownFlagStillErrors(t *testing.T) {
	fs, _, _, _, _ := newTestFlagSet()
	if _, err := parseInterleaved(fs, []string{"phrase", "--nope"}); err == nil {
		t.Error("expected an error for an undefined flag, got nil")
	}
}

func TestSpaceSeparatedFlagValueAfterPositional(t *testing.T) {
	positionals, _, agent, _, _ := parse(t, []string{"buddy", "--agent", "pi"})
	if got := *agent; got != "pi" {
		t.Errorf("agent = %q, want the space-separated value to be consumed", got)
	}
	if !reflect.DeepEqual(positionals, []string{"buddy"}) {
		t.Errorf("positionals = %v", positionals)
	}
}

// TestGroupsCommandOutput covers change group-sessions-in-one-index's
// replacement for `profiles`: `lazyrecall groups` must list each configured
// group (in config order) with its paths and non-archived session count,
// the archive count, and an installs section naming every discovered
// install readably - never an internal data structure representation like
// the old `profiles` command's raw %v of a map.
func TestGroupsCommandOutput(t *testing.T) {
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(home, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", claudeRoot)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(home, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(home, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(home, "nope-hermes"))
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[groups.work]\npaths = [\"/code\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAZYRECALL_CONFIG", cfgPath)

	installs, err := profile.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 {
		t.Fatalf("expected exactly 1 install, got %d: %+v", len(installs), installs)
	}
	install := installs[0].Name
	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db := &sqlitex.Runner{DBPath: profile.DBPath()}
	if _, err := schema.Open(db); err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "cwd", "install"}, []map[string]any{
		{"id": "claude:" + install + ":1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1", "end_state": "completed", "resumable": 1, "cwd": "/code/proj", "install": install},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	out, runErr := runCaptured(t, []string{"groups", "--no-refresh"})
	if runErr != nil {
		t.Fatalf("run(groups --no-refresh): %v", runErr)
	}
	if strings.Contains(out, "map[") {
		t.Errorf("expected no Go map representation in the output, got %q", out)
	}
	if !strings.Contains(out, "work (/code): 1") {
		t.Errorf("expected the configured group with its path and count, got:\n%s", out)
	}
	if !strings.Contains(out, "archive 0") {
		t.Errorf("expected an archive count line, got:\n%s", out)
	}
	if !strings.Contains(out, "installs:") || !strings.Contains(out, install) {
		t.Errorf("expected an installs section naming %q, got:\n%s", install, out)
	}
}

// TestVersionFlagReportsBuildIdentity covers the diagnostic this change
// adds (fix-herdr-resume-delegation): `-v`, `--version`, and `version`
// must all report a build identifier, so that when a bug report and the
// binary under test disagree - the failure mode that made this
// investigation harder than it needed to be - it's possible to tell which
// build is actually running. Defaults to "dev"/"unknown" when built
// without the `-X main.version=...`/`-X main.buildTime=...` ldflags (e.g.
// `go test`, `go run`, a plain `go build`), and reflects whatever those
// flags set otherwise (verified separately - see fyi.md - by building the
// real binary with -ldflags).
func TestVersionFlagReportsBuildIdentity(t *testing.T) {
	for _, arg := range []string{"-v", "--version", "version"} {
		t.Run(arg, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stdout
			os.Stdout = w
			runErr := run([]string{arg})
			w.Close()
			os.Stdout = old
			if runErr != nil {
				t.Fatalf("run(%q): %v", arg, runErr)
			}
			out, _ := io.ReadAll(r)
			got := strings.TrimSpace(string(out))
			if !strings.HasPrefix(got, "lazyrecall ") {
				t.Errorf("expected output to start with %q, got %q", "lazyrecall ", got)
			}
			if !strings.Contains(got, version) || !strings.Contains(got, buildTime) {
				t.Errorf("expected the current version (%q) and build time (%q) to appear, got %q", version, buildTime, got)
			}
		})
	}
}

// The following cover change fix-resume-session-identity, task 2: a
// resumed agent must be started under its own session's install
// configuration. profileNameFrom itself moved to internal/session as
// session.InstallFromID (change group-sessions-in-one-index) and is tested
// there; these test the resume-time consumer of it.

// TestResumeProfileEnvNonClaudeSourceNeverVaries covers task 2.2: pi, omp,
// and hermes are each bundled into exactly one profile on this machine
// (internal/profile) - their profile configuration never varies, so no
// environment override is ever needed and resolution always succeeds.
func TestResumeProfileEnvNonClaudeSourceNeverVaries(t *testing.T) {
	for _, source := range []string{"pi", "omp", "hermes"} {
		_, env, ok, reason := resumeSource(source, "whatever-profile-name")
		if !ok {
			t.Errorf("%s: expected ok=true, got reason %q", source, reason)
		}
		if len(env) != 0 {
			t.Errorf("%s: expected no environment override, got %v", source, env)
		}
	}
}

// TestResumeProfileEnvResolvesClaudeConfigDir covers design.md decision 3:
// a claude session belonging to a non-default installation must resolve to
// that installation's own config root, not the default one.
func TestResumeProfileEnvResolvesClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, ".claude")
	personal := filepath.Join(dir, ".claude-personal")
	for _, root := range []string{work, personal} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "history.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", work+":"+personal)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(dir, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(dir, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(dir, "nope-hermes"))

	_, env, ok, reason := resumeSource("claude", "claude-personal")
	if !ok {
		t.Fatalf("expected ok=true, got reason %q", reason)
	}
	if env["CLAUDE_CONFIG_DIR"] != personal {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", env["CLAUDE_CONFIG_DIR"], personal)
	}

	// The other (default/work) installation must resolve to its own root,
	// not the personal one - proving this isn't just always returning
	// whichever root happens to be discovered first.
	_, env2, ok2, reason2 := resumeSource("claude", "claude")
	if !ok2 {
		t.Fatalf("expected ok=true, got reason %q", reason2)
	}
	if env2["CLAUDE_CONFIG_DIR"] != work {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", env2["CLAUDE_CONFIG_DIR"], work)
	}
}

// TestResumeProfileEnvUnresolvedForUnknownProfile covers task 2.3: when
// the profile a session claims to belong to cannot be found at all (e.g.
// its installation was removed from this machine since the session was
// indexed), resolution must fail rather than guess.
func TestResumeProfileEnvUnresolvedForUnknownProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", filepath.Join(dir, "nope-claude"))
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(dir, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(dir, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(dir, "nope-hermes"))

	_, _, ok, reason := resumeSource("claude", "ghost-profile")
	if ok {
		t.Fatal("expected ok=false when no profile named ghost-profile can be discovered")
	}
	if reason == "" {
		t.Error("expected a non-empty reason")
	}
}

// Bare `lazyrecall` is the browser, not a usage dump (change
// rename-to-lazyrecall). Stdout in a test is a pipe, not a terminal, so
// cmdBrowse takes its documented non-terminal path and prints the plain
// listing - which is exactly what makes `lazyrecall | head` keep working.
func TestBareInvocationBrowsesInsteadOfPrintingUsage(t *testing.T) {
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// $XDG_CONFIG_HOME outranks $HOME/.config in config.Path, and CI
	// runners set it (GitHub's ubuntu images do; its macOS images and a
	// typical laptop do not). Redirecting HOME alone therefore isolates
	// this test on some machines and silently reads the real user's
	// config on others - which is how this passed locally and on macOS
	// and failed on the first Linux CI run.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(home, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", claudeRoot)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(home, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(home, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(home, "nope-hermes"))

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	runErr := run(nil)

	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	stdout, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)
	if runErr != nil {
		t.Fatalf("run(nil): %v", runErr)
	}

	combined := string(stdout) + string(stderr)
	if strings.Contains(combined, "Usage:") {
		t.Errorf("bare invocation printed usage instead of browsing:\n%s", combined)
	}
	if !strings.Contains(combined, "requires a terminal") {
		t.Errorf("bare invocation did not take the browse command's non-terminal path:\n%s", combined)
	}
}

// An unrecognised first argument is still an error naming the commands,
// never a silent fallthrough into the browser or a search.
func TestUnknownCommandStillErrors(t *testing.T) {
	old := os.Stderr
	_, w, _ := os.Pipe()
	os.Stderr = w
	err := run([]string{"bogus"})
	w.Close()
	os.Stderr = old
	if err == nil {
		t.Fatal("an unknown command returned no error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("the error %q does not name the unknown command", err)
	}
}

// TestConfigShowMarksFileAndDefaultOrigins covers the provenance display
// of `lazyrecall config show`: a value set by the config file must be shown
// as (file) and one never touched as (default) - printed with its source,
// not just printed.
func TestConfigShowMarksFileAndDefaultOrigins(t *testing.T) {
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "lazyrecall")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configContent := "[hide]\nmin_messages = 5\n\n[groups.work]\npaths = [\"~/code\"]\n\n[labels]\n\"~/.claude\" = \"cc\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// $XDG_CONFIG_HOME outranks $HOME/.config in config.Path, and CI
	// runners set it (GitHub's ubuntu images do; its macOS images and a
	// typical laptop do not). Redirecting HOME alone therefore isolates
	// this test on some machines and silently reads the real user's
	// config on others - which is how this passed locally and on macOS
	// and failed on the first Linux CI run.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_CONFIG", "")
	t.Setenv("LAZYRECALL_PROFILE", "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := run([]string{"config", "show"})
	w.Close()
	os.Stdout = old
	if runErr != nil {
		t.Fatalf("run(config show): %v", runErr)
	}
	out, _ := io.ReadAll(r)
	got := string(out)

	if !strings.Contains(got, "hide.min_messages = 5    (file)") {
		t.Errorf("expected the file-set value attributed to the file, got:\n%s", got)
	}
	if !strings.Contains(got, "browse.show_archived = false    (default)") {
		t.Errorf("expected an untouched value attributed to the default, got:\n%s", got)
	}
	// Change group-sessions-in-one-index: a configured group and a
	// configured label must show up in the effective config too, both
	// attributed to the file, and an untouched key (default_group) must
	// still show as the default even though other keys came from the file.
	if !strings.Contains(got, "groups.work.paths = ["+filepath.Join(home, "code")+"]    (file)") {
		t.Errorf("expected the configured group attributed to the file, got:\n%s", got)
	}
	wantLabel := "labels = map[" + filepath.Join(home, ".claude") + ":cc]    (file)"
	if !strings.Contains(got, wantLabel) {
		t.Errorf("expected the configured label attributed to the file, got:\n%s\nwant substring:\n%s", got, wantLabel)
	}
	if !strings.Contains(got, "browse.default_group =     (default)") {
		t.Errorf("expected an untouched default_group attributed to the default, got:\n%s", got)
	}
}

// TestArchiveCommandAcceptsShortHandle covers change add-archive-facility:
// `lazyrecall archive SESSION_ID` must accept the short handle exactly like
// every other command, resolving it through annotate.LineageForIdentifier
// and archiving the lineage it names. The lineage is seeded directly into
// the profile's database (so the test needs no real session source), and
// --no-refresh keeps the command from touching the sources.
func TestArchiveCommandAcceptsShortHandle(t *testing.T) {
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// $XDG_CONFIG_HOME outranks $HOME/.config in config.Path, and CI
	// runners set it (GitHub's ubuntu images do; its macOS images and a
	// typical laptop do not). Redirecting HOME alone therefore isolates
	// this test on some machines and silently reads the real user's
	// config on others - which is how this passed locally and on macOS
	// and failed on the first Linux CI run.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(home, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", claudeRoot)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(home, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(home, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(home, "nope-hermes"))

	installs, err := profile.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 {
		t.Fatalf("expected exactly 1 install, got %d: %+v", len(installs), installs)
	}
	install := installs[0].Name
	// refresh.New creates the data directory itself, but this test seeds the
	// database before the command ever runs - so the directory has to exist
	// first.
	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db := &sqlitex.Runner{DBPath: profile.DBPath()}
	if _, err := schema.Open(db); err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "handle"}, []map[string]any{
		{"id": "lin1", "profile": install, "orphaned": 0, "handle": 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable"}, []map[string]any{
		{"id": "claude:" + install + ":s1", "source": "claude", "source_session_id": "s1", "lineage_id": "lin1", "end_state": "completed", "resumable": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"archive", "--no-refresh", "5"}); err != nil {
		t.Fatalf("run(archive 5): %v", err)
	}

	archived, err := annotate.IsArchived(db, "lin1")
	if err != nil {
		t.Fatal(err)
	}
	if !archived {
		t.Fatal("expected handle 5's lineage to be archived after `archive 5`")
	}
}

// runCaptured runs the command with stdout captured, returning what it
// printed, so tests can assert on the human-readable output.
func runCaptured(t *testing.T, args []string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := run(args)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// TestListReportsHiddenAndAllFlagShowsEverything covers change
// apply-config-hide-rules end to end: the standing rules from the config
// are applied to `list` (the default config hides automated sessions), the
// header says how much was suppressed instead of hiding silently, and
// `--all` disables the rules and the note together.
func TestListReportsHiddenAndAllFlagShowsEverything(t *testing.T) {
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// $XDG_CONFIG_HOME outranks $HOME/.config in config.Path, and CI
	// runners set it (GitHub's ubuntu images do; its macOS images and a
	// typical laptop do not). Redirecting HOME alone therefore isolates
	// this test on some machines and silently reads the real user's
	// config on others - which is how this passed locally and on macOS
	// and failed on the first Linux CI run.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(home, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", claudeRoot)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(home, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(home, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(home, "nope-hermes"))

	installs, err := profile.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 {
		t.Fatalf("expected exactly 1 install, got %d: %+v", len(installs), installs)
	}
	install := installs[0].Name
	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db := &sqlitex.Runner{DBPath: profile.DBPath()}
	if _, err := schema.Open(db); err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "origin"}, []map[string]any{
		{"id": "claude:" + install + ":auto", "source": "claude", "source_session_id": "auto", "lineage_id": "lin-auto", "end_state": "completed", "resumable": 1, "origin": "automated"},
		{"id": "claude:" + install + ":inter", "source": "claude", "source_session_id": "inter", "lineage_id": "lin-inter", "end_state": "completed", "resumable": 1, "origin": "interactive"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	out, runErr := runCaptured(t, []string{"list", "--no-refresh"})
	if runErr != nil {
		t.Fatalf("run(list --no-refresh): %v", runErr)
	}
	if !strings.Contains(out, "1 hidden (--all to show)") {
		t.Errorf("expected the header to report the hidden automated session, got:\n%s", out)
	}

	out, runErr = runCaptured(t, []string{"list", "--no-refresh", "--all"})
	if runErr != nil {
		t.Fatalf("run(list --no-refresh --all): %v", runErr)
	}
	if strings.Contains(out, "hidden") {
		t.Errorf("--all must show everything without a hidden note, got:\n%s", out)
	}
}

// TestPrintHiddenNote covers what is left of the old "Profile: NAME" header
// once there is no longer one active profile to name (change
// group-sessions-in-one-index): just the hidden-count line, printed only
// when something was actually hidden.
func TestPrintHiddenNote(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	printHiddenNote(3)
	w.Close()
	os.Stdout = old
	buf, _ := io.ReadAll(r)
	if got := string(buf); got != "3 hidden (--all to show)\n" {
		t.Errorf("got %q, want the hidden note", got)
	}

	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printHiddenNote(0)
	w.Close()
	os.Stdout = old
	buf, _ = io.ReadAll(r)
	if got := string(buf); got != "" {
		t.Errorf("got %q, want nothing printed when nothing is hidden", got)
	}
}

// The following cover change group-sessions-in-one-index's CLI surface:
// --group on list/search/review/browse, the `group` command, and the JSON
// envelope's "group" field.

// setupGroupTestEnv isolates HOME/config/data exactly like
// TestListReportsHiddenAndAllFlagShowsEverything and seeds a config file
// with two groups, returning the single discovered install's name and an
// open runner over its (empty) index.
func setupGroupTestEnv(t *testing.T, configBody string) (install string, db *sqlitex.Runner) {
	t.Helper()
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(home, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", claudeRoot)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(home, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(home, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(home, "nope-hermes"))
	if configBody != "" {
		cfgPath := filepath.Join(home, "config.toml")
		if err := os.WriteFile(cfgPath, []byte(configBody), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LAZYRECALL_CONFIG", cfgPath)
	}

	installs, err := profile.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 {
		t.Fatalf("expected exactly 1 install, got %d: %+v", len(installs), installs)
	}
	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db = &sqlitex.Runner{DBPath: profile.DBPath()}
	if _, err := schema.Open(db); err != nil {
		t.Fatal(err)
	}
	return installs[0].Name, db
}

// TestGroupFlagFiltersByGroup covers --group=NAME on `list`: a session under
// a configured group's path is shown under --group=NAME and excluded from
// another group's view.
func TestGroupFlagFiltersByGroup(t *testing.T) {
	install, db := setupGroupTestEnv(t, "[groups.work]\npaths = [\"/code\"]\n\n[groups.personal]\npaths = [\"/home\"]\n")
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "cwd", "install"}, []map[string]any{
		{"id": "claude:" + install + ":work", "source": "claude", "source_session_id": "work", "lineage_id": "lin-work", "end_state": "completed", "resumable": 1, "cwd": "/code/proj", "install": install},
		{"id": "claude:" + install + ":personal", "source": "claude", "source_session_id": "personal", "lineage_id": "lin-personal", "end_state": "completed", "resumable": 1, "cwd": "/home/me", "install": install},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	out, runErr := runCaptured(t, []string{"list", "--no-refresh", "--group=work"})
	if runErr != nil {
		t.Fatalf("run(list --group=work): %v", runErr)
	}
	if strings.Contains(out, "/home/me") {
		t.Errorf("--group=work: expected the personal session to be excluded, got:\n%s", out)
	}

	out, runErr = runCaptured(t, []string{"list", "--no-refresh", "--group=personal"})
	if runErr != nil {
		t.Fatalf("run(list --group=personal): %v", runErr)
	}
	if strings.Contains(out, "/code/proj") {
		t.Errorf("--group=personal: expected the work session to be excluded, got:\n%s", out)
	}
}

// TestInvalidGroupFlagErrors covers that an unrecognised --group value is a
// reported error listing the valid ones, not a silent empty result.
func TestInvalidGroupFlagErrors(t *testing.T) {
	setupGroupTestEnv(t, "[groups.work]\npaths = [\"/code\"]\n")

	_, runErr := runCaptured(t, []string{"list", "--no-refresh", "--group=bogus"})
	if runErr == nil {
		t.Fatal("expected an error for an unrecognised --group value")
	}
	if !strings.Contains(runErr.Error(), "bogus") || !strings.Contains(runErr.Error(), "work") {
		t.Errorf("error %q should name the bad value and list the valid groups", runErr.Error())
	}
}

// TestEmptyMessageNamesTheSelectedGroup is the end-to-end regression test
// for a stale message (change group-sessions-in-one-index, review fix #5):
// EmptyMessage used to always say `No session in profile "all"...`,
// regardless of what was actually selected - a leftover from before groups
// replaced the single active profile. With a group selected the printed
// message now names it; with none selected it says nothing about scope at
// all, not even "all".
func TestEmptyMessageNamesTheSelectedGroup(t *testing.T) {
	setupGroupTestEnv(t, "[groups.work]\npaths = [\"/code\"]\n")

	out, runErr := runCaptured(t, []string{"list", "--no-refresh", "--group=work"})
	if runErr != nil {
		t.Fatalf("run(list --group=work): %v", runErr)
	}
	if !strings.Contains(out, `group "work"`) {
		t.Errorf("expected the empty message to name the selected group, got:\n%s", out)
	}

	out, runErr = runCaptured(t, []string{"list", "--no-refresh"})
	if runErr != nil {
		t.Fatalf("run(list): %v", runErr)
	}
	if strings.Contains(out, "group") || strings.Contains(out, `"all"`) {
		t.Errorf("expected the empty message to say nothing about scope for All, got:\n%s", out)
	}
}

// TestInvalidDefaultGroupConfigErrorsEndToEnd is the P1 regression test for
// a review finding: cmdBrowse used to apply cfg.Browse.DefaultGroup with no
// validation at all (unlike --group, which always goes through
// search.ValidateGroup), so a config with default_group = "typo" exited 0
// and opened on an empty view with no explanation. Since the fix moved
// validation into config.Load, every command that loads config now reports
// it - exercised here through `list`, which needs no terminal.
//
// This does not reuse setupGroupTestEnv: that helper calls profile.Discover
// (which itself calls config.Load) as part of its own setup and t.Fatals on
// any error - exactly what an invalid default_group now correctly produces,
// so using it here would fail the test before runCaptured ever ran. list
// never reaches install discovery for this case anyway: cmdList calls
// config.Load directly, before openDB, so no install root needs to exist.
func TestInvalidDefaultGroupConfigErrorsEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(home, "data"))
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[groups.work]\npaths = [\"/code\"]\n\n[browse]\ndefault_group = \"typo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAZYRECALL_CONFIG", cfgPath)

	_, runErr := runCaptured(t, []string{"list", "--no-refresh"})
	if runErr == nil {
		t.Fatal("expected a non-zero exit for an invalid browse.default_group")
	}
	if !strings.Contains(runErr.Error(), "typo") {
		t.Errorf("error %q should name the bad value", runErr.Error())
	}
}

// TestValidDefaultGroupConfigValuesPass covers every value
// browse.default_group may legitimately take, exercised end to end: none of
// them should error, whether or not a configured group even applies to the
// (empty) index used here.
func TestValidDefaultGroupConfigValuesPass(t *testing.T) {
	for _, v := range []string{"", "all", "archive", "unknown", "work"} {
		setupGroupTestEnv(t, fmt.Sprintf("[groups.work]\npaths = [\"/code\"]\n\n[browse]\ndefault_group = %q\n", v))

		_, runErr := runCaptured(t, []string{"list", "--no-refresh"})
		if runErr != nil {
			t.Errorf("default_group = %q: unexpected error: %v", v, runErr)
		}
	}
}

// TestGroupCommandRoundTrip covers `lazyrecall group SESSION_ID NAME`,
// `... archive`, and `... --auto`: each must leave the lineage's
// group_name/archived_at exactly as annotate.SetGroup's own unit tests
// establish, reachable through the CLI's handle-based identifier.
func TestGroupCommandRoundTrip(t *testing.T) {
	install, db := setupGroupTestEnv(t, "[groups.work]\npaths = [\"/code\"]\n")
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "handle"}, []map[string]any{
		{"id": "lin1", "profile": install, "orphaned": 0, "handle": 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable"}, []map[string]any{
		{"id": "claude:" + install + ":s1", "source": "claude", "source_session_id": "s1", "lineage_id": "lin1", "end_state": "completed", "resumable": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	readLineage := func() (groupName *string, archivedAt *int64) {
		t.Helper()
		var rows []struct {
			GroupName  *string `json:"group_name"`
			ArchivedAt *int64  `json:"archived_at"`
		}
		if err := db.Query("SELECT group_name, archived_at FROM lineages WHERE id = 'lin1';", &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected exactly one lineage row, got %d", len(rows))
		}
		return rows[0].GroupName, rows[0].ArchivedAt
	}

	out, runErr := runCaptured(t, []string{"group", "--no-refresh", "5", "work"})
	if runErr != nil {
		t.Fatalf("run(group 5 work): %v", runErr)
	}
	if !strings.Contains(out, "5") || !strings.Contains(out, "work") {
		t.Errorf("expected the command to print what was set, got %q", out)
	}
	name, archivedAt := readLineage()
	if name == nil || *name != "work" {
		t.Errorf("after 'group 5 work': group_name = %v, want work", name)
	}
	if archivedAt != nil {
		t.Errorf("after 'group 5 work': archived_at = %v, want nil", archivedAt)
	}

	if _, runErr := runCaptured(t, []string{"group", "--no-refresh", "5", "archive"}); runErr != nil {
		t.Fatalf("run(group 5 archive): %v", runErr)
	}
	name, archivedAt = readLineage()
	if name == nil || *name != "work" {
		t.Errorf("after 'group 5 archive': group_name = %v, want work (archiving must not clear it)", name)
	}
	if archivedAt == nil {
		t.Error("after 'group 5 archive': archived_at = nil, want set")
	}

	if _, runErr := runCaptured(t, []string{"group", "--no-refresh", "5", "--auto"}); runErr != nil {
		t.Fatalf("run(group 5 --auto): %v", runErr)
	}
	name, archivedAt = readLineage()
	if name != nil {
		t.Errorf("after 'group 5 --auto': group_name = %v, want nil", name)
	}
	if archivedAt != nil {
		t.Errorf("after 'group 5 --auto': archived_at = %v, want nil", archivedAt)
	}
}

// TestNameCommandRoundTrip covers `lazyrecall name SESSION_ID TEXT...` and
// `lazyrecall name SESSION_ID --clear` end to end: resolving the short
// handle, writing lineages.custom_name through annotate.SetName exactly as
// annotate's own unit tests establish, and clearing it again. This is
// lazyrecall's own name, distinct from sessions.name (a source tool's own
// rename, which `name` never touches).
func TestNameCommandRoundTrip(t *testing.T) {
	install, db := setupGroupTestEnv(t, "")
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned", "handle"}, []map[string]any{
		{"id": "lin1", "profile": install, "orphaned": 0, "handle": 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable"}, []map[string]any{
		{"id": "claude:" + install + ":s1", "source": "claude", "source_session_id": "s1", "lineage_id": "lin1", "end_state": "completed", "resumable": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	readCustomName := func() *string {
		t.Helper()
		var rows []struct {
			CustomName *string `json:"custom_name"`
		}
		if err := db.Query("SELECT custom_name FROM lineages WHERE id = 'lin1';", &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected exactly one lineage row, got %d", len(rows))
		}
		return rows[0].CustomName
	}

	out, runErr := runCaptured(t, []string{"name", "--no-refresh", "5", "the", "retry", "loop"})
	if runErr != nil {
		t.Fatalf("run(name 5 the retry loop): %v", runErr)
	}
	if !strings.Contains(out, "5") || !strings.Contains(out, "the retry loop") {
		t.Errorf("expected the command to print what was set, got %q", out)
	}
	if got := readCustomName(); got == nil || *got != "the retry loop" {
		t.Errorf("after 'name 5 the retry loop': custom_name = %v, want \"the retry loop\"", got)
	}

	if _, runErr := runCaptured(t, []string{"name", "--no-refresh", "5", "--clear"}); runErr != nil {
		t.Fatalf("run(name 5 --clear): %v", runErr)
	}
	if got := readCustomName(); got != nil {
		t.Errorf("after 'name 5 --clear': custom_name = %v, want nil", got)
	}
}

// TestNameCommandAcceptsFullyQualifiedIdentifier covers resolving SESSION_ID
// via search.ItemForIdentifier's other form - the full composite id, not
// just the short handle - mirroring how comment/tag/group accept either.
func TestNameCommandAcceptsFullyQualifiedIdentifier(t *testing.T) {
	install, db := setupGroupTestEnv(t, "")
	b := db.NewBatch()
	if err := b.BulkInsert("lineages", []string{"id", "profile", "orphaned"}, []map[string]any{
		{"id": "lin1", "profile": install, "orphaned": 0},
	}); err != nil {
		t.Fatal(err)
	}
	sessionID := "claude:" + install + ":s1"
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable"}, []map[string]any{
		{"id": sessionID, "source": "claude", "source_session_id": "s1", "lineage_id": "lin1", "end_state": "completed", "resumable": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	if _, runErr := runCaptured(t, []string{"name", "--no-refresh", sessionID, "retry-loop"}); runErr != nil {
		t.Fatalf("run(name %s retry-loop): %v", sessionID, runErr)
	}
	var rows []struct {
		CustomName *string `json:"custom_name"`
	}
	if err := db.Query("SELECT custom_name FROM lineages WHERE id = 'lin1';", &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CustomName == nil || *rows[0].CustomName != "retry-loop" {
		t.Errorf("custom_name = %+v, want retry-loop", rows)
	}
}

// TestNameCommandUnresolvedIdentifierErrors covers an identifier that does
// not resolve to any session: the command must report that rather than
// silently doing nothing (the same contract every other handle-accepting
// command has).
func TestNameCommandUnresolvedIdentifierErrors(t *testing.T) {
	setupGroupTestEnv(t, "")

	if _, runErr := runCaptured(t, []string{"name", "--no-refresh", "999", "a name"}); runErr == nil {
		t.Fatal("expected an error for a handle that does not resolve to any session")
	}
}

// TestListJSONEnvelopeShape covers the restored JSON envelope
// ({"group": ..., "items": [...]}, change group-sessions-in-one-index) for
// `list --json`: the top-level shape, not just the item array Stage B left
// behind.
func TestListJSONEnvelopeShape(t *testing.T) {
	install, db := setupGroupTestEnv(t, "[groups.work]\npaths = [\"/code\"]\n")
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "cwd", "install"}, []map[string]any{
		{"id": "claude:" + install + ":1", "source": "claude", "source_session_id": "1", "lineage_id": "lin1", "end_state": "completed", "resumable": 1, "cwd": "/code/proj", "install": install},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	out, runErr := runCaptured(t, []string{"list", "--no-refresh", "--json", "--group=work"})
	if runErr != nil {
		t.Fatalf("run(list --json --group=work): %v", runErr)
	}
	var envelope struct {
		Group string           `json:"group"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("output is not the expected JSON envelope: %v\noutput:\n%s", err, out)
	}
	if envelope.Group != "work" {
		t.Errorf("envelope.Group = %q, want work", envelope.Group)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("envelope.Items = %+v, want exactly one item", envelope.Items)
	}

	// The bare "all" case restores the same envelope shape with "all" as
	// the group.
	outAll, runErr := runCaptured(t, []string{"list", "--no-refresh", "--json"})
	if runErr != nil {
		t.Fatalf("run(list --json): %v", runErr)
	}
	var envelopeAll struct {
		Group string `json:"group"`
	}
	if err := json.Unmarshal([]byte(outAll), &envelopeAll); err != nil {
		t.Fatalf("output is not the expected JSON envelope: %v\noutput:\n%s", err, outAll)
	}
	if envelopeAll.Group != "all" {
		t.Errorf("envelope.Group = %q, want all", envelopeAll.Group)
	}
}

// TestAgentFlagResolvesLabelsAndInstallNames covers change
// group-sessions-in-one-index's follow-up fix: two discovered claude
// installs, one of them literally named "claude" (the same as its own
// source), labeled cc/ccp. --agent must resolve a label to that one
// install, a source name to every install of it, and an install name (with
// no label) to that one install - resolveAgentFilter's whole priority
// order, exercised end to end through `list --json`.
func TestAgentFlagResolvesLabelsAndInstallNames(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, ".claude")
	personal := filepath.Join(dir, ".claude-personal")
	for _, root := range []string{work, personal} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "history.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_HOME", filepath.Join(dir, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", work+":"+personal)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(dir, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(dir, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(dir, "nope-hermes"))
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[labels]\n\"~/.claude\" = \"cc\"\n\"~/.claude-personal\" = \"ccp\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAZYRECALL_CONFIG", cfgPath)

	installs, err := profile.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 2 {
		t.Fatalf("expected exactly 2 installs, got %d: %+v", len(installs), installs)
	}
	// The work install's name equals its own source's name - the exact
	// condition the fixed bug depended on.
	if installs[0].Name != "claude" && installs[1].Name != "claude" {
		t.Fatalf("expected one install named exactly %q, got %+v", "claude", installs)
	}

	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db := &sqlitex.Runner{DBPath: profile.DBPath()}
	if _, err := schema.Open(db); err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "install"}, []map[string]any{
		{"id": "claude:claude:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin-work", "end_state": "completed", "resumable": 1, "install": "claude"},
		{"id": "claude:claude-personal:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin-personal", "end_state": "completed", "resumable": 1, "install": "claude-personal"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	sessionIDs := func(t *testing.T, agent string) []string {
		t.Helper()
		out, runErr := runCaptured(t, []string{"list", "--no-refresh", "--json", "--agent=" + agent})
		if runErr != nil {
			t.Fatalf("run(list --agent=%s): %v", agent, runErr)
		}
		var envelope struct {
			Items []struct {
				SessionID string `json:"session_id"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("--agent=%s: output is not the expected JSON envelope: %v\noutput:\n%s", agent, err, out)
		}
		ids := make([]string, len(envelope.Items))
		for i, it := range envelope.Items {
			ids[i] = it.SessionID
		}
		return ids
	}

	if got := sessionIDs(t, "cc"); !reflect.DeepEqual(got, []string{"claude:claude:1"}) {
		t.Errorf("--agent=cc: got %v, want only the work install (label -> Filter.Install)", got)
	}
	if got := sessionIDs(t, "ccp"); !reflect.DeepEqual(got, []string{"claude:claude-personal:1"}) {
		t.Errorf("--agent=ccp: got %v, want only the personal install (label -> Filter.Install)", got)
	}
	got := sessionIDs(t, "claude")
	sort.Strings(got)
	want := []string{"claude:claude-personal:1", "claude:claude:1"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--agent=claude: got %v, want both installs (source name -> Filter.Agent)", got)
	}
	if got := sessionIDs(t, "claude-personal"); !reflect.DeepEqual(got, []string{"claude:claude-personal:1"}) {
		t.Errorf("--agent=claude-personal: got %v, want only that install (install name, no label needed -> Filter.Install)", got)
	}
}

// TestAgentFlagResolvesInstallsWithNoLabelsConfigured is the P1 regression
// test for the bug TestAgentFlagResolvesLabelsAndInstallNames never caught:
// that test always configures [labels], so resolveAgentFilter's step 1
// (Profile.ConfiguredLabel) simply never matched by accident the way the old
// Profile.Label-based check did. With NO [labels] table at all, the work
// install is still named exactly "claude" - the same as its own source - so
// the old code's step 1 (comparing against Label's install-name fallback)
// mistook that install for a label of itself and --agent=claude resolved to
// Filter.Install="claude" (this one install only) instead of
// Filter.Agent="claude" (every claude install), silently narrower than the
// README promises. This test is otherwise identical to
// TestAgentFlagResolvesLabelsAndInstallNames minus the config file.
func TestAgentFlagResolvesInstallsWithNoLabelsConfigured(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, ".claude")
	personal := filepath.Join(dir, ".claude-personal")
	for _, root := range []string{work, personal} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "history.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LAZYRECALL_CONFIG", filepath.Join(dir, "no-such-config.toml"))
	t.Setenv("LAZYRECALL_HOME", filepath.Join(dir, "data"))
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", work+":"+personal)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(dir, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(dir, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(dir, "nope-hermes"))

	installs, err := profile.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 2 {
		t.Fatalf("expected exactly 2 installs, got %d: %+v", len(installs), installs)
	}
	if installs[0].Name != "claude" && installs[1].Name != "claude" {
		t.Fatalf("expected one install named exactly %q, got %+v", "claude", installs)
	}

	if err := os.MkdirAll(profile.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db := &sqlitex.Runner{DBPath: profile.DBPath()}
	if _, err := schema.Open(db); err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "install"}, []map[string]any{
		{"id": "claude:claude:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin-work", "end_state": "completed", "resumable": 1, "install": "claude"},
		{"id": "claude:claude-personal:1", "source": "claude", "source_session_id": "1", "lineage_id": "lin-personal", "end_state": "completed", "resumable": 1, "install": "claude-personal"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	sessionIDs := func(t *testing.T, agent string) []string {
		t.Helper()
		out, runErr := runCaptured(t, []string{"list", "--no-refresh", "--json", "--agent=" + agent})
		if runErr != nil {
			t.Fatalf("run(list --agent=%s): %v", agent, runErr)
		}
		var envelope struct {
			Items []struct {
				SessionID string `json:"session_id"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Fatalf("--agent=%s: output is not the expected JSON envelope: %v\noutput:\n%s", agent, err, out)
		}
		ids := make([]string, len(envelope.Items))
		for i, it := range envelope.Items {
			ids[i] = it.SessionID
		}
		return ids
	}

	got := sessionIDs(t, "claude")
	sort.Strings(got)
	want := []string{"claude:claude-personal:1", "claude:claude:1"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--agent=claude with no labels configured: got %v, want both installs (source name -> Filter.Agent)", got)
	}
	if got := sessionIDs(t, "claude-personal"); !reflect.DeepEqual(got, []string{"claude:claude-personal:1"}) {
		t.Errorf("--agent=claude-personal with no labels configured: got %v, want only that install", got)
	}
}

// The following cover task "resume shortcuts and unique id prefixes":
// `resume --last`, `resume .`, `resume --repo=PATH`, an ambiguous prefix,
// and an identifier that resolves to nothing. Every seeded session here has
// dir_exists=0 (and no git_repo_root), so resume.Resume's own
// "DirectoryMissing" outcome fires and returns before it would ever resolve
// a program, chdir, or exec anything (internal/resume.Resume checks
// DirExists before ProfileUnresolved, before agentCommand, before
// exec.LookPath) - real end-to-end coverage of which session cmdResume
// selected, with no stub needed and no risk of actually invoking an agent.
// The outcome message echoes back the session's own CWD, which is how each
// test tells which session was chosen.

// seedResumeCandidate inserts one never-actually-resumed session: resumable,
// but with a missing working directory, so Resume reports DirectoryMissing
// (naming cwd in its message) instead of ever touching exec.LookPath or
// execAgent. source is deliberately "pi" (no configured EnvVar), so
// resumeSource's profile-environment lookup succeeds trivially with no
// installation discovery needed - irrelevant here anyway, since
// DirectoryMissing is decided first.
func seedResumeCandidate(t *testing.T, db *sqlitex.Runner, id, lineageID, cwd, gitCommonRoot string, lastActivityAt int64) {
	t.Helper()
	rec := map[string]any{
		"id": id, "source": "pi", "source_session_id": id, "lineage_id": lineageID,
		"end_state": "completed", "resumable": 1, "origin": "interactive",
		"cwd": cwd, "dir_exists": 0, "last_activity_at": lastActivityAt,
	}
	if gitCommonRoot != "" {
		rec["git_common_root"] = gitCommonRoot
	}
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", keysOf(rec), []map[string]any{rec}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestResumeLastFlagPicksMostRecentDefaultListing covers `resume --last`:
// the most recent session of the same default listing `lazyrecall list`
// applies with no flags (non-archived, hide rules applied), with no picker.
func TestResumeLastFlagPicksMostRecentDefaultListing(t *testing.T) {
	_, db := setupGroupTestEnv(t, "")
	seedResumeCandidate(t, db, "pi:p:older", "lin-older", "/only-older", "", 100)
	seedResumeCandidate(t, db, "pi:p:newer", "lin-newer", "/only-newer", "", 900)

	out, runErr := runCaptured(t, []string{"resume", "--no-refresh", "--last"})
	if runErr != nil {
		t.Fatalf("run(resume --last): %v", runErr)
	}
	if !strings.Contains(out, "/only-newer") {
		t.Errorf("expected the most recently active session to be chosen, got:\n%s", out)
	}
	if strings.Contains(out, "/only-older") {
		t.Errorf("expected the older session not to be chosen, got:\n%s", out)
	}
}

// TestResumeLastFlagNoSessionsExitsNonZero covers that --last, unlike the
// ordinary empty picker, reports failure (non-zero exit) when there is
// nothing to resume - --last promises to resume something directly, and an
// empty index cannot honour that promise silently.
func TestResumeLastFlagNoSessionsExitsNonZero(t *testing.T) {
	setupGroupTestEnv(t, "")

	_, runErr := runCaptured(t, []string{"resume", "--no-refresh", "--last"})
	if runErr == nil {
		t.Fatal("expected a non-zero exit when --last has no session to resume")
	}
}

// TestResumeDotResumesMostRecentSessionInCurrentRepo covers `resume .`:
// resolved to the current directory's own absolute path first, then matched
// exactly the way `list --repo=PATH` matches (literal comparison against a
// session's own recorded git_common_root, falling back to cwd) - narrowing
// out a session recorded elsewhere even though it is more recently active,
// and picking the newer of two sessions within the matching repo.
func TestResumeDotResumesMostRecentSessionInCurrentRepo(t *testing.T) {
	_, db := setupGroupTestEnv(t, "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	seedResumeCandidate(t, db, "pi:p:here", "lin-here", cwd, "", 100)
	seedResumeCandidate(t, db, "pi:p:elsewhere", "lin-elsewhere", "/elsewhere/repo", "", 900)

	out, runErr := runCaptured(t, []string{"resume", "--no-refresh", "."})
	if runErr != nil {
		t.Fatalf("run(resume .): %v", runErr)
	}
	if !strings.Contains(out, cwd) {
		t.Errorf("expected the session recorded in the current directory to be chosen, got:\n%s", out)
	}
	if strings.Contains(out, "/elsewhere/repo") {
		t.Errorf("expected the more-recent-but-elsewhere session not to be chosen, got:\n%s", out)
	}
}

// TestResumeDotNoMatchExitsNonZero covers "If nothing matches, print a
// clear message and exit non-zero" for `resume .`.
func TestResumeDotNoMatchExitsNonZero(t *testing.T) {
	_, db := setupGroupTestEnv(t, "")
	seedResumeCandidate(t, db, "pi:p:elsewhere", "lin-elsewhere", "/elsewhere/repo", "", 100)

	out, runErr := runCaptured(t, []string{"resume", "--no-refresh", "."})
	if runErr == nil {
		t.Fatalf("expected a non-zero exit when \".\" matches no session, got output:\n%s", out)
	}
}

// TestResumeRepoFlagOpensPickerNarrowedToRepo covers `resume --repo=PATH`
// with no --last: a picker (the numbered list, cli.Pick), narrowed to that
// repo. Stdin is redirected to immediate EOF, so Pick reports no choice was
// made (ok=false) without hanging or resuming anything - what is asserted
// here is only which sessions were offered.
func TestResumeRepoFlagOpensPickerNarrowedToRepo(t *testing.T) {
	_, db := setupGroupTestEnv(t, "")
	seedResumeCandidate(t, db, "pi:p:in-repo", "lin-in-repo", "/repo/proj", "/repo/proj", 100)
	seedResumeCandidate(t, db, "pi:p:other", "lin-other", "/elsewhere", "", 200)

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close() // immediate EOF: Pick reads nothing and reports ok=false
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	out, runErr := runCaptured(t, []string{"resume", "--no-refresh", "--repo=/repo/proj"})
	if runErr != nil {
		t.Fatalf("run(resume --repo=/repo/proj): %v", runErr)
	}
	if !strings.Contains(out, "/repo/proj") {
		t.Errorf("expected the in-repo session to be offered in the picker, got:\n%s", out)
	}
	if strings.Contains(out, "/elsewhere") {
		t.Errorf("expected the picker to be narrowed to the repo, got:\n%s", out)
	}
}

// TestResumeRepoWithLastResumesNewestInThatRepo covers `resume
// --repo=PATH --last`: the newest session in that repo, resumed directly
// with no picker - not merely the newest session overall.
func TestResumeRepoWithLastResumesNewestInThatRepo(t *testing.T) {
	_, db := setupGroupTestEnv(t, "")
	const repo = "/repo/proj"
	seedResumeCandidate(t, db, "pi:p:repo-old", "lin-repo-old", repo+"/old", repo, 100)
	seedResumeCandidate(t, db, "pi:p:repo-new", "lin-repo-new", repo+"/new", repo, 500)
	seedResumeCandidate(t, db, "pi:p:elsewhere", "lin-elsewhere", "/elsewhere", "", 900)

	out, runErr := runCaptured(t, []string{"resume", "--no-refresh", "--repo=" + repo, "--last"})
	if runErr != nil {
		t.Fatalf("run(resume --repo=%s --last): %v", repo, runErr)
	}
	if !strings.Contains(out, repo+"/new") {
		t.Errorf("expected the newest session within the repo to be chosen, got:\n%s", out)
	}
	if strings.Contains(out, repo+"/old") || strings.Contains(out, "/elsewhere") {
		t.Errorf("expected only the newest in-repo session to be chosen, got:\n%s", out)
	}
}

// TestResumeIdentifierDoesNotResolve covers the "no match" outcome for a
// plain (non-handle, non-prefix-ambiguous) identifier: reported as an
// error, not a silent no-op.
func TestResumeIdentifierDoesNotResolve(t *testing.T) {
	setupGroupTestEnv(t, "")

	_, runErr := runCaptured(t, []string{"resume", "--no-refresh", "nosuchsession"})
	if runErr == nil {
		t.Fatal("expected an error for an identifier that resolves to nothing")
	}
	if !strings.Contains(runErr.Error(), "nosuchsession") {
		t.Errorf("error %q should name the identifier that did not resolve", runErr.Error())
	}
}

// TestResumeAmbiguousPrefixPrintsCandidatesAndExitsNonZero covers a unique
// id prefix that matches more than one session: a short list of candidate
// rows printed to stderr (cli.RenderAmbiguous), and a non-zero exit -
// covering `resume`'s use of the same resolveItem/search.ItemForIdentifier
// path every other identifier-taking verb uses.
func TestResumeAmbiguousPrefixPrintsCandidatesAndExitsNonZero(t *testing.T) {
	_, db := setupGroupTestEnv(t, "")
	// Distinct cwds so the two candidates' printed rows are distinguishable
	// - RenderRow shows neither the composite id nor the native
	// SourceSessionID, only fields like location/topic/tags.
	b := db.NewBatch()
	if err := b.BulkInsert("sessions", []string{"id", "source", "source_session_id", "lineage_id", "end_state", "resumable", "origin", "last_activity_at", "cwd"}, []map[string]any{
		{"id": "pi:p:abcd1111", "source": "pi", "source_session_id": "abcd1111", "lineage_id": "lin1", "end_state": "completed", "resumable": 1, "origin": "interactive", "last_activity_at": 100, "cwd": "/abcd/one"},
		{"id": "pi:p:abcd2222", "source": "pi", "source_session_id": "abcd2222", "lineage_id": "lin2", "end_state": "completed", "resumable": 1, "origin": "interactive", "last_activity_at": 200, "cwd": "/abcd/two"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(); err != nil {
		t.Fatal(err)
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	runErr := run([]string{"resume", "--no-refresh", "abcd"})

	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)

	if runErr == nil {
		t.Fatal("expected a non-zero exit for an ambiguous prefix")
	}
	if !strings.Contains(runErr.Error(), "abcd") {
		t.Errorf("error %q should name the ambiguous identifier", runErr.Error())
	}
	got := string(stderr)
	if !strings.Contains(got, "/abcd/one") || !strings.Contains(got, "/abcd/two") {
		t.Errorf("expected both candidate sessions listed on stderr, got:\n%s", got)
	}
}
