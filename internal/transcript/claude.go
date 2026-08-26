package transcript

import "strings"

// claudeVocab maps Claude Code's transcript record shape. Record types seen
// in the wild: "user", "assistant", "system" (subtype "compact_boundary"),
// "ai-title", "custom-title", "last-prompt", "queue-operation",
// "attachment". Only the first six carry anything LazyRecall needs; the rest
// are recognized and
// intentionally ignored (classified as not-ok, i.e. skipped) rather than
// treated as malformed.
type claudeVocab struct{}

// injectedBlockMarkers are the wrapper tags Claude Code's harness itself
// injects into a "user"-role record - text that was not written by the
// user and must never be treated as a prompt (change
// fix-cli-usability-defects, design.md decision 2: one classification of
// non-user text covering every kind of injected block a source is known to
// emit, replacing the narrower marker-specific check that only covered
// local slash-command output). Confirmed against real transcripts on this
// machine (structure only, never copied into this repository - see
// devdocs/fyi.md):
//
//   - command-name / local-command-stdout / local-command-caveat: local
//     slash-command invocation and output (the original markers);
//   - command-message: the newer wrapper that carries a command
//     invocation (its content begins with <command-name>...);
//   - task-notification: background task notifications, recorded with
//     origin.kind "task-notification" and promptSource "system";
//   - system-reminder: harness reminders;
//   - bash-input / bash-stdout / bash-stderr: terminal-session capture
//     (a command the user ran in a pane and its output - tool output,
//     not a prompt to the agent);
//   - context: a harness-injected context block referencing a file
//     ("<context ref=\"file://...\">..."), recorded with promptSource
//     "sdk" - file contents, not user text.
//
// Notably absent: "leader". A real record whose content begins
// "<leader>" was found with promptSource "typed" and origin.kind
// "human" - genuinely user-written text that happens to resemble a tag,
// which must stay searchable (task 2.4).
var injectedBlockMarkers = []string{
	"command-name",
	"command-message",
	"local-command-stdout",
	"local-command-caveat",
	"task-notification",
	"system-reminder",
	"bash-input",
	"bash-stdout",
	"bash-stderr",
	"context",
}

// isInjectedBlock reports whether text is one of Claude Code's own
// injected blocks rather than something the user typed (change
// fix-cli-usability-defects, task 2.1 - the single classification applied
// wherever prompts are extracted). Matching stays structural and anchored
// on the form a harness emits: the text's own leading "<tag>" or
// "<tag attr=...>" form, never a tag occurring elsewhere inside genuine
// user-typed text (task 2.4) - so a user who happens to write these
// characters mid-message, or a user-typed tag like "<leader>", is never
// excluded.
func isInjectedBlock(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<") {
		return false
	}
	for _, tag := range injectedBlockMarkers {
		prefix := "<" + tag
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		// The tag must be a real tag boundary, not a longer identifier:
		// "<task-notification>", "<task-notification ...>", or the tag
		// alone at end of text. "<task-notification-x>" is not one.
		rest := t[len(prefix):]
		if rest == "" {
			return true
		}
		switch rest[0] {
		case '>', ' ', '\t', '\n':
			return true
		}
	}
	return false
}

