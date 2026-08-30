// Package cli is the command surface: human-readable default output, a
// machine-readable JSON mode, and interactive picking - the in-process
// browser, or the numbered-list fallback in pick.go (spec session-search /
// session-review / session-resume outputs; tasks 11.1-11.3).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/rfist/lazyrecall/internal/review"
	"github.com/rfist/lazyrecall/internal/search"
)

// jsonItem is the machine-readable shape of one session (task 11.2). Field
// names are stable and independent of the human-readable rendering.
type jsonItem struct {
	SessionID      string   `json:"session_id"`
	Source         string   `json:"source"`
	LineageID      string   `json:"lineage_id"`
	Handle         int      `json:"handle,omitempty"`
	CWD            *string  `json:"cwd,omitempty"`
	GitBranch      *string  `json:"git_branch,omitempty"`
	GitRepoRoot    *string  `json:"git_repo_root,omitempty"`
	StartedAt      *string  `json:"started_at,omitempty"`
	LastActivityAt *string  `json:"last_activity_at,omitempty"`
	Topic          *string  `json:"topic,omitempty"`
	Name           *string  `json:"name,omitempty"`
	LastPrompt     *string  `json:"last_prompt,omitempty"`
	EndState       string   `json:"end_state"`
	Origin         string   `json:"origin,omitempty"`
	Client         *string  `json:"client,omitempty"`
	DirExists      *bool    `json:"dir_exists,omitempty"`
	MessageCount   *int64   `json:"message_count,omitempty"`
	Resumable      bool     `json:"resumable"`
	Archived       bool     `json:"archived,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	MatchSnippet   string   `json:"match_snippet,omitempty"`
}

func toJSONItem(it search.Item) jsonItem {
	j := jsonItem{
		SessionID: it.SessionID, Source: it.Source, LineageID: it.LineageID, Handle: it.Handle,
		CWD: it.CWD, GitBranch: it.GitBranch, GitRepoRoot: it.GitRepoRoot,
		Topic: it.Topic, Name: it.Name, LastPrompt: it.LastPrompt, EndState: string(it.EndState),
		Origin:    string(it.Origin),
		Client:    it.Client,
		DirExists: it.DirExists, MessageCount: it.MessageCount, Resumable: it.Resumable,
		Archived: it.Archived, Tags: it.Tags, MatchSnippet: it.MatchSnippet,
	}
	if it.StartedAt != nil {
		s := it.StartedAt.Format(time.RFC3339)
		j.StartedAt = &s
	}
	if it.LastActivityAt != nil {
		s := it.LastActivityAt.Format(time.RFC3339)
		j.LastActivityAt = &s
	}
	return j
}

// ToJSONItems converts search results into the stable machine-readable
// shape (task 11.2) - snake_case field names, absent fields omitted rather
// than emitted as null/zero. Exported so callers that need to wrap the
// array (e.g. with the active profile name) can still go through the same
// conversion WriteItemsJSON uses.
func ToJSONItems(items []search.Item) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = toJSONItem(it)
	}
	return out
}

// WriteItemsJSON writes items as a JSON array (task 11.2).
func WriteItemsJSON(w io.Writer, items []search.Item) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ToJSONItems(items))
}

// WriteItemsHuman renders items as a human-readable listing (task 11.1,
// 4.1-4.5; spec session-search, "Unified session listing with preview" /
// "A listed session occupies one line that fits the terminal" / "Fields
// are visually distinguishable"): one line per session, handle inline, no
// separate identifier line, truncated to opts.Width, styled per opts.Style.
func WriteItemsHuman(w io.Writer, items []search.Item, emptyMessage string, opts RenderOptions) {
	if len(items) == 0 {
		fmt.Fprintln(w, emptyMessage)
		return
	}
	for _, it := range items {
		fmt.Fprintln(w, RenderRow(it, opts))
		if it.MatchSnippet != "" {
			// Match context (spec session-search, "Match context is
			// shown") is supplementary to the entry, not part of it - kept
			// on its own indented line rather than folded into the
			// one-line entry RenderRow produces (which RenderRow must keep
			// to exactly one line, since the interactive picker feeds the
			// same string to the picker as a single candidate row). The snippet
			// comes from the FTS index over prompt text, which can itself
			// contain a line break - sanitized the same way row text is
			// (change fix-row-newlines-and-profile-switch) so this
			// supplementary line stays one line too.
			fmt.Fprintf(w, "      %s\n", sanitizeSingleLine(it.MatchSnippet))
		}
	}
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// WriteReviewHuman renders the loose-ends report (task 8.3's fields, task
// 11.1's default format; task 4.5: the same row rendering as list/search).
func WriteReviewHuman(w io.Writer, entries []review.Entry, emptyMessage string, opts RenderOptions) {
	items := make([]search.Item, len(entries))
	for i, e := range entries {
		items[i] = e.Item
	}
	WriteItemsHuman(w, items, emptyMessage, opts)
}

// WriteReviewJSON renders the loose-ends report as JSON.
func WriteReviewJSON(w io.Writer, entries []review.Entry) error {
	items := make([]search.Item, len(entries))
	for i, e := range entries {
		items[i] = e.Item
	}
	return WriteItemsJSON(w, items)
}
