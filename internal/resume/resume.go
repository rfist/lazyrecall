// Package resume takes a chosen session back to its agent, running directly
// in the terminal LazyRecall was invoked from (spec session-resume; design.md
// decisions 1-3 of change resume-in-current-terminal). It changes to the
// session's recorded working directory, then hands off to the agent that
// produced it by replacing the running LazyRecall process (syscall.Exec on unix,
// see exec_unix.go) or, where that is unavailable, by running the agent as a
// child and exiting with its status (see exec_fallback.go). This supersedes
// the earlier herdr-delegated design: LazyRecall cannot leave its caller's shell
// in a directory after it exits, but it can run a program in one directly,
// which is all resumption ever needed. Nothing here ever writes to a session
// source - the only process that touches source data is the agent it hands
// off to.
package resume

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Target is the session being resumed.
type Target struct {
	Source          string
	SourceSessionID string
	CWD             *string
	GitRepoRoot     *string // offered as a fallback location if CWD is gone
	DirExists       *bool
	RepoRootExists  *bool
	Resumable       bool

	// Resume is the already-resolved argv template this session's source
	// uses to resume a session, with "{id}" standing in for the session id
	// (internal/config: a source's Resume). It is resolved by the caller
	// and passed in, exactly like ProfileEnv: resume stays a pure function
	// of its Target and never reads the config itself. agentCommand
	// substitutes "{id}" element-wise and hands the result to execAgent
	// directly - an argv vector, never a shell command string. Empty/nil
	// means the source has no configured resume command and cannot be
	// resumed.
	Resume []string

	// ProfileEnv carries environment variable overrides needed to run the
	// agent against the installation this session's own profile belongs to
	// - e.g. CLAUDE_CONFIG_DIR, for a claude session whose profile is not
	// the default installation (design.md decision 3 of change
	// fix-resume-session-identity: "internal/profile already models each
	// profile's Claude configuration root ... apply the session's profile
	// configuration to the environment"). Resolved by the caller, which
	// already knows how to map a profile name onto a config root via
	// internal/profile; empty/nil when the source's profile does not vary
	// (every source but claude, today - task 2.2, "apply only what the
	// session's profile requires").
	ProfileEnv map[string]string

	// ProfileUnresolved is true when the caller could not determine the
	// configuration this session's profile requires. Resume must report
	// this and must not start the agent: doing so could silently resume a
	// different session under the same identifier, in the wrong
	// installation (design.md decision 3's explicit warning; spec
	// session-resume, "Profile configuration cannot be determined").
	ProfileUnresolved bool
	// ProfileUnresolvedReason explains why, for the message Resume reports.
	ProfileUnresolvedReason string
}

// agentCommand is the program and argument vector that resumes a specific
// session non-interactively, built by substituting the session id into the
// source's configured argv template (t.Resume; internal/config: a source's
// Resume). argv[0] is conventionally the program's own name, matching what
// that program would see if invoked directly from a shell; the executable
// itself is resolved separately, via exec.LookPath, before anything else
// happens (decision 2).
//
// The result is deliberately an argv vector, never a shell command string:
// the config file is user-supplied input, and execAgent execs the program
// directly with no shell in between, so metacharacters in a template
// element (";", "&&", "|", ...) stay literal bytes of that one argument. A
// shell string built from the same template would be parsed and executed,
// turning a config file into a shell-injection surface - which is exactly
// why substitution is strings.ReplaceAll per element and nothing more.
func agentCommand(t Target) (program string, argv []string, ok bool) {
	if len(t.Resume) == 0 {
		return "", nil, false
	}
	argv = make([]string, len(t.Resume))
	for i, el := range t.Resume {
		argv[i] = strings.ReplaceAll(el, "{id}", t.SourceSessionID)
	}
	return argv[0], argv, true
}

// Outcome describes why resumption did not place the user into the session.
// Resume never returns a success value: when the agent actually starts, it
// either replaces the LazyRecall process outright or (on the child-process
// fallback) runs to completion and LazyRecall exits with its status - either way
// there is no LazyRecall left to report anything back to (spec session-resume,
// "Resumption reports success only when the agent runs"). A value is only
// ever returned along a failure path.
type Outcome struct {
	// NotResumable is true when the session has no working directory at all
	// and never can be resumed.
	NotResumable bool

	// DirectoryMissing is true when the recorded working directory no
	// longer exists.
	DirectoryMissing bool

	// AlternativeOffered is the repository root offered in place of a
	// missing working directory, when it still exists. Never a substitute
	// directory LazyRecall picked on its own - only ever this session's own
	// recorded repo root.
	AlternativeOffered *string

	// Failed is true when the agent could not be started for any other
	// reason: its program is not installed, the working directory could not
	// be entered, or starting it outright failed.
	Failed bool

	// Message is a human-readable summary of the outcome.
	Message string
}

