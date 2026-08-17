package plugin

import "testing"

// Regression test for a real live-validation finding (agent-ai-app,
// 2026-08-06): qwen2.5:14b-instruct's "CallCheck" marker tic leaked twice in
// a row -- once as extractPseudoToolCalls' tolerated "cosmetic residue"
// ahead of a tool call, once again with no tool call attached on the next
// round -- and streaming.go sent both as separate chunks with no separator,
// rendering in the chat as "CallCheckCallCheckThere are no currently firing
// or pending alerts right now. Everything seems to be stable."
func TestSanitizeAssistantChatOutput_StripsLeakedModelMarkerTokens(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "doubled CallCheck ahead of real answer",
			raw:  "CallCheckCallCheckThere are no currently firing or pending alerts right now. Everything seems to be stable.",
			want: "There are no currently firing or pending alerts right now. Everything seems to be stable.",
		},
		{
			name: "single CallCheck prefix",
			raw:  "CallCheck\nLet me look into that.",
			want: "Let me look into that.",
		},
		{
			name: "other known marker tokens",
			raw:  "_icall_ _ical_ Here's the fallback message.",
			want: "Here's the fallback message.",
		},
		{
			name: "clean content untouched",
			raw:  "There are no currently firing or pending alerts right now.",
			want: "There are no currently firing or pending alerts right now.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeAssistantChatOutput(c.raw)
			if got != c.want {
				t.Fatalf("sanitizeAssistantChatOutput(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}
