package transcript

import "github.com/rfist/lazyrecall/internal/session"

// ClassifyEndState derives a session's end state from the tail of its
// transcript (task 4.5; spec session-review, "Session end state
// classification"). tail is the bounded trailing window Scan produced -
// the true tail of the file, since Scan's delta always runs to EOF.
//
// The rules, applied to the last meaningful (non-tool-result) record:
//   - last record is a completed assistant text turn (stop reason
//     "end_turn"/"stop", or simply text with no trailing tool_use) ->
//     completed
//   - last record is a user prompt with nothing after it -> dangling
//   - last record is a tool use with no following tool result -> interrupted
//   - no usable tail at all -> unknown
//
// inactiveSince reports whether the session's last activity is older than
// the configured inactivity threshold; when the tail carries no other
// terminal marker and this is true, the session is abandoned rather than
// left unknown.
func ClassifyEndState(tail []Record, inactive bool) session.EndState {
	if len(tail) == 0 {
		return session.EndStateUnknown
	}

	last := tail[len(tail)-1]
	switch last.Kind {
	case KindUserPrompt:
		return session.EndStateDangling

	case KindToolUse:
		return session.EndStateInterrupted

	case KindAssistantText:
		if last.StopReason != nil {
			switch *last.StopReason {
			case "end_turn", "stop":
				return session.EndStateCompleted
			default:
				// e.g. a "tool_use"-flagged stop reason on a
				// thinking-only fragment with no captured tool_use block:
				// the turn was still in progress when the transcript ends.
				return session.EndStateInterrupted
			}
		}
		return session.EndStateCompleted

	case KindToolResult:
		// A tool result was the last thing recorded: the agent turn that
		// issued it hasn't produced a follow-up yet. Look one step further
		// back to see whether that pattern itself looks finished; lacking
		// enough information to say more, this is exactly what "unknown"
		// exists for unless we can also observe staleness.
		if inactive {
			return session.EndStateAbandoned
		}
		return session.EndStateUnknown

	default:
		if inactive {
			return session.EndStateAbandoned
		}
		return session.EndStateUnknown
	}
}

// ExtractWorkingDirectory returns the authoritative cwd/branch pair found
// in scanned transcript content. It never falls back to decoding a
// source's directory-encoding scheme (task 4.4; spec session-index,
// "Authoritative working directory") - if the transcript never records a
// cwd, the caller reports it absent.
func ExtractWorkingDirectory(res Result) (cwd, branch *string) {
	return res.CWD, res.GitBranch
}

// ToSessionCompaction converts accumulated CompactionInfo events into the
// session model's Compaction property, kept independent of end state (task
// 4.6; spec session-review, "Compaction is reported independently of end
// state"). Returns nil only when the caller should represent "this source
// has no concept of compaction" - callers that know their source supports
// compaction (Claude, omp) always pass a non-nil (possibly empty) slice.
func ToSessionCompaction(events []CompactionInfo, sourceSupportsCompaction bool) *session.Compaction {
	if !sourceSupportsCompaction {
		return nil
	}
	c := &session.Compaction{Count: len(events)}
	for _, e := range events {
		c.Events = append(c.Events, session.CompactionEvent{
			Automatic:     e.Automatic,
			DroppedTokens: e.DroppedTokens,
		})
	}
	return c
}
