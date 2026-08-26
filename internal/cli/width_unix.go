//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize mirrors struct winsize from <sys/ioctl.h> - the kernel's answer to
// TIOCGWINSZ, identical in layout on Darwin and Linux.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// terminalWidth asks the kernel for f's terminal column count via a raw
// TIOCGWINSZ ioctl - the standard technique for terminal size (and, doubling
// as an is-this-a-terminal check: the ioctl itself fails with ENOTTY against
// a pipe or a regular file), done directly through the stdlib "syscall"
// package rather than a third-party library (constraint 1: zero third-party
// Go modules).
func terminalWidth(f *os.File) (int, bool) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(tiocgwinsz), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 0, false
	}
	return int(ws.Col), true
}
