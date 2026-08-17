package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestMaxContextTokensForAgent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		agent      string
		configured int
		want       int
	}{
		{"generic", 120000, genericMaxContextTokens},
		{"", 120000, genericMaxContextTokens},
		{"agent-1", 120000, 120000},
		{"agent-2", 120000, 120000},
		{"generic", 50000, 50000}, // configured lower than the cap stays as configured
		{"generic", 0, genericMaxContextTokens},
	}
	for _, c := range cases {
		got := maxContextTokensForAgent(c.agent, c.configured, nil)
		if got != c.want {
			t.Errorf("maxContextTokensForAgent(%q, %d) = %d, want %d", c.agent, c.configured, got, c.want)
		}
	}
}

func TestMaxContextTokensForAgent_PerAgentOverride(t *testing.T) {
	t.Parallel()

	perAgent := map[string]int{"agent-1": 110000, "agent-2": 999999999}

	if got := maxContextTokensForAgent("agent-1", 120000, perAgent); got != 110000 {
		t.Errorf("expected per-agent override 110000, got %d", got)
	}
	if got := maxContextTokensForAgent("agent-2", 120000, perAgent); got != maxCustomAgentContextTokens {
		t.Errorf("expected override clamped to %d, got %d", maxCustomAgentContextTokens, got)
	}
	if got := maxContextTokensForAgent("agent-3", 120000, perAgent); got != 120000 {
		t.Errorf("expected agent with no override to fall back to configured, got %d", got)
	}
}

func TestMaxContextTokensForAgent_PerAgentOverrideBelowFloorIsClamped(t *testing.T) {
	t.Parallel()

	// The Agents page's slider never offers below genericMaxContextTokens,
	// but Settings.AgentContextTokens can be set directly (provisioning, the
	// Admin API) -- an absurdly small value must not starve the request of
	// the budget its own system prompt alone requires.
	perAgent := map[string]int{"agent-1": 500}

	if got := maxContextTokensForAgent("agent-1", 120000, perAgent); got != genericMaxContextTokens {
		t.Errorf("expected below-floor override clamped up to %d, got %d", genericMaxContextTokens, got)
	}
}

func newTestOpenAIClient(t *testing.T, handler http.HandlerFunc) *openai.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL = server.URL
	return openai.NewClientWithConfig(cfg)
}

// TestSummarizeMessages_PromptPreservesOriginalRequest reproduces a real live
// incident (2026-08-08): a request to dispatch two workers and summarize
// their findings triggered enough tool rounds to compact, and the resulting
// summary let the final answer drift into an unrelated generic Grafana
// question instead of ever addressing what the user actually asked for. The
// summarization prompt must explicitly call out preserving the user's
// original request/goal, not just "facts found so far" -- otherwise a
// summarizer reasonably treats the original question as redundant once its
// answer's constituent facts are listed, and drops it.
func TestSummarizeMessages_PromptPreservesOriginalRequest(t *testing.T) {
	t.Parallel()

	var systemPrompt string
	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			systemPrompt = req.Messages[0].Content
		}
		resp := openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "summary"},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	_, err := summarizeMessages(context.Background(), client, "test-model", []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "Dispatch a worker to check node CPU usage and another worker to check pod restarts, then summarize both."},
	})
	if err != nil {
		t.Fatalf("summarizeMessages failed: %v", err)
	}
	if !strings.Contains(systemPrompt, "original request") {
		t.Errorf("summarization prompt = %q, want it to explicitly instruct preserving the user's original request/goal, not just facts already found", systemPrompt)
	}
}

func TestCompactIfNeeded_NoOpBelowThreshold(t *testing.T) {
	t.Parallel()

	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call the LLM when below the compaction threshold")
	})

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{Role: openai.ChatMessageRoleAssistant, Content: "hi there"},
	}
	got := compactIfNeeded(context.Background(), client, "test-model", messages, 120000, nil)
	if len(got) != len(messages) {
		t.Errorf("expected no compaction, got %d messages, want %d", len(got), len(messages))
	}
}

