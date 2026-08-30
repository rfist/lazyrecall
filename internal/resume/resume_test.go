package resume

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rfist/lazyrecall/internal/config"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func TestResumeNotResumableWithoutWorkingDirectory(t *testing.T) {
	out := Resume(Target{Source: "hermes", SourceSessionID: "x", Resumable: false})
	if !out.NotResumable {
		t.Errorf("expected NotResumable, got %+v", out)
	}
	if out.Message == "" {
		t.Error("expected an explanatory message")
	}
}

func TestResumeMissingDirectoryOffersRepoRoot(t *testing.T) {
	out := Resume(Target{
		Source: "claude", SourceSessionID: "id", Resumable: true,
		CWD: strp("/work/repo/deleted-subdir"), DirExists: boolp(false),
		GitRepoRoot: strp("/work/repo"), RepoRootExists: boolp(true),
	})
	if !out.DirectoryMissing {
		t.Fatalf("expected DirectoryMissing, got %+v", out)
	}
	if out.AlternativeOffered == nil || *out.AlternativeOffered != "/work/repo" {
		t.Errorf("expected repo root offered, got %+v", out)
	}
}

func TestResumeMissingDirectoryNoAlternative(t *testing.T) {
	out := Resume(Target{
		Source: "claude", SourceSessionID: "id", Resumable: true,
		CWD: strp("/work/repo/gone"), DirExists: boolp(false),
	})
	if !out.DirectoryMissing || out.AlternativeOffered != nil {
		t.Fatalf("got %+v", out)
	}
}

// TestResumeReportsMissingAgentProgram covers spec session-resume, "The
// agent's own program is not installed": PATH is pointed at an empty
// directory, so exec.LookPath cannot find "claude" no matter what is
// actually installed on the machine running this test, and Resume must
// report which program is missing rather than trying to run it anyway.
func TestResumeReportsMissingAgentProgram(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()

	out := Resume(Target{Source: "claude", SourceSessionID: "abc", Resumable: true, CWD: strp(dir), Resume: []string{"claude", "--resume", "{id}"}})
	if !out.Failed {
		t.Fatalf("expected Failed, got %+v", out)
	}
	if !strings.Contains(out.Message, "claude") || !strings.Contains(out.Message, "not installed") {
		t.Errorf("expected a message naming the missing program, got %q", out.Message)
	}
}

// fakeAgentOnPath puts an executable file (never actually run - execAgent is
// stubbed in these tests) on PATH under name, so exec.LookPath succeeds.
func fakeAgentOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// TestResumeChangesDirectoryBeforeInvokingAgent covers design.md decision 1:
// Resume must chdir into the session's recorded working directory before
// handing off to the agent, and must pass exactly the resume arguments
// already used for this source. execAgent is stubbed so this test never
// actually replaces the test process - the real replacement is covered by
// TestResumeReplacesProcessWithAgent (resume_unix_test.go), which exercises
// the genuine syscall.Exec path end to end.
func TestResumeChangesDirectoryBeforeInvokingAgent(t *testing.T) {
	fakeAgentOnPath(t, "claude")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origWD) })

	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var gotPath string
	var gotArgv []string
	var gotWD string
	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		gotPath = path
		gotArgv = argv
		gotWD, _ = os.Getwd()
		return nil
	}
	t.Cleanup(func() { execAgent = oldExec })

	Resume(Target{Source: "claude", SourceSessionID: "abc-123", Resumable: true, CWD: strp(target), Resume: []string{"claude", "--resume", "{id}"}})

	if !strings.HasSuffix(gotPath, string(filepath.Separator)+"claude") {
		t.Errorf("expected the resolved claude binary path, got %q", gotPath)
	}
	wantArgv := []string{"claude", "--resume", "abc-123"}
	if len(gotArgv) != len(wantArgv) {
		t.Fatalf("argv = %v, want %v", gotArgv, wantArgv)
	}
	for i := range wantArgv {
		if gotArgv[i] != wantArgv[i] {
			t.Errorf("argv[%d] = %q, want %q", i, gotArgv[i], wantArgv[i])
		}
	}
	resolvedWD, err := filepath.EvalSymlinks(gotWD)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedWD != target {
		t.Errorf("expected the agent to be started in %q, got %q", target, resolvedWD)
	}
}

