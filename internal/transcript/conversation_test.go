package transcript

import (
	"fmt"
	"strings"
	"testing"
)

// All content below is synthetic, hand-written test data - never real
// session content.

func claudeVocabOrFail(t *testing.T) Vocab {
	t.Helper()
	v, ok := VocabFor("claude")
	if !ok {
		t.Fatal("expected claude vocab")
	}
	return v
}

func TestConversationKeepsTurnsInOrder(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"fix the flaky test"},"cwd":"/work/repo","timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Looking now."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}],"stop_reason":"tool_use"},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]},"timestamp":"2026-01-01T00:00:03Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed it."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:04Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)

	turns, dropped, err := Conversation(p, claudeVocabOrFail(t), DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	// The tool result is deliberately not a turn: it carries no text in
	// any vocabulary, so a line for it would say only that something came
	// back.
	want := []Kind{KindUserPrompt, KindAssistantText, KindToolUse, KindAssistantText}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i, k := range want {
		if turns[i].Kind != k {
			t.Errorf("turn %d kind = %v, want %v", i, turns[i].Kind, k)
		}
	}
	if turns[0].Text != "fix the flaky test" {
		t.Errorf("first turn text = %q", turns[0].Text)
	}
	if got := strings.Join(turns[2].Tool, ","); got != "Bash,Read" {
		t.Errorf("tool names = %q, want Bash,Read", got)
	}
	if turns[3].At == nil {
		t.Error("expected the last turn to carry its timestamp")
	}
}

// A thinking-only assistant record classifies as KindAssistantText with no
// text at all. It must not become a blank turn: the pane would show a
// speaker label with nothing under it.
func TestConversationSkipsTextlessTurns(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"..."}],"stop_reason":"end_turn"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"user","message":{"role":"user","content":"   "},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"user","message":{"role":"user","content":"a real prompt"},"timestamp":"2026-01-01T00:00:02Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)

	turns, _, err := Conversation(p, claudeVocabOrFail(t), DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want only the real prompt: %+v", len(turns), turns)
	}
	if turns[0].Text != "a real prompt" {
		t.Errorf("text = %q", turns[0].Text)
	}
}

// The budget keeps the *end* of the conversation, because "where did I
// leave off" is the question this view exists to answer.
func TestConversationKeepsTheTailAndReportsWhatItDropped(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	const total = 50
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"prompt %d"},"timestamp":"2026-01-01T00:00:00Z"}`+"\n", i)
	}
	p := writeFile(t, dir, "s.jsonl", b.String())

	turns, dropped, err := Conversation(p, claudeVocabOrFail(t), ConversationLimits{MaxTurns: 10, MaxTurnBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 10 {
		t.Fatalf("got %d turns, want 10", len(turns))
	}
	if dropped != total-10 {
		t.Errorf("dropped = %d, want %d", dropped, total-10)
	}
	if turns[0].Text != "prompt 40" {
		t.Errorf("first kept turn = %q, want prompt 40", turns[0].Text)
	}
	if turns[9].Text != "prompt 49" {
		t.Errorf("last kept turn = %q, want prompt 49", turns[9].Text)
	}
}

func TestConversationTruncatesAnOversizedTurn(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 500)
	content := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},"timestamp":"2026-01-01T00:00:00Z"}`+"\n", long)
	p := writeFile(t, dir, "s.jsonl", content)

	turns, _, err := Conversation(p, claudeVocabOrFail(t), ConversationLimits{MaxTurns: 10, MaxTurnBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if len(turns[0].Text) != 100 {
		t.Errorf("kept %d bytes, want 100", len(turns[0].Text))
	}
	if !turns[0].Truncated {
		t.Error("expected the turn to be marked truncated")
	}
}

// Truncation must not split a rune: a cut mid-sequence would put a
// replacement character on screen and mis-measure the line's width.
func TestConversationTruncationDoesNotSplitARune(t *testing.T) {
	dir := t.TempDir()
	// Three-byte runes, so a byte budget that is not a multiple of three
	// falls inside one.
	content := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`+"\n", strings.Repeat("あ", 20))
	p := writeFile(t, dir, "s.jsonl", content)

	turns, _, err := Conversation(p, claudeVocabOrFail(t), ConversationLimits{MaxTurns: 10, MaxTurnBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	if got := turns[0].Text; got != strings.Repeat("あ", 3) {
		t.Errorf("text = %q, want three whole runes", got)
	}
}

// A transcript is allowed to contain records this program has never seen,
// and a partial write can leave a line that is not JSON at all. Neither
// aborts the read (spec session-index, "Unrecognized or malformed record").
func TestConversationSkipsMalformedAndUnknownLines(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"first"},"timestamp":"2026-01-01T00:00:00Z"}
this is not json at all
{"type":"queue-operation","op":"whatever"}
{"type":"user","message":{"role":"user","content":"second"},"timestamp":"2026-01-01T00:00:01Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)

	turns, _, err := Conversation(p, claudeVocabOrFail(t), DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
	if turns[0].Text != "first" || turns[1].Text != "second" {
		t.Errorf("texts = %q, %q", turns[0].Text, turns[1].Text)
	}
}

func TestConversationReportsAMissingFile(t *testing.T) {
	_, _, err := Conversation(t.TempDir()+"/gone.jsonl", claudeVocabOrFail(t), DefaultConversationLimits)
	if err == nil {
		t.Fatal("expected an error for a transcript that is not there")
	}
}

// A compaction boundary is shown, because a reader who does not know the
// conversation was compacted cannot make sense of the jump in it.
func TestConversationShowsCompactionBoundaries(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"user","message":{"role":"user","content":"before"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":1000},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"user","message":{"role":"user","content":"after"},"timestamp":"2026-01-01T00:00:02Z"}
`
	p := writeFile(t, dir, "s.jsonl", content)

	turns, _, err := Conversation(p, claudeVocabOrFail(t), DefaultConversationLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3: %+v", len(turns), turns)
	}
	if turns[1].Kind != KindCompactionBoundary {
		t.Errorf("middle turn = %v, want a compaction boundary", turns[1].Kind)
	}
}
