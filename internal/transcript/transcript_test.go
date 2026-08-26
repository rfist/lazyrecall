package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lazyrecall/internal/session"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// All content below is synthetic, hand-written test data - never real
// session content.

func TestClaudeVocabCompletedSession(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"fix the flaky test"},"cwd":"/work/repo","gitBranch":"main","timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed it."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:03Z"}
{"type":"last-prompt","lastPrompt":"fix the flaky test"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, ok := VocabFor("claude")
	if !ok {
		t.Fatal("expected claude vocab")
	}
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.CWD == nil || *res.CWD != "/work/repo" {
		t.Errorf("cwd = %v", res.CWD)
	}
	if res.GitBranch == nil || *res.GitBranch != "main" {
		t.Errorf("branch = %v", res.GitBranch)
	}
	state := ClassifyEndState(res.Tail, false)
	if state != session.EndStateCompleted {
		t.Errorf("end state = %v, want completed", state)
	}
	if res.LastPrompt == nil {
		t.Fatal("expected a last prompt")
	}
}

func TestClaudeVocabDanglingSession(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"are you there?"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifyEndState(res.Tail, false); got != session.EndStateDangling {
		t.Errorf("got %v, want dangling", got)
	}
}

func TestClaudeVocabInterruptedSession(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"delete the temp dir"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:01Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifyEndState(res.Tail, false); got != session.EndStateInterrupted {
		t.Errorf("got %v, want interrupted", got)
	}
}

func TestClaudeCompactionDetected(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"start"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":100000,"postTokens":9000,"cumulativeDroppedTokens":91000},"timestamp":"2026-01-01T00:05:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:06:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Compactions) != 1 {
		t.Fatalf("got %d compactions, want 1", len(res.Compactions))
	}
	c := res.Compactions[0]
	if c.Automatic == nil || !*c.Automatic {
		t.Errorf("expected automatic=true, got %v", c.Automatic)
	}
	if c.DroppedTokens == nil || *c.DroppedTokens != 91000 {
		t.Errorf("dropped tokens = %v", c.DroppedTokens)
	}

	comp := ToSessionCompaction(res.Compactions, true)
	if comp.Count != 1 {
		t.Errorf("compaction count = %d", comp.Count)
	}

	// Compacted + still dangling: compaction must not mask the unfinished
	// end state (spec session-review, "Compacted session that was also
	// left unfinished").
	danglingContent := content + `{"type":"user","message":{"role":"user","content":"one more thing"},"cwd":"/work/repo","timestamp":"2026-01-01T00:07:00Z"}
`
	p2 := writeFile(t, dir, "s2.jsonl", danglingContent)
	res2, err := Scan(p2, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifyEndState(res2.Tail, false); got != session.EndStateDangling {
		t.Errorf("got %v, want dangling despite compaction", got)
	}
	if len(res2.Compactions) != 1 {
		t.Errorf("compaction should still be reported: got %d", len(res2.Compactions))
	}
}

func TestPiVocabNoCompactionConcept(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"stop"},"timestamp":"2026-01-01T00:00:02Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, ok := VocabFor("pi")
	if !ok {
		t.Fatal("expected pi vocab")
	}
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.CWD == nil || *res.CWD != "/work/repo" {
		t.Errorf("cwd = %v", res.CWD)
	}
	// pi has no compaction concept: caller passes sourceSupportsCompaction=false.
	comp := ToSessionCompaction(res.Compactions, false)
	if comp != nil {
		t.Errorf("expected nil (absent) compaction for pi, got %+v", comp)
	}
	if got := ClassifyEndState(res.Tail, false); got != session.EndStateCompleted {
		t.Errorf("got %v, want completed", got)
	}
}

// TestPiVocabSourceIDFromSessionRecord covers change fix-resume-session-identity,
// design.md decision 1: the identifier passed to pi must be the "id" the
// session record itself carries, not anything derived from the transcript's
// file name (which LazyRecall's caller never even passes into Scan - a
// deliberate proof that the vocab cannot be reading it).
func TestPiVocabSourceIDFromSessionRecord(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","id":"019f9009-af52-781d-a202-5c5927ec2c4c","cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-01-01T00:00:01Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, ok := VocabFor("pi")
	if !ok {
		t.Fatal("expected pi vocab")
	}
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceID == nil || *res.SourceID != "019f9009-af52-781d-a202-5c5927ec2c4c" {
		t.Errorf("SourceID = %v, want the record's own id, not anything derived from a file name", res.SourceID)
	}
}

