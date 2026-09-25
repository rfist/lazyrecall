// Date separator rows for the Sessions panel (change date-separator-rows):
// "── Today ──", "── Yesterday ──", "── Last 7 days ──", "── Last 30 days
// ──", "── Older ──", and "── Unknown date ──" for a session with no
// recorded LastActivityAt at all, dim and fitted to the panel's inner width.
//
// A header is real content sharing the fixed row budget geometry() hands the
// Sessions panel, not decoration layered on top of it - so every piece of
// arithmetic that used to assume "one visible row per session" (keepCursorVisible,
// sessionsPanel's own scroll window) has to walk a mixed list of session and
// header rows instead. displayRow is that mixed list's element, and
// sessionDisplayRows is what builds it from m.visible; m.cursor and
// m.listTop stay session indices throughout; see keepCursorVisible and
// sessionsPanel (browse.go) for how they walk it.
package cli

import (
	"math"
	"strings"
	"time"

	"github.com/rfist/lazyrecall/internal/search"
)

// dateBucket is one of the Sessions panel's time buckets, ordered oldest-last
// the way they are drawn: a session's bucket only ever gets larger walking
// down a recency-ordered list, never smaller, which is exactly what
// recencyOrdered checks for.
type dateBucket int

const (
	bucketToday dateBucket = iota
	bucketYesterday
	bucketLast7Days
	bucketLast30Days
	bucketOlder
	// bucketUnknown is a session with no recorded LastActivityAt at all. It
	// sorts last regardless of anything else - both because SQLite's own
	// `ORDER BY last_activity_at DESC` already puts a NULL timestamp after
	// every real one (NULLs sort as the smallest value, so DESC trails them),
	// and because a session with no known activity time has no place to sit
	// among sessions that do.
	bucketUnknown
)

// label is the header text drawn for the bucket, e.g. "── Today ──"
// (dateHeaderLine adds the rule and the dimming).
func (b dateBucket) label() string {
	switch b {
	case bucketToday:
		return "Today"
	case bucketYesterday:
		return "Yesterday"
	case bucketLast7Days:
		return "Last 7 days"
	case bucketLast30Days:
		return "Last 30 days"
	case bucketOlder:
		return "Older"
	case bucketUnknown:
		return "Unknown date"
	}
	return ""
}

// bucketFor buckets one session by LastActivityAt against now, in the
// session's local time (time.Unix, which is how search.Item.LastActivityAt
// is built, already returns Local - internal/search/search.go).
func bucketFor(it search.Item, now time.Time) dateBucket {
	if it.LastActivityAt == nil {
		return bucketUnknown
	}
	return bucketForTime(*it.LastActivityAt, now)
}

// bucketForTime buckets t against now at local-midnight boundaries: two
// timestamps on the same calendar day are both Today regardless of how many
// hours apart they are, and a timestamp one calendar day back is Yesterday
// even if it is less than 24 hours ago (11pm last night). midnight collapses
// each to its own day's local midnight first, so what is compared is whole
// calendar days, not elapsed duration.
//
// The subtraction is rounded to the nearest day, not truncated, so a
// daylight-saving transition - which can make one calendar day 23 or 25
// hours long instead of 24 - never quietly shifts a session into the bucket
// next to the one its actual calendar date belongs in.
func bucketForTime(t, now time.Time) dateBucket {
	days := int(math.Round(midnight(now).Sub(midnight(t)).Hours() / 24))
	switch {
	case days <= 0:
		// <= 0 covers both "later today" and a session whose recorded
		// activity is technically after now (clock skew between this
		// process and whatever wrote the timestamp) - either way, the only
		// bucket that makes sense for something not in the past is Today.
		return bucketToday
	case days == 1:
		return bucketYesterday
	case days <= 7:
		return bucketLast7Days
	case days <= 30:
		return bucketLast30Days
	default:
		return bucketOlder
	}
}

// midnight is t's local calendar day at 00:00:00, the boundary bucketForTime
// compares against.
func midnight(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
}

// recencyOrdered reports whether items is already sorted newest-first by
// LastActivityAt, treating "no timestamp" as older than every real one (the
// same rule bucketUnknown sorting last follows) - checked empirically
// against the actual rows on every render, rather than inferred from which
// query state produced them.
//
// That is a deliberate choice, not the obvious one: the browser has three
// states that can populate the Sessions list - the plain listing
// (search.List), the "/" row filter (applyTextFilter, a walk over what is
// already loaded), and the "s" full-text search (search.Search). Reading
// their SQL settles which of them are newest-first: List and Search both
// `ORDER BY s.last_activity_at DESC` as their PRIMARY sort key - Search adds
// `, rank` only to break ties between sessions sharing the exact same
// timestamp, it does not rank by relevance first - and neither
// applyTextFilter nor the facet narrowing in narrow() (facet.go) ever
// reorders what they are given. Every state this browser can be in today is
// therefore already newest-first, which means headers show in all of them.
// Naming those three states in an if/else here would be repeating that
// finding as a set of assumptions instead of leaving it checked: the day
// this ever stops being true - a future change to Search's ORDER BY, most
// plausibly - this function stops finding the list sorted and the panel
// falls back to no headers on its own, rather than drawing a "Today" section
// with an older session inside it because nothing rechecked the premise.
func recencyOrdered(items []search.Item) bool {
	for i := 1; i < len(items); i++ {
		if lastActivityOrZero(items[i-1]).Before(lastActivityOrZero(items[i])) {
			return false
		}
	}
	return true
}

