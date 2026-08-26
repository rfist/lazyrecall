package session

import "testing"

func TestIsHandle(t *testing.T) {
	cases := map[string]bool{
		"":              false,
		"3":             true,
		"42":            true,
		"0":             true,
		"claude:p:uuid": false,
		"claude:p:3":    false, // contains separators - fully-qualified, not a handle
		"-1":            false,
		"1.5":           false,
		"claude":        false,
		" 3":            false,
		"3 ":            false,
	}
	for arg, want := range cases {
		if got := IsHandle(arg); got != want {
			t.Errorf("IsHandle(%q) = %v, want %v", arg, got, want)
		}
	}
}
