// Package adapter defines the boundary between "finding sessions" and
// "reading their content" (design.md decision 3). An Adapter's only job is
// to enumerate what exists and where; it never parses transcripts - that is
// internal/transcript's job, invoked by internal/refresh once identity is
// known (task 5.1).
package adapter

import (
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/session"
)

// Discovered is one session an adapter found, with whatever its source's
// own index cheaply provides already filled in on Session. Fields the
// adapter cannot cheaply provide are left nil - the refresh pipeline fills
// them in from a transcript scan (for sources that have transcripts) or
// leaves them absent (for sources that never record them).
type Discovered struct {
	Session session.Session

	// TranscriptPath is where refresh should read this session's
	// transcript for tier 1/2. Empty for sources with no transcripts
	// (hermes), which must fill Session completely themselves.
	TranscriptPath string

	// FileSize is the transcript's current size in bytes, used by the
	// refresh pipeline's rewrite-detection (task 6.2). Meaningless when
	// TranscriptPath is empty.
	FileSize int64
}

// Adapter enumerates the sessions one source has on disk. It does not read
// transcript content itself.
type Adapter interface {
	// Name identifies this adapter's source: "claude", "pi", "omp", or
	// "hermes".
	Name() string

	// Discover lists every session currently discoverable for profile p. A
	// source that is absent, empty, or unreadable returns a nil slice and a
	// non-nil error describing why; callers must not treat that as fatal -
	// per-source failures are reported against that source alone and never
	// block the other sources (spec session-index, "Source discovery";
	// task 5.6).
	Discover(p profile.Profile) ([]Discovered, error)
}

// Unavailable is returned by Discover when a source has no root configured
// for this profile at all (as opposed to a root that exists but failed to
// read). It is not an error condition to report loudly - it just means
// this profile has nothing from this source.
type Unavailable struct{ Reason string }

func (u *Unavailable) Error() string { return u.Reason }
