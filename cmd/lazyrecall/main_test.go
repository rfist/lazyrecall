package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newTestFlagSet builds the same FlagSet configuration every real
// subcommand parses with: the global flags (profile/json/no-refresh) plus
// the filter flags (agent/repo/tag/since). Keeping it identical to main.go
// means these tests exercise the real command parsing path, not a
// lookalike.
func newTestFlagSet() (*flag.FlagSet, *string, *string, *string, *string) {
	fs := flag.NewFlagSet("lazyrecall", flag.ContinueOnError)
	profileFlag := fs.String("profile", "", "profile to operate under")
	fs.Bool("json", false, "machine-readable JSON output")
	fs.Bool("no-refresh", false, "skip the automatic refresh")
	agent := fs.String("agent", "", "filter by agent")
	repo := fs.String("repo", "", "filter by repository root or working directory")
	tag := fs.String("tag", "", "filter by tag")
	fs.Int("since", 0, "only sessions active in the last N days")
	return fs, profileFlag, agent, repo, tag
}

func parse(t *testing.T, args []string) ([]string, *string, *string, *string, *string) {
	t.Helper()
	fs, profile, agent, repo, tag := newTestFlagSet()
	positionals, err := parseInterleaved(fs, args)
	if err != nil {
		t.Fatalf("parseInterleaved(%q) error: %v", args, err)
	}
	return positionals, profile, agent, repo, tag
}

// The following cover change fix-cli-usability-defects, tasks 1.1-1.4:
// options are accepted before, after, and between positional arguments and
// interpreted identically in every order; the end-of-options marker keeps
// dash-leading text as text.

func TestOptionAfterPositionalIsNotAbsorbed(t *testing.T) {
	positionals, profile, _, _, _ := parse(t, []string{"buddy", "--profile=work"})
	if got := *profile; got != "work" {
		t.Errorf("profile = %q, want the option to take effect", got)
	}
	if !reflect.DeepEqual(positionals, []string{"buddy"}) {
		t.Errorf("positionals = %v, want the phrase alone without the option text", positionals)
	}
}

func TestOptionBeforePositionalBehavesIdentically(t *testing.T) {
	before, beforeProfile, _, _, _ := parse(t, []string{"--profile=work", "buddy"})
	after, afterProfile, _, _, _ := parse(t, []string{"buddy", "--profile=work"})
	if *beforeProfile != *afterProfile || *beforeProfile != "work" {
		t.Errorf("profile before=%q after=%q, want identical and set", *beforeProfile, *afterProfile)
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
	positionals, profile, _, _, _ := parse(t, []string{"--", "--profile=work"})
	if got := *profile; got != "" {
		t.Errorf("profile = %q, want unset - text after -- must not be parsed as an option", got)
	}
	if !reflect.DeepEqual(positionals, []string{"--profile=work"}) {
		t.Errorf("positionals = %v, want the dash-leading text preserved", positionals)
	}
}

func TestEndOfOptionsMarkerMidArgs(t *testing.T) {
	positionals, profile, agent, _, _ := parse(t, []string{"--agent=pi", "--", "--profile=work", "buddy"})
	if got := *agent; got != "pi" {
		t.Errorf("agent = %q, want the option before -- to take effect", got)
	}
	if got := *profile; got != "" {
		t.Errorf("profile = %q, want text after -- to stay text", got)
	}
	if !reflect.DeepEqual(positionals, []string{"--profile=work", "buddy"}) {
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

// TestProfilesCommandIsReadable covers choose-from-known-values tasks
// 4.1/4.2: `lazyrecall profiles` must print each profile by name together with
// the sources it covers, never an internal data structure representation.
// Before this change it printed p.Sources() through %v - Go's own map
// syntax, e.g. "claude: map[claude:/Users/...]".
func TestProfilesCommandIsReadable(t *testing.T) {
	dir := t.TempDir()
	claudeRoot := filepath.Join(dir, "fakeclaude")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// looksLikeClaudeRoot requires history.jsonl or a projects/ dir - an
	// empty history.jsonl is enough to be discovered as a real root, all
	// synthetic, never touching this machine's real ~/.claude*.
	if err := os.WriteFile(filepath.Join(claudeRoot, "history.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAZYRECALL_CLAUDE_CONFIG_DIRS", claudeRoot)
	t.Setenv("LAZYRECALL_PI_HOME", filepath.Join(dir, "nope-pi"))
	t.Setenv("LAZYRECALL_OMP_HOME", filepath.Join(dir, "nope-omp"))
	t.Setenv("LAZYRECALL_HERMES_HOME", filepath.Join(dir, "nope-hermes"))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmdErr := cmdProfiles(false)
	w.Close()
	os.Stdout = old
	if cmdErr != nil {
		t.Fatalf("cmdProfiles: %v", cmdErr)
	}
	out, _ := io.ReadAll(r)
	got := string(out)

	if strings.Contains(got, "map[") {
		t.Errorf("expected no Go map representation in the output, got %q", got)
	}
	if !strings.Contains(got, "fakeclaude") {
		t.Errorf("expected the profile name to appear, got %q", got)
	}
	if !strings.Contains(got, "claude: "+claudeRoot) {
		t.Errorf("expected the source it covers to be shown readably, got %q", got)
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
// resumed agent must be started under its own session's profile
// configuration.

func TestProfileNameFromCompositeID(t *testing.T) {
	got := profileNameFrom("claude:claude-personal:abc-123")
	if got != "claude-personal" {
		t.Errorf("got %q, want claude-personal", got)
	}
}

func TestProfileNameFromMalformedIDReturnsEmpty(t *testing.T) {
	if got := profileNameFrom("not-a-composite-id"); got != "" {
		t.Errorf("got %q, want empty for a malformed id", got)
	}
}

// TestResumeProfileEnvNonClaudeSourceNeverVaries covers task 2.2: pi, omp,
// and hermes are each bundled into exactly one profile on this machine
// (internal/profile) - their profile configuration never varies, so no
// environment override is ever needed and resolution always succeeds.
func TestResumeProfileEnvNonClaudeSourceNeverVaries(t *testing.T) {
	for _, source := range []string{"pi", "omp", "hermes"} {
		env, ok, reason := resumeProfileEnv(source, "whatever-profile-name")
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

	env, ok, reason := resumeProfileEnv("claude", "claude-personal")
	if !ok {
		t.Fatalf("expected ok=true, got reason %q", reason)
	}
	if env["CLAUDE_CONFIG_DIR"] != personal {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", env["CLAUDE_CONFIG_DIR"], personal)
	}

	// The other (default/work) installation must resolve to its own root,
	// not the personal one - proving this isn't just always returning
	// whichever root happens to be discovered first.
	env2, ok2, reason2 := resumeProfileEnv("claude", "claude")
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

	_, ok, reason := resumeProfileEnv("claude", "ghost-profile")
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
