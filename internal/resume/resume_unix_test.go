//go:build unix

package resume

// TestResumeReplacesProcessWithAgent is the real end-to-end proof of
// design.md decision 1 (change resume-in-current-terminal): Resume must
// truly replace the process, not fork-and-wait, and task 1.5 requires
// verifying that no Recall process remains once the agent is running. A
// unit test cannot call Resume() directly with the real execAgent and
// observe that, because a successful syscall.Exec never returns - it would
// simply end the test binary. So this test re-execs the test binary itself
// as a subprocess with an environment variable telling it to become a
// resume "helper" that calls the real Resume() with a stub "claude" program
// on PATH (and nothing else on PATH - covering task 5.2, "neither herdr nor
// fzf on PATH"). Resume()'s own syscall.Exec then replaces that helper
// process outright: what the outer test observes afterward - the stub's
// distinctive exit code and the argv/cwd it recorded before exiting - could
// only have come from the stub actually running in the helper's place, not
// from anything Recall itself printed or returned.
import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	helperEnvVar    = "RECALL_RESUME_TEST_HELPER"
	helperCWDVar    = "RECALL_RESUME_TEST_CWD"
	helperMarkerVar = "RECALL_RESUME_TEST_MARKER"
	stubExitCode    = 42
)

func TestResumeReplacesProcessWithAgent(t *testing.T) {
	if os.Getenv(helperEnvVar) == "1" {
		runResumeHelper(t)
		return
	}

	workDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")

	stub := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"argv0=$0\"\n" +
		"  i=0\n" +
		"  for a in \"$@\"; do echo \"arg$i=$a\"; i=$((i+1)); done\n" +
		"  echo \"cwd=$(pwd -P)\"\n" +
		"} > " + shq(marker) + "\n" +
		"exit " + strconv.Itoa(stubExitCode) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestResumeReplacesProcessWithAgent$")
	cmd.Env = []string{
		helperEnvVar + "=1",
		helperCWDVar + "=" + workDir,
		helperMarkerVar + "=" + marker,
		// PATH contains only the stub - proves task 5.2 ("neither herdr
		// nor fzf on PATH") along the way, since neither is present here.
		"PATH=" + binDir,
		"HOME=" + t.TempDir(),
	}
	out, runErr := cmd.CombinedOutput()

	exitErr, isExitErr := runErr.(*exec.ExitError)
	if !isExitErr {
		t.Fatalf("expected the helper process to exit with the stub's own status; got err=%v output=%s", runErr, out)
	}
	if code := exitErr.ExitCode(); code != stubExitCode {
		t.Fatalf("expected exit code %d (the stub's), got %d; helper output: %s", stubExitCode, code, out)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected the stub to have run and written its marker (proving it - not Recall - was the process that kept running): %v; helper output: %s", err, out)
	}
	content := string(data)
	argv0 := ""
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, "argv0="); ok {
			argv0 = v
		}
	}
	// Not exactly "claude": the stub is invoked through a "#!/bin/sh"
	// shebang, and the kernel's own shebang handling replaces argv[0] with
	// the script's own path before the interpreter ever sees it, regardless
	// of what argv[0] execve was actually called with - that substitution
	// happens beneath Resume() and is not something it controls. Checking
	// that the path ends in "/claude" still confirms the right program was
	// resolved and run.
	if !strings.HasSuffix(argv0, string(filepath.Separator)+"claude") {
		t.Errorf("expected argv0 to resolve to the claude stub, got %q (full marker: %q)", argv0, content)
	}
	if !strings.Contains(content, "arg0=--resume") || !strings.Contains(content, "arg1=abc-123") {
		t.Errorf("expected the claude resume arguments, got %q", content)
	}
	if !strings.Contains(content, "cwd="+workDir) {
		t.Errorf("expected the agent to run in the session's recorded working directory %q, got %q", workDir, content)
	}
}

// runResumeHelper is the re-exec'd side: it calls the real, unstubbed
// Resume() so the genuine syscall.Exec path runs. Reaching the end of this
// function at all means Resume failed to replace the process - which the
// outer test would catch as a mismatched exit code / missing marker file,
// but exit distinctly here too so a failure is never mistaken for the
// stub's own exit code.
func runResumeHelper(t *testing.T) {
	cwd := os.Getenv(helperCWDVar)
	out := Resume(Target{Source: "claude", SourceSessionID: "abc-123", Resumable: true, CWD: &cwd})
	t.Logf("resume helper: Resume unexpectedly returned: %+v", out)
	os.Exit(97)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
