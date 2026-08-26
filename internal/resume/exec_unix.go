//go:build unix

package resume

import "syscall"

// platformExec replaces the running process with the agent at path via
// syscall.Exec (design.md decision 1 of change resume-in-current-terminal):
// the same PID, the same terminal, the same signal disposition, continue on
// as the agent. It only returns when the replacement itself failed to
// happen at all - on success there is nothing left of this process to
// return to.
func platformExec(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}
