package transcript

// ompVocab maps omp's transcript record shape. Record types seen in the
// wild: "session" (cwd + an initial title), "title_change" (a later,
// superseding title), "model_change", "thinking_level_change", "message"
// (message.role in "user", "assistant", "toolResult"), "compaction",
// "custom" / "custom_message" (tool/UI bookkeeping, ignored). omp's own
// history.db + history_fts is the tier 2 source of truth (task 5.3); this
// vocab exists for tier 1 (identity, topic, cwd, end state, compaction) and
// as the tier 2 fallback for sessions the history table doesn't cover.
type ompVocab struct{}

func (ompVocab) Classify(raw map[string]any) (Record, bool) {
	typ, _ := raw["type"].(string)
	ts := parseTime(asString(raw["timestamp"]))

	switch typ {
	case "session":
		// The session record carries both cwd and an initial title, but
		// Scan folds one Kind's worth of data per classified record. cwd is
		// the more time-critical fact for tier 1 (it never changes again),
		// so this record is surfaced as KindSessionMeta; the initial title
		// is picked up separately from the "title_change" record omp emits
		// immediately after session start. If no title_change ever
		// follows, Topic stays absent - correct, since it is never guessed.
		cwd, _ := raw["cwd"].(string)
		if cwd == "" {
			return Record{}, false
		}
		rec := Record{Kind: KindSessionMeta, Timestamp: ts, CWD: &cwd}
		// The session record's own "id" is omp's session identifier too
		// (change fix-resume-session-identity, design.md decision 1/task
		// 3.2). omp names its transcript files the same
		// "<timestamp>_<uuid>.jsonl" way pi does, and LazyRecall previously
		// passed that whole file-name stem to `omp --resume` here as well -
		// it happened to keep working only because an unmatched --resume
		// value makes omp silently start a brand-new session instead of
		// reporting an error, not because the stem was ever a value omp's
		// own identifier scheme recognised (confirmed live and recorded in
		// devdocs/fyi.md). omp's own history.db stores this same bare id in
		// its session_id column, corroborating that the id field - not the
		// file name - is omp's canonical identifier.
		if id, ok := raw["id"].(string); ok && id != "" {
			rec.SourceID = &id
		}
		return rec, true

	case "title", "title_change":
		title, _ := raw["title"].(string)
		if title == "" {
			return Record{}, false
		}
		return Record{Kind: KindTopic, Timestamp: ts, Text: &title}, true

	case "compaction":
		ci := &CompactionInfo{}
		if v, ok := numberField(raw, "tokensBefore"); ok {
			ci.PreTokens = &v
		}
		if b, ok := raw["fromExtension"].(bool); ok {
			auto := !b
			ci.Automatic = &auto
		}
		return Record{Kind: KindCompactionBoundary, Timestamp: ts, Compaction: ci}, true

	case "message":
		msg, _ := raw["message"].(map[string]any)
		role, _ := msg["role"].(string)
		content, _ := msg["content"].([]any)

		switch role {
		case "user":
			if text, ok := textFromBlocks(content, "text", "text"); ok && text != "" {
				return Record{Kind: KindUserPrompt, Timestamp: ts, Text: strPtr(text)}, true
			}
			return Record{}, false

		case "assistant":
			var stopPtr *string
			if sr, ok := msg["stopReason"].(string); ok && sr != "" {
				stopPtr = &sr
			}
			if hasBlockType(content, "toolCall") {
				return Record{Kind: KindToolUse, Timestamp: ts, StopReason: stopPtr}, true
			}
			if text, ok := textFromBlocks(content, "text", "text"); ok {
				return Record{Kind: KindAssistantText, Timestamp: ts, StopReason: stopPtr, Text: strPtr(text)}, true
			}
			return Record{Kind: KindAssistantText, Timestamp: ts, StopReason: stopPtr}, true

		case "toolResult":
			return Record{Kind: KindToolResult, Timestamp: ts}, true

		default:
			return Record{}, false
		}

	default:
		return Record{}, false
	}
}