// TestPiVocabNoSourceIDWhenRecordOmitsIt covers task 1.3: a session record
// missing "id" leaves SourceID absent rather than inventing one, so the
// caller's fallback-to-file-derived-value path is reachable and never
// silently masked.
func TestPiVocabNoSourceIDWhenRecordOmitsIt(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("pi")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceID != nil {
		t.Errorf("SourceID = %v, want nil when the record carries none", res.SourceID)
	}
}

// TestOmpVocabSourceIDFromSessionRecord covers task 3.2: omp names its
// transcript files the same "<timestamp>_<uuid>.jsonl" way pi does, so its
// session record's own "id" - not the file name - must be what LazyRecall reads
// as omp's identifier too, for the same reason as pi.
func TestOmpVocabSourceIDFromSessionRecord(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","id":"019f5ba3-23ea-7000-b6f8-876258ebb632","cwd":"/work/repo","title":"t","timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, ok := VocabFor("omp")
	if !ok {
		t.Fatal("expected omp vocab")
	}
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceID == nil || *res.SourceID != "019f5ba3-23ea-7000-b6f8-876258ebb632" {
		t.Errorf("SourceID = %v, want the record's own id", res.SourceID)
	}
}

func TestOmpVocabTopicAndCompaction(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","cwd":"/work/repo","title":"initial guess","timestamp":"2026-01-01T00:00:00Z"}
{"type":"title_change","title":"Refactor the widget loader","timestamp":"2026-01-01T00:00:01Z"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"refactor please"}]},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"compaction","tokensBefore":50000,"fromExtension":false,"timestamp":"2026-01-01T00:00:03Z"}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"},"timestamp":"2026-01-01T00:00:04Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, ok := VocabFor("omp")
	if !ok {
		t.Fatal("expected omp vocab")
	}
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.Topic == nil || *res.Topic != "Refactor the widget loader" {
		t.Errorf("topic = %v, want the title_change value to supersede the initial title", res.Topic)
	}
	if len(res.Compactions) != 1 {
		t.Fatalf("got %d compactions", len(res.Compactions))
	}
	if res.Compactions[0].Automatic == nil || !*res.Compactions[0].Automatic {
		t.Errorf("fromExtension=false should mean automatic=true, got %v", res.Compactions[0].Automatic)
	}
}

func TestIncrementalScanContinuesFromOffset(t *testing.T) {
	dir := t.TempDir()
	first := `{"type":"session","cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"part one"}]},"timestamp":"2026-01-01T00:00:01Z"}
`
	p := writeFile(t, dir, "s.jsonl", first)
	vocab, _ := VocabFor("pi")
	res1, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res1.RecordCount != 2 {
		t.Fatalf("first scan record count = %d, want 2", res1.RecordCount)
	}

	// Append more content, as a live agent would.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	second := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"part two"}],"stopReason":"stop"},"timestamp":"2026-01-01T00:00:02Z"}
`
	if _, err := f.WriteString(second); err != nil {
		t.Fatal(err)
	}
	f.Close()

	res2, err := Scan(p, res1.EndOffset, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res2.RecordCount != 1 {
		t.Fatalf("incremental scan should read only the new record, got %d", res2.RecordCount)
	}
	if got := ClassifyEndState(res2.Tail, false); got != session.EndStateCompleted {
		t.Errorf("got %v, want completed", got)
	}
}

// TestAbandonedRequiresBothNoTerminalMarkerAndInactivity covers spec
// session-review, "Session stopped without any terminal marker": abandoned
// only applies when there's no clearer signal AND the session has been
// inactive past the threshold; the same ambiguous tail with recent
// activity must stay unknown rather than guessing abandoned.
func TestAbandonedRequiresBothNoTerminalMarkerAndInactivity(t *testing.T) {
	dir := t.TempDir()
	// A tool result is the last record: not clearly finished, not clearly
	// mid-operation - ambiguous on its own.
	content := `{"type":"user","message":{"role":"user","content":"do a thing"},"cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:01Z"}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"done"}]},"timestamp":"2026-01-01T00:00:02Z"}` + "\n"
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}

	if got := ClassifyEndState(res.Tail, false); got != session.EndStateUnknown {
		t.Errorf("recent + ambiguous tail: got %v, want unknown", got)
	}
	if got := ClassifyEndState(res.Tail, true); got != session.EndStateAbandoned {
		t.Errorf("stale + ambiguous tail: got %v, want abandoned", got)
	}
}

// TestUnknownWhenNoConversationalTailAtAll covers spec session-review,
// "Source data insufficient to classify": a session with nothing but
// metadata records (no user prompt, no assistant turn) gives
// ClassifyEndState an empty tail, and unknown is the only honest answer.
func TestUnknownWhenNoConversationalTailAtAll(t *testing.T) {
	if got := ClassifyEndState(nil, false); got != session.EndStateUnknown {
		t.Errorf("got %v, want unknown", got)
	}
	if got := ClassifyEndState(nil, true); got != session.EndStateUnknown {
		t.Errorf("an empty tail must stay unknown even when stale - there's nothing to call abandoned: got %v", got)
	}
}

// TestPhraseInAgentReplyOnlyIsNotIndexed covers spec session-search,
// "Phrase appears only in agent output": decision 4 scopes the search
// index to what the user typed - Scan's collected Prompts must never
// include assistant text, tool output, or thinking content.
func TestPhraseInAgentReplyOnlyIsNotIndexed(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"hello"},"cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the secret phrase is xylophone-cascade"}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:01Z"}` + "\n"
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range res.Prompts {
		if strings.Contains(pr.Text, "xylophone-cascade") {
			t.Fatalf("agent-only text leaked into the tier-2 prompt index: %q", pr.Text)
		}
	}
	if len(res.Prompts) != 1 || res.Prompts[0].Text != "hello" {
		t.Fatalf("expected only the user's own prompt to be collected, got %+v", res.Prompts)
	}
}

