package session

import "strings"

// InstallFromID recovers the install name from LazyRecall's composite
// session id ("source:install:sourceSessionID") - the middle segment.
//
// It lives here, not in cmd/lazyrecall, because resolving "which install
// produced this session" is no longer something only the CLI's resume path
// needs (change group-sessions-in-one-index): once one index holds every
// install's sessions together, the browser's Transcript tab must also read
// a database-backed source's conversation from the session's own install,
// not from whichever install happens to be "active" - a notion that no
// longer exists once there is only one browsing session over the whole
// index.
func InstallFromID(compositeID string) string {
	parts := strings.SplitN(compositeID, ":", 3)
	if len(parts) == 3 {
		return parts[1]
	}
	return ""
}