// TestResumeOtherSourcesPassTheirOwnArguments covers the per-source resume
// arguments (design.md: "the resume arguments already used for each
// source"), for pi/omp/hermes.
func TestResumeOtherSourcesPassTheirOwnArguments(t *testing.T) {
	cases := []struct {
		source string
		tpl    []string
		want   []string
	}{
		{"pi", []string{"pi", "--session", "{id}"}, []string{"pi", "--session", "abc-123"}},
		{"omp", []string{"omp", "--resume", "{id}"}, []string{"omp", "--resume", "abc-123"}},
		{"hermes", []string{"hermes", "--resume", "{id}"}, []string{"hermes", "--resume", "abc-123"}},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			fakeAgentOnPath(t, tc.source)
			dir := t.TempDir()

			var gotArgv []string
			oldExec := execAgent
			execAgent = func(path string, argv []string, env []string) error {
				gotArgv = argv
				return nil
			}
			t.Cleanup(func() { execAgent = oldExec })

			Resume(Target{Source: tc.source, SourceSessionID: "abc-123", Resumable: true, CWD: strp(dir), Resume: tc.tpl})

			if len(gotArgv) != len(tc.want) {
				t.Fatalf("argv = %v, want %v", gotArgv, tc.want)
			}
			for i := range tc.want {
				if gotArgv[i] != tc.want[i] {
					t.Errorf("argv[%d] = %q, want %q", i, gotArgv[i], tc.want[i])
				}
			}
		})
	}
}

// TestResumeRefusesToStartWhenProfileUnresolved covers change
// fix-resume-session-identity, design.md decision 3: "If a profile's
// configuration cannot be determined, do not start the agent." execAgent is
// stubbed to record whether it was ever called at all - it must not be,
// since starting the agent without knowing which installation to point it
// at could resume a different session than the one asked for.
func TestResumeRefusesToStartWhenProfileUnresolved(t *testing.T) {
	fakeAgentOnPath(t, "claude")
	dir := t.TempDir()

	called := false
	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { execAgent = oldExec })

	out := Resume(Target{
		Source: "claude", SourceSessionID: "abc", Resumable: true, CWD: strp(dir),
		ProfileUnresolved: true, ProfileUnresolvedReason: "no discovered profile named \"ghost\"",
	})
	if !out.Failed {
		t.Fatalf("expected Failed, got %+v", out)
	}
	if called {
		t.Error("execAgent must never be called when the profile could not be resolved")
	}
	if !strings.Contains(out.Message, "ghost") {
		t.Errorf("expected the reason to be reported, got %q", out.Message)
	}
}

// TestResumeAppliesProfileEnvOverridingExistingValue covers design.md
// decision 3: the agent must be started with CLAUDE_CONFIG_DIR (or any
// other profile-scoped variable) set to the session's own profile - not
// merely appended alongside whatever the calling shell already exported,
// which could leave two conflicting entries in the environment slice.
func TestResumeAppliesProfileEnvOverridingExistingValue(t *testing.T) {
	fakeAgentOnPath(t, "claude")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "/should-not-be-used")

	var gotEnv []string
	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		gotEnv = env
		return nil
	}
	t.Cleanup(func() { execAgent = oldExec })

	Resume(Target{
		Source: "claude", SourceSessionID: "abc", Resumable: true, CWD: strp(dir),
		Resume:     []string{"claude", "--resume", "{id}"},
		ProfileEnv: map[string]string{"CLAUDE_CONFIG_DIR": "/home/x/.claude-personal"},
	})

	var found int
	var value string
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			found++
			value = strings.TrimPrefix(kv, "CLAUDE_CONFIG_DIR=")
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one CLAUDE_CONFIG_DIR entry in the agent's environment, found %d: %v", found, gotEnv)
	}
	if value != "/home/x/.claude-personal" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the session's own profile root", value)
	}
}

