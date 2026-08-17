package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// Feature request: if a question genuinely needs more tool calls than a
// single reasonable budget allows, the assistant should auto-continue
// across checkpoints WITHOUT the user needing to notice a cutoff and ask
// again -- while still guaranteeing an eventual hard stop, so a confused or
// runaway model can never loop tool calls forever (the "what if it needed
// 1000 tools" concern this whole feature answers).

func TestToolBudgetCheckpointNudge_FiresOnlyAtLegBoundariesBeforeTheLast(t *testing.T) {
	t.Parallel()

	if nudge, _ := toolBudgetCheckpointNudge(0); nudge {
		t.Error("must not fire at 0 rounds used")
	}
	if nudge, _ := toolBudgetCheckpointNudge(1); nudge {
		t.Error("must not fire mid-leg")
	}
	if nudge, msg := toolBudgetCheckpointNudge(maxToolRoundsPerLeg); !nudge || msg == "" {
		t.Errorf("must fire at the first leg boundary (%d rounds)", maxToolRoundsPerLeg)
	}
	if nudge, msg := toolBudgetCheckpointNudge(2 * maxToolRoundsPerLeg); !nudge || msg == "" {
		t.Errorf("must fire at the second leg boundary (%d rounds)", 2*maxToolRoundsPerLeg)
	}
	if nudge, _ := toolBudgetCheckpointNudge(maxToolRounds); nudge {
		t.Error("must not fire at the final hard limit -- the caller's own final-answer message takes over there instead")
	}
}

// alwaysToolCallHandler simulates a model that never stops calling tools on
// its own -- every non-streaming request gets another tool call; the one
// streaming request (streamFinalResponse, issued only after the hard
// maxToolRounds limit) gets a real final SSE answer. Used to force the
// round loop all the way to its hard limit so the checkpoint/auto-continue
// behavior in between is actually exercised.
func alwaysToolCallHandler(callCount *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(callCount, 1)
		var reqBody map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &reqBody)

		if isStream, _ := reqBody["stream"].(bool); isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Done after extensive checking. I verified a lot but not everything."}}]}` + "\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "",
						"tool_calls": []map[string]any{
							{
								"id":       "call_" + string(rune('a'+int(n)%26)),
								"type":     "function",
								"function": map[string]any{"name": "does_not_exist", "arguments": "{}"},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
		})
	}
}

func TestStreamingChat_AutoContinuesPastFirstCheckpointThenStopsAtHardLimit(t *testing.T) {
	t.Parallel()

	var callCount int32
	llmServer := httptest.NewServer(alwaysToolCallHandler(&callCount))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{"mode": "chat", "prompt": "investigate everything"}`
	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var statuses []string
	var finalContent strings.Builder
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		var chunk ChatResponse
		if err := json.Unmarshal(res.Body, &chunk); err != nil {
			return nil
		}
		if chunk.Status != "" {
			statuses = append(statuses, chunk.Status)
		}
		finalContent.WriteString(chunk.Content)
		return nil
	})

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	foundCheckpoint := false
	for _, s := range statuses {
		if strings.Contains(s, "Still investigating") {
			foundCheckpoint = true
		}
	}
	if !foundCheckpoint {
		t.Errorf("expected at least one 'Still investigating' checkpoint status, got statuses: %v", statuses)
	}

	// The model never stopped calling tools on its own, so the loop must
	// have run all the way to the hard limit (maxToolRounds non-streaming
	// calls) before falling back to the final streamed answer.
	if n := atomic.LoadInt32(&callCount); int(n) < maxToolRounds {
		t.Errorf("LLM called %d times, want at least %d (the full hard budget, since the model never stopped on its own)", n, maxToolRounds)
	}

	if !strings.Contains(finalContent.String(), "Done after extensive checking") {
		t.Errorf("final content = %q, want the streamFinalResponse answer", finalContent.String())
	}
}
