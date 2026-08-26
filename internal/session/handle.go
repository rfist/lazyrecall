package session

// IsHandle reports whether arg has the shape of a short session handle
// rather than a fully-qualified session identifier (spec session-search,
// "Short session handle"; design.md decision 4: "An argument that is
// entirely digits is a handle; anything else is a fully-qualified
// identifier."). The two forms can never collide, since a fully-qualified
// identifier always contains "<source>:<profile>:<...>" separators.
func IsHandle(arg string) bool {
	if arg == "" {
		return false
	}
	for _, r := range arg {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
