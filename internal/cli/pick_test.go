package cli

import (
	"bytes"
	"strings"
	"testing"

	"lazyrecall/internal/search"
)

func strp(s string) *string { return &s }

func sampleItems() []search.Item {
	return []search.Item{
		{SessionID: "claude:p:1", Source: "claude", CWD: strp("/work/repo"), Topic: strp("Fix flaky test")},
		{SessionID: "pi:p:1", Source: "pi", CWD: strp("/work/other"), Topic: strp("Refactor loader")},
	}
}

func TestPickNumberedList(t *testing.T) {
	in := strings.NewReader("2\n")
	var out bytes.Buffer
	item, ok, err := Pick(sampleItems(), in, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || item.SessionID != "pi:p:1" {
		t.Fatalf("got item=%+v ok=%v", item, ok)
	}
	if !strings.Contains(out.String(), "Fix flaky test") {
		t.Errorf("expected the listing to be printed, got %q", out.String())
	}
}

func TestPickNumberedListBlankCancels(t *testing.T) {
	in := strings.NewReader("\n")
	var out bytes.Buffer
	_, ok, err := Pick(sampleItems(), in, &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected blank input to cancel")
	}
}

func TestPickNoItems(t *testing.T) {
	in := strings.NewReader("1\n")
	var out bytes.Buffer
	_, ok, err := Pick(nil, in, &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no items to report ok=false")
	}
}

func TestPickInvalidChoiceIsAnError(t *testing.T) {
	in := strings.NewReader("99\n")
	var out bytes.Buffer
	_, ok, err := Pick(sampleItems(), in, &out)
	if err == nil {
		t.Fatal("expected an out-of-range choice to be an error")
	}
	if ok {
		t.Fatal("expected ok=false on error")
	}
}