// TestNoActivitySinceLastScanReadsNothing covers spec session-index,
// "Refresh after no activity": scanning from an offset that already sits
// at end-of-file must yield zero records, not silently rescan.
func TestNoActivitySinceLastScanReadsNothing(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","cwd":"/x","timestamp":"2026-01-01T00:00:00Z"}` + "\n"
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("pi")
	first, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Scan(p, first.EndOffset, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if second.RecordCount != 0 {
		t.Errorf("expected zero records read for an unchanged file, got %d", second.RecordCount)
	}
}

// TestSessionNeverRecordsWorkingDirectory covers spec session-index,
// "Session records no working directory": a transcript with real
// conversational content but no record ever carrying cwd must leave CWD
// absent - never guessed - while still producing a usable scan (the
// session still gets indexed by its caller).
func TestSessionNeverRecordsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"no cwd anywhere in this file"}]},"timestamp":"2026-01-01T00:00:00Z"}` + "\n"
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("pi")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.CWD != nil {
		t.Errorf("expected cwd to remain absent, got %v", *res.CWD)
	}
	if res.RecordCount == 0 {
		t.Error("the session's content should still have been scanned")
	}
}

func TestUnparseableRecordsAreSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"session","cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
not even json
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"still works"}]},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"something-nobody-has-heard-of","weird":true}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"yep"}],"stopReason":"stop"},"timestamp":"2026-01-01T00:00:02Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("pi")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatalf("Scan must not abort on malformed/unrecognized records: %v", err)
	}
	if got := ClassifyEndState(res.Tail, false); got != session.EndStateCompleted {
		t.Errorf("got %v, want completed - rest of session should still be indexed", got)
	}
}

// The following cover change add-readable-session-listing, tasks 3.1-3.3:
// Claude Code injects local slash-command invocations and their output as
// "user"-role transcript records (structure confirmed against real
// transcripts on this machine - never copied into this repo, see
// devdocs/fyi.md); none of that is something the user typed, and it must
// never reach the topic, the last-prompt field, or the search index.

func TestLocalCommandInvocationIsNotIndexedAsUserPrompt(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<command-name>/model</command-name><command-message>Set the model</command-message>"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("expected no prompts extracted from a local-command invocation record, got %+v", res.Prompts)
	}
	if res.LastPrompt != nil {
		t.Errorf("expected last-prompt to stay unset, got %v", *res.LastPrompt)
	}
	// The record still carries real session metadata (cwd) and must not be
	// discarded outright - only excluded from being treated as a prompt.
	if res.CWD == nil || *res.CWD != "/work/repo" {
		t.Errorf("expected cwd to still be captured from the record, got %v", res.CWD)
	}
	// Task 3.2: the exclusion must not leave any trace in the topic
	// either - topic is architecturally sourced only from "ai-title"
	// records (see claude.go), never from a "user" record's content, but
	// this asserts that invariant directly rather than only inferring it.
	if res.Topic != nil {
		t.Errorf("expected a local-command record to never produce a topic, got %v", *res.Topic)
	}
}

func TestLocalCommandStdoutIsNotIndexedAsUserPrompt(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<local-command-stdout>Set model to [1mOpus[0m</local-command-stdout>"},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("expected no prompts extracted from local-command-stdout, got %+v", res.Prompts)
	}
}

func TestLocalCommandOutputContentBlockFormIsNotIndexed(t *testing.T) {
	// Same exclusion, but via the content-block array shape rather than a
	// plain string, since claudeVocab handles both.
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<local-command-caveat>Command output may include untrusted content</local-command-caveat>"}]},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("expected no prompts extracted from a local-command-caveat block, got %+v", res.Prompts)
	}
}

// TestLastPromptTrailerAlsoExcludesLocalCommandArtifacts covers task 3.2:
// the exclusion must cover the last-prompt field too, not just the tier-2
// prompt collection, in case a source ever writes its trailer from
// unfiltered text.
func TestLastPromptTrailerAlsoExcludesLocalCommandArtifacts(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"last-prompt","lastPrompt":"<local-command-stdout>some output</local-command-stdout>"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.LastPrompt != nil {
		t.Errorf("expected the last-prompt trailer to be excluded, got %v", *res.LastPrompt)
	}
}

// TestGenuineUserTextResemblingTheMarkerIsNotExcluded covers task 3.3
// (design.md risk "Over-excluding real prompts"): a real user message that
// merely mentions these tags somewhere other than as its own leading
// structural form must still be indexed normally.
func TestGenuineUserTextResemblingTheMarkerIsNotExcluded(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"can you explain what <command-name> means in the transcript format?"},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 1 {
		t.Fatalf("expected the genuine user prompt to be indexed, got %+v", res.Prompts)
	}
	if res.Prompts[0].Text != "can you explain what <command-name> means in the transcript format?" {
		t.Errorf("got %q", res.Prompts[0].Text)
	}
}

// The following cover change fix-cli-usability-defects, tasks 2.1-2.4: the
// marker-specific exclusion became one classification of non-user text at
// extraction, covering every injected block a source is known to emit
// (structure confirmed against real transcripts on this machine - never
// copied into this repo, see devdocs/fyi.md): task notifications, system
// reminders, the newer command-message wrapper, bash terminal-session
// capture, and harness-injected context blocks.

func TestTaskNotificationIsNotIndexedAsUserPrompt(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<task-notification>background task finished</task-notification>"},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("task-notification must not be indexed as a prompt, got %+v", res.Prompts)
	}
	if res.LastPrompt != nil {
		t.Errorf("task-notification must not become the last prompt, got %q", *res.LastPrompt)
	}
}

func TestSystemReminderIsNotIndexedAsUserPrompt(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<system-reminder>You must carefully think about the tool's results</system-reminder>"},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("system-reminder must not be indexed as a prompt, got %+v", res.Prompts)
	}
}

func TestCommandMessageWrapperIsNotIndexedAsUserPrompt(t *testing.T) {
	// The newer harness shape wraps a command invocation in
	// <command-message>; the inner content begins with <command-name>.
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<command-message>\n<command-name>/opsx:apply</command-name>..."},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("command-message wrapper must not be indexed as a prompt, got %+v", res.Prompts)
	}
}

func TestBashCaptureBlocksAreNotIndexedAsUserPrompt(t *testing.T) {
	for _, tag := range []string{"bash-input", "bash-stdout", "bash-stderr"} {
		dir := t.TempDir()
		content := `{"type":"user","message":{"role":"user","content":"<` + tag + `>captured terminal content</` + tag + `>"},"timestamp":"2026-01-01T00:00:00Z"}