// TestResumeWithNoProfileEnvLeavesEnvironmentUntouched covers task 2.2:
// "apply only what the session's profile requires, leaving other sources
// unaffected" - a source with no ProfileEnv must get exactly the calling
// environment, unmodified.
func TestResumeWithNoProfileEnvLeavesEnvironmentUntouched(t *testing.T) {
	fakeAgentOnPath(t, "pi")
	dir := t.TempDir()
	t.Setenv("SOME_MARKER_VAR", "still-here")

	var gotEnv []string
	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		gotEnv = env
		return nil
	}
	t.Cleanup(func() { execAgent = oldExec })

	Resume(Target{Source: "pi", SourceSessionID: "abc", Resumable: true, CWD: strp(dir), Resume: []string{"pi", "--session", "{id}"}})

	found := false
	for _, kv := range gotEnv {
		if kv == "SOME_MARKER_VAR=still-here" {
			found = true
		}
	}
	if !found {
		t.Error("expected the calling environment to pass through unmodified when ProfileEnv is empty")
	}
}

func TestMergeEnvOverridesWithoutDuplicating(t *testing.T) {
	base := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/old", "FOO=bar"}
	out := mergeEnv(base, map[string]string{"CLAUDE_CONFIG_DIR": "/new"})

	count := 0
	for _, kv := range out {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			count++
			if kv != "CLAUDE_CONFIG_DIR=/new" {
				t.Errorf("got %q, want CLAUDE_CONFIG_DIR=/new", kv)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one CLAUDE_CONFIG_DIR entry, got %d in %v", count, out)
	}
	// Untouched variables survive.
	hasFoo := false
	for _, kv := range out {
		if kv == "FOO=bar" {
			hasFoo = true
		}
	}
	if !hasFoo {
		t.Errorf("expected FOO=bar to survive unrelated to the override, got %v", out)
	}
}

func TestMergeEnvEmptyOverridesReturnsBaseUnchanged(t *testing.T) {
	base := []string{"PATH=/bin"}
	out := mergeEnv(base, nil)
	if len(out) != 1 || out[0] != "PATH=/bin" {
		t.Errorf("got %v, want base unchanged", out)
	}
}

// TestResumeReportsFailureWhenAgentCannotBeStarted covers spec
// session-resume, "The agent cannot be started for any reason": Resume must
// report the failure and must not claim the session was resumed.
func TestResumeReportsFailureWhenAgentCannotBeStarted(t *testing.T) {
	fakeAgentOnPath(t, "claude")
	dir := t.TempDir()

	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		return errors.New("boom")
	}
	t.Cleanup(func() { execAgent = oldExec })

	out := Resume(Target{Source: "claude", SourceSessionID: "abc", Resumable: true, CWD: strp(dir), Resume: []string{"claude", "--resume", "{id}"}})
	if !out.Failed {
		t.Fatalf("expected Failed, got %+v", out)
	}
	if !strings.Contains(out.Message, "boom") {
		t.Errorf("expected the underlying error to be reported, got %q", out.Message)
	}
}