func TestCompactIfNeeded_CompactsOverThreshold(t *testing.T) {
	t.Parallel()

	var calledWithSummaryPrompt bool
	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "Summarize the conversation") {
			calledWithSummaryPrompt = true
		}
		resp := openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "resumo compacto da conversa anterior"},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Build a long conversation whose estimated tokens exceed 85% of a small budget.
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
	}
	longContent := strings.Repeat("dado tecnico relevante da conversa ", 50)
	for i := 0; i < 10; i++ {
		messages = append(messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: longContent},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: longContent},
		)
	}
	// Mark the last few distinctly so we can assert they survive verbatim.
	messages[len(messages)-1] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "ULTIMA_RESPOSTA_VERBATIM"}
	messages[len(messages)-2] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "ULTIMA_PERGUNTA_VERBATIM"}

	budget := estimateMessagesTokens(messages) // conversation is already ~100% of this budget
	var statusMessages []string
	got := compactIfNeeded(context.Background(), client, "test-model", messages, budget, func(s string) {
		statusMessages = append(statusMessages, s)
	})

	if !calledWithSummaryPrompt {
		t.Fatal("expected compaction to call the LLM with the summarization prompt")
	}
	if len(statusMessages) != 1 {
		t.Errorf("expected exactly one onStatus call, got %d", len(statusMessages))
	}
	if len(got) >= len(messages) {
		t.Errorf("expected compaction to shrink message count, got %d, original %d", len(got), len(messages))
	}
	if got[0].Role != openai.ChatMessageRoleSystem || got[0].Content != "system prompt" {
		t.Error("expected the original system prompt to survive as messages[0]")
	}
	found := false
	for _, m := range got {
		if strings.Contains(m.Content, "Automatic summary") {
			found = true
		}
	}
	if !found {
		t.Error("expected a compaction summary message to be present")
	}
	last := got[len(got)-1]
	if last.Content != "ULTIMA_RESPOSTA_VERBATIM" {
		t.Errorf("expected the most recent message to survive verbatim, got %q", last.Content)
	}
}

func TestSafeSplitIndex_NeverOrphansToolMessage(t *testing.T) {
	t.Parallel()

	rest := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "0"},
		{Role: openai.ChatMessageRoleAssistant, Content: "1", ToolCalls: []openai.ToolCall{{ID: "call1"}}},
		{Role: openai.ChatMessageRoleTool, Content: "2", ToolCallID: "call1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "3"},
		{Role: openai.ChatMessageRoleUser, Content: "4"},
	}

	// A naive split lands right on the tool message (index 2) -- must be
	// pushed back to index 1 (the assistant message that issued the call)
	// so "recent" never starts with an orphaned tool result.
	got := safeSplitIndex(rest, 2)
	if got != 1 {
		t.Errorf("safeSplitIndex(rest, 2) = %d, want 1 (the assistant tool_calls message)", got)
	}
	if rest[got].Role == openai.ChatMessageRoleTool {
		t.Errorf("safeSplitIndex must never return an index pointing at a tool message, got role %q", rest[got].Role)
	}

	// A split that already lands on a safe boundary should be unchanged.
	if got := safeSplitIndex(rest, 3); got != 3 {
		t.Errorf("safeSplitIndex(rest, 3) = %d, want 3 (already safe)", got)
	}
}

func TestCompactIfNeeded_TooFewMessagesSkipped(t *testing.T) {
	t.Parallel()

	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call the LLM when there are too few messages to compact")
	})

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("x", 10000)},
	}
	got := compactIfNeeded(context.Background(), client, "test-model", messages, 1, nil) // budget of 1 forces "over threshold"
	if len(got) != len(messages) {
		t.Errorf("expected no compaction on a tiny conversation, got %d messages", len(got))
	}
}
