package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lazyrecall/internal/search"
)

func TestWriteItemsJSONUsesSnakeCaseAndOmitsAbsentFields(t *testing.T) {
	cwd := "/work/repo"
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := []search.Item{
		{SessionID: "claude:p:1", Source: "claude", CWD: &cwd, LastActivityAt: &t0, EndState: "completed", Resumable: true},
	}
	var buf bytes.Buffer
	if err := WriteItemsJSON(&buf, items); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{`"session_id"`, `"cwd"`, `"last_activity_at"`, `"end_state"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %s in output, got:\n%s", want, out)
		}
	}
	// A Go-default field name would leak through if ToJSONItems/WriteItemsJSON
	// stopped converting search.Item and started encoding it directly - this
	// is the exact bug caught during manual end-to-end testing.
	for _, mustNotAppear := range []string{`"SessionID"`, `"CWD"`, `"LastActivityAt"`} {
		if strings.Contains(out, mustNotAppear) {
			t.Errorf("found raw Go field name %s in JSON output - search.Item leaked unconverted:\n%s", mustNotAppear, out)
		}
	}
	// Absent fields (Topic, GitBranch, ...) must be omitted, not present as null.
	for _, mustNotAppear := range []string{`"topic"`, `"git_branch"`, `"tags"`} {
		if strings.Contains(out, mustNotAppear) {
			t.Errorf("expected absent field %s to be omitted, got:\n%s", mustNotAppear, out)
		}
	}

	var decoded []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(decoded) != 1 || decoded[0]["session_id"] != "claude:p:1" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestRenderRowOmitsAbsentTopic(t *testing.T) {
	it := search.Item{Source: "pi", EndState: "unknown"}
	line := RenderRow(it, DefaultRenderOptions)
	if strings.Contains(line, "<nil>") {
		t.Errorf("absent fields must never render as <nil>: %q", line)
	}
}

func TestWriteItemsJSONIncludesOriginAndOmitsEmpty(t *testing.T) {
	items := []search.Item{
		{SessionID: "claude:p:1", EndState: "completed", Origin: "interactive"},
		{SessionID: "claude:p:2", EndState: "completed", Origin: "unknown"},
		{SessionID: "claude:p:3", EndState: "completed"}, // zero value Origin
	}
	var buf bytes.Buffer
	if err := WriteItemsJSON(&buf, items); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"origin": "interactive"`) {
		t.Errorf("expected origin interactive in output, got:\n%s", out)
	}
	if !strings.Contains(out, `"origin": "unknown"`) {
		t.Errorf("expected origin unknown in output, got:\n%s", out)
	}
	// The zero value ("") must be omitted by the omitempty tag, not emitted
	// as an empty string.
	if strings.Contains(out, `"origin": ""`) {
		t.Errorf("expected an empty origin to be omitted, got:\n%s", out)
	}
}

func TestWriteItemsHumanShowsEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	WriteItemsHuman(&buf, nil, "nothing here", DefaultRenderOptions)
	if strings.TrimSpace(buf.String()) != "nothing here" {
		t.Errorf("got %q", buf.String())
	}
}
