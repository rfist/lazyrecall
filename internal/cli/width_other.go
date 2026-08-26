//go:build !darwin && !linux

package cli

import "os"

// terminalWidth has no portable implementation outside darwin/linux here;
// callers fall back to a fixed default width (DetermineOptions).
func terminalWidth(f *os.File) (int, bool) { return 0, false }
