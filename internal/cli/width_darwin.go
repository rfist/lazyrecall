//go:build darwin

package cli

// TIOCGWINSZ on Darwin (from <sys/ttycom.h>).
const tiocgwinsz = 0x40087468