`
		p := writeFile(t, dir, "s.jsonl", content)
		vocab, _ := VocabFor("claude")
		res, err := Scan(p, 0, vocab)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Prompts) != 0 {
			t.Errorf("<%s> must not be indexed as a prompt, got %+v", tag, res.Prompts)
		}
	}
}

func TestContextBlockIsNotIndexedAsUserPrompt(t *testing.T) {
	// The harness-injected context block carries an attribute, so the
	// structural match must accept "<tag attr=...>" as well as "<tag>".
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<context ref=\"file:///Users/x/out.txt\">file contents</context>"},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 0 {
		t.Errorf("context block must not be indexed as a prompt, got %+v", res.Prompts)
	}
}

// TestMostRecentEntryInjectedLastPromptIsPriorUserText covers the delta
// spec scenario "Session whose most recent entry is injected": when a
// session's most recent entry is a block injected by the agent, the
// session's last prompt is the most recent text the user actually wrote.
func TestMostRecentEntryInjectedLastPromptIsPriorUserText(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"fix the flaky retry"},"timestamp":"2026-01-01T00:00:00Z"}
` +
		`{"type":"user","message":{"role":"user","content":"<task-notification>deployment finished</task-notification>"},"timestamp":"2026-01-01T00:00:01Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.LastPrompt == nil || *res.LastPrompt != "fix the flaky retry" {
		t.Errorf("last prompt = %v, want the most recent user-written text", res.LastPrompt)
	}
	if len(res.Prompts) != 1 || res.Prompts[0].Text != "fix the flaky retry" {
		t.Errorf("prompts = %+v, want only the user-written prompt", res.Prompts)
	}
}

// TestUserTextResemblingInjectedBlockStaysSearchable covers the delta spec
// scenario "User text resembling an injected block": a user genuinely
// writing text that resembles an injected block is treated as a prompt and
// remains searchable. Two cases: a leading user-typed tag that is NOT a
// known injected marker ("<leader>", confirmed user-typed in real data),
// and a known marker that appears mid-message rather than as the leading
// structural form.
func TestUserTextResemblingInjectedBlockStaysSearchable(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<leader> what does the leader key do"},"timestamp":"2026-01-01T00:00:00Z"}
` +
		`{"type":"user","message":{"role":"user","content":"why did I get a <task-notification> warning?"},"timestamp":"2026-01-01T00:00:01Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prompts) != 2 {
		t.Fatalf("expected both genuine user prompts to be indexed, got %+v", res.Prompts)
	}
	if res.Prompts[0].Text != "<leader> what does the leader key do" {
		t.Errorf("user-typed <leader> text was excluded: %q", res.Prompts[0].Text)
	}
	if res.Prompts[1].Text != "why did I get a <task-notification> warning?" {
		t.Errorf("mid-message marker mention was excluded: %q", res.Prompts[1].Text)
	}
}

// TestInjectedBlockNeverReachesPromptIndex covers the delta spec scenario
// "Searching for text that only appears in an injected block" at the
// extraction mechanism: the tier-2 prompt collection must never contain a
// phrase that appears only inside an injected block, which is what keeps
// such a phrase out of the FTS search index (the search layer reads only
// prompt_fts).
func TestInjectedBlockNeverReachesPromptIndex(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"<system-reminder>remember the passphrase aurora-sentinel-77</system-reminder>"},"timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range res.Prompts {
		if strings.Contains(pr.Text, "aurora-sentinel-77") {
			t.Fatalf("injected-block-only text leaked into the prompt index: %q", pr.Text)
		}
	}
	if len(res.Prompts) != 0 {
		t.Errorf("expected zero prompts from an injected-block-only transcript, got %+v", res.Prompts)
	}
}

// TestClaudeVocabCustomTitle covers the user-chosen session name (change
// show-session-names): Claude Code writes a "custom-title" record when a
// session is renamed, repeats it with the current name as the session goes
// on, and carries no timestamp on it. The last one seen wins, so a second
// rename supersedes the first, and it never disturbs the agent-written
// topic that "ai-title" records carry.
func TestClaudeVocabCustomTitle(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"ai-title","aiTitle":"Flaky retry loop investigation","timestamp":"2026-01-01T00:00:00Z"}
{"type":"custom-title","customTitle":"retry-loop","sessionId":"s1"}
{"type":"custom-title","customTitle":"","sessionId":"s1"}
{"type":"custom-title","customTitle":"retry-loop-v2","sessionId":"s1"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, ok := VocabFor("claude")
	if !ok {
		t.Fatal("expected claude vocab")
	}
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.CustomTitle == nil || *res.CustomTitle != "retry-loop-v2" {
		t.Errorf("CustomTitle = %v, want the later rename to supersede the earlier one (and an empty name to be ignored)", res.CustomTitle)
	}
	if res.Topic == nil || *res.Topic != "Flaky retry loop investigation" {
		t.Errorf("topic = %v, want the ai-title value: a rename must not disturb the agent-written topic", res.Topic)
	}
}

// TestClaudeVocabNoCustomTitle: a transcript with no rename in it leaves
// CustomTitle absent rather than falling back to the topic - the two are
// separate fields precisely so "never named" stays distinguishable.
func TestClaudeVocabNoCustomTitle(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"ai-title","aiTitle":"Some topic","timestamp":"2026-01-01T00:00:00Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)
	vocab, _ := VocabFor("claude")
	res, err := Scan(p, 0, vocab)
	if err != nil {
		t.Fatal(err)
	}
	if res.CustomTitle != nil {
		t.Errorf("CustomTitle = %v, want nil when the session was never renamed", *res.CustomTitle)
	}
}
