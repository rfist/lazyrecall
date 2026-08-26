//go:build !unix

package resume

import (
	"os"
	"os/exec"
)

// platformExec is the fallback for platforms where replacing the running
// process is not available (design.md decision 5 of change
// resume-in-current-terminal): it runs the agent as a child with the
// terminal passed straight through, waits for it, and exits this process
// with the child's own status. The observable behaviour is the same as the
// unix path - the agent runs in the current terminal and Recall does not
// remain running once it has - even though, unlike syscall.Exec, this
// process technically stays alive (as the child's parent) for the agent's
// lifetime rather than being replaced by it. It only returns to its caller
// when the agent could not even be started.
func platformExec(path string, argv []string, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err := cmd.Wait()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		// The agent started but its outcome cannot be read as an exit
		// code (e.g. killed by a signal on a platform that reports that
		// differently) - fall through with a nonzero status rather than
		// silently reporting success.
		code = 1
	}
	os.Exit(code)
	return nil // unreachable
}