func (claudeVocab) Classify(raw map[string]any) (Record, bool) {
	typ, _ := raw["type"].(string)

	ts := parseTime(asString(raw["timestamp"]))

	switch typ {
	case "ai-title":
		title, _ := raw["aiTitle"].(string)
		if title == "" {
			return Record{}, false
		}
		return Record{Kind: KindTopic, Timestamp: ts, Text: strPtr(title)}, true

	case "custom-title":
		// The user renamed the session inside Claude Code. The record
		// carries no timestamp and repeats with the current name as the
		// session goes on; Scan keeps the last one, so a later rename
		// supersedes an earlier one.
		title, _ := raw["customTitle"].(string)
		if title == "" {
			return Record{}, false
		}
		return Record{Kind: KindCustomTitle, Timestamp: ts, Text: strPtr(title)}, true

	case "last-prompt":
		p, _ := raw["lastPrompt"].(string)
		if p == "" || isInjectedBlock(p) {
			return Record{}, false
		}
		return Record{Kind: KindLastPrompt, Timestamp: ts, Text: strPtr(p)}, true

	case "system":
		if sub, _ := raw["subtype"].(string); sub == "compact_boundary" {
			ci := &CompactionInfo{}
			if meta, ok := raw["compactMetadata"].(map[string]any); ok {
				if trig, ok := meta["trigger"].(string); ok {
					auto := trig == "auto"
					ci.Automatic = &auto
				}
				if v, ok := numberField(meta, "cumulativeDroppedTokens"); ok {
					ci.DroppedTokens = &v
				}
				if v, ok := numberField(meta, "preTokens"); ok {
					ci.PreTokens = &v
				}
			}
			rec := Record{Kind: KindCompactionBoundary, Timestamp: ts, Compaction: ci}
			// compact_boundary records also carry cwd/gitBranch like any
			// other record in the file - capture opportunistically.
			rec.CWD, rec.GitBranch = claudeCWDBranch(raw)
			return rec, true
		}
		return Record{}, false

	case "user":
		msg, _ := raw["message"].(map[string]any)
		cwd, branch := claudeCWDBranch(raw)
		content := msg["content"]
		switch c := content.(type) {
		case string:
			if c == "" || isInjectedBlock(c) {
				// Empty prompt, or Claude Code's own injected block
				// (local-command invocation/output, task notification,
				// system reminder, context block, ...) rather than
				// something the user typed - still worth reporting session
				// meta, never worth indexing as a prompt.
				if cwd != nil || branch != nil {
					return Record{Kind: KindSessionMeta, Timestamp: ts, CWD: cwd, GitBranch: branch}, true
				}
				return Record{}, false
			}
			return Record{Kind: KindUserPrompt, Timestamp: ts, CWD: cwd, GitBranch: branch, Text: strPtr(c)}, true
		case []any:
			if hasBlockType(c, "tool_result") {
				return Record{Kind: KindToolResult, Timestamp: ts, CWD: cwd, GitBranch: branch}, true
			}
			if text, ok := textFromBlocks(c, "text", "text"); ok && text != "" {
				if isInjectedBlock(text) {
					if cwd != nil || branch != nil {
						return Record{Kind: KindSessionMeta, Timestamp: ts, CWD: cwd, GitBranch: branch}, true
					}
					return Record{}, false
				}
				return Record{Kind: KindUserPrompt, Timestamp: ts, CWD: cwd, GitBranch: branch, Text: strPtr(text)}, true
			}
			if cwd != nil || branch != nil {
				return Record{Kind: KindSessionMeta, Timestamp: ts, CWD: cwd, GitBranch: branch}, true
			}
			return Record{}, false
		default:
			if cwd != nil || branch != nil {
				return Record{Kind: KindSessionMeta, Timestamp: ts, CWD: cwd, GitBranch: branch}, true
			}
			return Record{}, false
		}

	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		stopReason, _ := msg["stop_reason"].(string)
		var stopPtr *string
		if stopReason != "" {
			stopPtr = &stopReason
		}
		if hasBlockType(content, "tool_use") {
			return Record{Kind: KindToolUse, Timestamp: ts, StopReason: stopPtr}, true
		}
		if text, ok := textFromBlocks(content, "text", "text"); ok {
			return Record{Kind: KindAssistantText, Timestamp: ts, StopReason: stopPtr, Text: strPtr(text)}, true
		}
		// thinking-only records etc: recognized, but carry nothing tier 1
		// needs beyond marking that the assistant turn is in progress.
		return Record{Kind: KindAssistantText, Timestamp: ts, StopReason: stopPtr}, true

	default:
		return Record{}, false
	}
}

func claudeCWDBranch(raw map[string]any) (*string, *string) {
	var cwd, branch *string
	if s, ok := raw["cwd"].(string); ok && s != "" {
		cwd = &s
	}
	if s, ok := raw["gitBranch"].(string); ok && s != "" {
		branch = &s
	}
	return cwd, branch
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// numberField reads a JSON number field that decoded as float64 (the
// default for encoding/json into map[string]any) and returns it as int64.
func numberField(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}