// TestAgentCommandUsesConfigDefaults pins the contract between the built-in
// config defaults and the commands agentCommand emits: the four default
// templates (internal/config defaultSources) must produce exactly the
// resume commands they produce today. The templates are read from the real
// config defaults (with HOME pinned to a temp dir so no file can interfere),
// so a future edit to the defaults fails loudly here instead of silently
// changing what `lazyrecall resume` runs.
func TestAgentCommandUsesConfigDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LAZYRECALL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source      string
		wantProgram string
		wantArgv    []string
	}{
		{"claude", "claude", []string{"claude", "--resume", "abc-123"}},
		{"pi", "pi", []string{"pi", "--session", "abc-123"}},
		{"omp", "omp", []string{"omp", "--resume", "abc-123"}},
		{"hermes", "hermes", []string{"hermes", "--resume", "abc-123"}},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			tpl := cfg.Sources[tc.source].Resume
			if len(tpl) == 0 {
				t.Fatalf("the default config has no resume template for %s", tc.source)
			}
			program, argv, ok := agentCommand(Target{Source: tc.source, SourceSessionID: "abc-123", Resume: tpl})
			if !ok {
				t.Fatalf("expected the default template for %s to be resumable", tc.source)
			}
			if program != tc.wantProgram {
				t.Errorf("program = %q, want %q", program, tc.wantProgram)
			}
			if !reflect.DeepEqual(argv, tc.wantArgv) {
				t.Errorf("argv = %q, want %q", argv, tc.wantArgv)
			}
		})
	}
}

// TestAgentCommandSubstitutesEmbeddedPlaceholder covers the "{id}" in the
// middle of an element (internal/config: "argv template; {id} is replaced
// with the session id") - substitution is whole-element via
// strings.ReplaceAll, not only a standalone "{id}" element.
func TestAgentCommandSubstitutesEmbeddedPlaceholder(t *testing.T) {
	program, argv, ok := agentCommand(Target{
		Source: "myagent", SourceSessionID: "sess-77",
		Resume: []string{"myagent", "--session={id}"},
	})
	if !ok {
		t.Fatal("expected the custom template to be resumable")
	}
	if program != "myagent" {
		t.Errorf("program = %q, want %q", program, "myagent")
	}
	want := []string{"myagent", "--session=sess-77"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// TestResumeEmptyTemplateDoesNotAttemptToExec covers the "no configured
// resume command" outcome: a source whose template is empty or missing must
// be reported as one lazyrecall does not know how to resume, and execAgent
// must never be reached - there is no command to exec.
func TestResumeEmptyTemplateDoesNotAttemptToExec(t *testing.T) {
	dir := t.TempDir()
	called := false
	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { execAgent = oldExec })

	out := Resume(Target{Source: "unknown", SourceSessionID: "abc", Resumable: true, CWD: strp(dir)})
	if !out.Failed {
		t.Fatalf("expected Failed, got %+v", out)
	}
	if called {
		t.Error("execAgent must never be called when the source has no resume template")
	}
	want := `"unknown" is not an agent lazyrecall knows how to resume.`
	if out.Message != want {
		t.Errorf("message = %q, want %q", out.Message, want)
	}
}

// TestResumeTemplateMetacharactersStayLiteralArgv covers the injection
// surface: a template whose elements contain shell metacharacters is handed
// to execAgent as literal argv elements, never parsed by a shell. The
// assertion is on the argv the exec double receives - the same vector
// execAgent would pass straight to the program.
func TestResumeTemplateMetacharactersStayLiteralArgv(t *testing.T) {
	fakeAgentOnPath(t, "myagent")
	dir := t.TempDir()

	var gotArgv []string
	oldExec := execAgent
	execAgent = func(path string, argv []string, env []string) error {
		gotArgv = argv
		return nil
	}
	t.Cleanup(func() { execAgent = oldExec })

	Resume(Target{
		Source: "myagent", SourceSessionID: "abc", Resumable: true, CWD: strp(dir),
		Resume: []string{"myagent", "--resume={id}", ";", "echo pwned && rm -rf /", "|", "sh"},
	})

	want := []string{"myagent", "--resume=abc", ";", "echo pwned && rm -rf /", "|", "sh"}
	if !reflect.DeepEqual(gotArgv, want) {
		t.Errorf("argv = %q, want the metacharacters passed through as literal elements %q", gotArgv, want)
	}
}
