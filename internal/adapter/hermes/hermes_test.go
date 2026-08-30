package hermes

import (
	"testing"

	"github.com/rfist/lazyrecall/internal/session"
)

func strp(s string) *string { return &s }
func i64p(i int64) *int64   { return &i }

func TestClassifyEndState(t *testing.T) {
	cases := []struct {
		name string
		row  sessionRow
		want session.EndState
	}{
		{"no messages at all", sessionRow{}, session.EndStateUnknown},
		{"last message from user", sessionRow{LastMsgRole: strp("user")}, session.EndStateDangling},
		{"assistant stopped cleanly", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("stop")}, session.EndStateCompleted},
		{"assistant mid tool call", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("tool_calls"), LastMsgHasToolCalls: i64p(1)}, session.EndStateInterrupted},
		{"assistant awaiting verification", sessionRow{LastMsgRole: strp("assistant"), LastMsgFinish: strp("verification_required")}, session.EndStateInterrupted},
		{"assistant with no finish reason recorded", sessionRow{LastMsgRole: strp("assistant")}, session.EndStateUnknown},
		{"last message a bare tool result", sessionRow{LastMsgRole: strp("tool")}, session.EndStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEndState(tc.row)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
