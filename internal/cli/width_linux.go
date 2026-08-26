//go:build linux

package cli

// TIOCGWINSZ on Linux (from <asm-generic/ioctls.h>).
const tiocgwinsz = 0x5413