// execAgent replaces the running process with the program at path (argv is
// its full argument vector, argv[0] included; env is the environment to run
// it under) - or, on platforms where that is unavailable, runs it as a child
// with the terminal passed through and exits this process with its status.
// It only ever returns when the agent could not be started at all: on any
// path that actually starts the agent, this process is gone by the time
// execAgent would return. A package variable so tests can substitute a
// double that records its arguments instead of truly replacing the test
// binary. Defaults to platformExec, defined per-platform in exec_unix.go
// (syscall.Exec) or exec_fallback.go (child process + os.Exit).
var execAgent = platformExec

// Resume places the user into t's session: changes to its recorded working
// directory, then hands off to the agent that produced it (design.md
// decisions 1-3; spec session-resume). It never writes anything to t's
// source - the only side effect is handing off to another process.
func Resume(t Target) Outcome {
	if !t.Resumable || t.CWD == nil || *t.CWD == "" {
		return Outcome{
			NotResumable: true,
			Message:      "This session has no recorded working directory and cannot be resumed.",
		}
	}

	if t.DirExists != nil && !*t.DirExists {
		out := Outcome{DirectoryMissing: true}
		if t.GitRepoRoot != nil && *t.GitRepoRoot != "" && (t.RepoRootExists == nil || *t.RepoRootExists) {
			out.AlternativeOffered = t.GitRepoRoot
			out.Message = fmt.Sprintf("The recorded working directory %q no longer exists. Its repository root %q does; you can resume there instead.", *t.CWD, *t.GitRepoRoot)
		} else {
			out.Message = fmt.Sprintf("The recorded working directory %q no longer exists, and no related directory could be found. The session's location is gone.", *t.CWD)
		}
		return out
	}

	// A session whose profile configuration could not be determined must
	// never be started at all (design.md decision 3) - checked before
	// anything else so an unresolved profile can never race past program
	// resolution or the working-directory change below.
	if t.ProfileUnresolved {
		msg := "could not determine which installation's configuration this session's profile requires"
		if t.ProfileUnresolvedReason != "" {
			msg += ": " + t.ProfileUnresolvedReason
		}
		msg += fmt.Sprintf("; refusing to start %s, since doing so could resume the wrong session.", t.Source)
		return Outcome{Failed: true, Message: msg}
	}

	program, argv, ok := agentCommand(t)
	if !ok {
		return Outcome{Failed: true, Message: fmt.Sprintf("%q is not an agent lazyrecall knows how to resume.", t.Source)}
	}

	// Resolve the program before touching anything else (decision 2; spec
	// "makes no other change" when the program is missing) - this is the
	// one dependency resumption genuinely has, and it belongs to the agent,
	// not to LazyRecall.
	path, err := exec.LookPath(program)
	if err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("%s is not installed; the session cannot be resumed without it.", program)}
	}

	if err := os.Chdir(*t.CWD); err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("could not change to %q: %v", *t.CWD, err)}
	}

	env := mergeEnv(os.Environ(), t.ProfileEnv)
	if err := execAgent(path, argv, env); err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("could not start %s: %v", program, err)}
	}

	// Unreachable when the agent actually started: execAgent either
	// replaced this process (never returning at all) or ran the agent to
	// completion and exited this process with its status. Kept as a safety
	// net rather than a panic, in case a future execAgent implementation
	// ever legitimately returns nil without one of those two things having
	// happened.
	return Outcome{Failed: true, Message: "resumption ended unexpectedly"}
}

// mergeEnv returns base with every key in overrides set to its override
// value, replacing any existing entry for that key rather than appending a
// second one alongside it. A plain append would leave two entries for the
// same variable in the environment slice; some C runtimes' getenv scans
// from the start and would still see the old value, silently defeating the
// override this function exists to apply (design.md decision 3). Returns
// base unmodified (not even a copy) when overrides is empty, which is the
// overwhelming common case (task 2.2: every source but claude today).
func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if _, overridden := overrides[key]; overridden {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}
