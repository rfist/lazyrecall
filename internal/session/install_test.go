package session

import "testing"

func TestInstallFromIDCompositeID(t *testing.T) {
	got := InstallFromID("claude:claude-personal:abc-123")
	if got != "claude-personal" {
		t.Errorf("got %q, want claude-personal", got)
	}
}

func TestInstallFromIDMalformedIDReturnsEmpty(t *testing.T) {
	if got := InstallFromID("not-a-composite-id"); got != "" {
		t.Errorf("got %q, want empty for a malformed id", got)
	}
}