// lastActivityOrZero is recencyOrdered's nil-safe accessor: the Go zero
// time.Time sorts before every real timestamp, which is what makes a run of
// no-LastActivityAt sessions trailing at the end of the list compare as
// "still sorted" rather than as a violation.
func lastActivityOrZero(it search.Item) time.Time {
	if it.LastActivityAt == nil {
		return time.Time{}
	}
	return *it.LastActivityAt
}

// rowKind distinguishes a displayRow's two shapes.
type rowKind int

const (
	rowSession rowKind = iota
	rowHeader
)

// displayRow is one line the Sessions panel can draw: either a session (by
// its index into m.visible) or a bucket header. session is meaningless for a
// header row and bucket is meaningless for a session row - callers switch on
// kind before reading either, the same convention search.Item's own optional
// fields follow.
type displayRow struct {
	kind    rowKind
	session int
	bucket  dateBucket
}

// sessionDisplayRows builds the Sessions panel's mixed row list from
// m.visible: one displayRow per session, with a header row spliced in front
// of every bucket's first session, in bucket order - or, when headers should
// not show at all (m.dateHeaders is off, or m.visible is not recencyOrdered
// - see its own doc comment for why that is checked empirically rather than
// assumed), exactly one row per session and no headers, so every caller
// downstream (keepCursorVisible, sessionsPanel) can walk the same shape
// regardless of whether any header is actually showing.
//
// The second return value, sessionRow, maps a session's index in m.visible
// to its own row's index in the first return value - what lets
// keepCursorVisible and sessionsPanel translate m.cursor and m.listTop
// (session indices, unchanged by this feature) into the row range that
// includes them, in O(1) per lookup instead of a scan.
func (m browseModel) sessionDisplayRows() (rows []displayRow, sessionRow []int) {
	rows = make([]displayRow, 0, len(m.visible))
	sessionRow = make([]int, len(m.visible))

	headers := m.dateHeaders && recencyOrdered(m.visible)
	last := dateBucket(-1) // no real bucket has this value, so item 0 always opens one
	for i, it := range m.visible {
		if headers {
			b := bucketFor(it, m.clock())
			if b != last {
				rows = append(rows, displayRow{kind: rowHeader, bucket: b})
				last = b
			}
		}
		sessionRow[i] = len(rows)
		rows = append(rows, displayRow{kind: rowSession, session: i})
	}
	return rows, sessionRow
}

// windowCost is how many display rows it takes to show visible sessions
// [top, bottom] inclusive: the run of rows between their two session rows
// (which already counts every header for a bucket that starts strictly
// between them), plus one more when top itself opens a bucket - the one
// header a same-range scan of sessionRow[top]..sessionRow[bottom] cannot see
// because it sits immediately before that range, not inside it.
func windowCost(rows []displayRow, sessionRow []int, top, bottom int) int {
	cost := sessionRow[bottom] - sessionRow[top] + 1
	if sessionRow[top] > 0 && rows[sessionRow[top]-1].kind == rowHeader {
		cost++
	}
	return cost
}

// clock is the Sessions panel's "now" for bucketing - m.now when a test has
// set one, else time.Now (see browseModel.now's doc comment).
func (m browseModel) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// dateHeaderLine renders one bucket separator, dim and centered within
// width: "── Today ──" with the two flanking rules extended to fill exactly
// width columns, the same "fitted to the panel" contract every other line
// sessionsPanel draws follows. A width too narrow for even the bracketed
// label falls back to the bare label, truncated like any other field
// (truncateToWidth) rather than drawing rules with nothing readable between
// them.
func dateHeaderLine(b dateBucket, width int, styled bool) string {
	if width <= 0 {
		return ""
	}
	label := " " + b.label() + " "
	labelWidth := visibleWidth(label)
	if labelWidth >= width {
		return style(truncateToWidth(strings.TrimSpace(label), width), ansiDim, styled)
	}
	left := (width - labelWidth) / 2
	right := width - labelWidth - left
	line := strings.Repeat("─", left) + label + strings.Repeat("─", right)
	return style(line, ansiDim, styled)
}
