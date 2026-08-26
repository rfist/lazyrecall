package transcript

// piVocab maps pi's transcript record shape. Record types seen in the
// wild: "session" (cwd, once near the start), "model_change",
// "thinking_level_change", "message" (message.role in "user", "assistant",
// "toolResult"). pi keeps no source index and no topic/compaction concept
// at all (design.md: "pi... no index at all... must extract prompts from
// transcripts"; proposal context: only Claude and omp are known to record
// compaction).
type piVocab struct{}

func (piVocab) Classify(raw map[string]any) (Record, bool) {
	typ, _ := raw["type"].(string)
	ts := parseTime(asString(raw["timestamp"]))

	switch typ {
	case "session":
		cwd, _ := raw["cwd"].(string)
		if cwd == "" {
			return Record{}, false
		}
		rec := Record{Kind: KindSessionMeta, Timestamp: ts, CWD: &cwd}
		// The session record's own "id" is the identifier pi itself uses
		// for `pi --session` (change fix-resume-session-identity, design.md
		// decision 1) - distinct from the transcript file's name, which pi
		// prefixes with a timestamp LazyRecall must never pass to pi. Confirmed
		// against a real transcript on this machine (structure only, never
		// copied into this repository - see devdocs/fyi.md):
		//   file : 2026-07-23T17-33-01-650Z_019f9009-af52-781d-a202-5c5927ec2c4c.jsonl
		//   id   :                          019f9009-af52-781d-a202-5c5927ec2c4c
		if id, ok := raw["id"].(string); ok && id != "" {
			rec.SourceID = &id
		}
		return rec, true

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
