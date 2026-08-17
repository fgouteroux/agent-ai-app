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
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func TestStreamingChat_SendsSSEChunks(t *testing.T) {
	t.Parallel()

	requestCount := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		_ = json.Unmarshal(bodyBytes, &reqBody)
		requestCount++

		isStream, _ := reqBody["stream"].(bool)

		if !isStream {
			// Non-streaming: tool-check round — respond with content (no tool_calls)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "Hello world!",
						},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     5,
					"completion_tokens": 3,
				},
			})
			return
		}

		// Streaming: final response
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected http.Flusher")
		}

		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" world"}}]}`,
			`data: {"choices":[{"delta":{"content":"!"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk + "\n\n"))
			flusher.Flush()
		}
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{
		"mode": "explain_panel",
		"prompt": "test",
		"context": {"panel": {"title": "Test"}}
	}`

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var responses []*backend.CallResourceResponse

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{
			Status:  res.Status,
			Headers: res.Headers,
			Body:    make([]byte, len(res.Body)),
		}
		copy(cp.Body, res.Body)
		responses = append(responses, cp)
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if len(responses) == 0 {
		t.Fatal("expected at least one response")
	}

	// Collect all content from streamed responses
	var fullContent strings.Builder
	for _, resp := range responses {
		var chunk ChatResponse
		if err := json.Unmarshal(resp.Body, &chunk); err != nil {
			continue
		}
		fullContent.WriteString(chunk.Content)
	}

	if got := fullContent.String(); got != "Hello world!" {
		t.Errorf("streamed content = %q, want %q", got, "Hello world!")
	}
}

// Real observed failure: the non-streaming chatCompletion (llm.go) had its
// own looksLikeLanguageMismatch/retryForLanguageSwitch guardrail, but real
// UI traffic goes through streamChatCompletion (this file), which never had
// the same check -- curl testing against the non-streaming /chat route kept
// passing while the real chat UI kept shipping Chinese responses to an
// English prompt. This locks in the fix applied here: the guardrail now
// also runs in the streaming path's round-0 direct-content branch, before
// anything is sent to the frontend.
func TestStreamingChat_RetriesOnLanguageMismatchBeforeSendingAnyChunk(t *testing.T) {
	t.Parallel()

	var calls int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		content := "这里有可用的数据源：Loki (UID: local-loki)，Prometheus (UID: local-prometheus)。如果您需要更多信息，请告诉我。"
		if n >= 3 {
			content = "Available datasources: Loki (UID: local-loki), Prometheus (UID: local-prometheus)."
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": content},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20},
		})
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{"mode": "chat", "prompt": "List the datasources available in this Grafana instance."}`
	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var responses []*backend.CallResourceResponse
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{Status: res.Status, Headers: res.Headers, Body: append([]byte(nil), res.Body...)}
		responses = append(responses, cp)
		return nil
	})

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	var fullContent strings.Builder
	for _, resp := range responses {
		var chunk ChatResponse
		if err := json.Unmarshal(resp.Body, &chunk); err != nil {
			continue
		}
		fullContent.WriteString(chunk.Content)
	}

	got := fullContent.String()
	if strings.Contains(got, "数据源") {
		t.Fatalf("streamed content still contains the mismatched-language text: %q", got)
	}
	want := "Available datasources: Loki (UID: local-loki), Prometheus (UID: local-prometheus)."
	if got != want {
		t.Errorf("streamed content = %q, want %q", got, want)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("LLM called %d times, want 3 (original + 2 retry attempts, the second one clean)", n)
	}
}

// TestStreamingChat_RetriesOnceWhenModelAsksForMissingToolArgument is a
// regression test for a real live incident (2026-08-11, qwen2.5:14b-instruct,
// reproduced 4+ times in a row against the real chat UI): a tool call
// errored on a missing/invalid argument, and instead of retrying with the
// fix the error message itself supplied, the model asked the user to
// provide it -- a message no one would ever answer, permanently stalling
// the turn. This locks in the fix: the round loop now detects that shape
// (looksLikeToolCallAvoidance) and retries once, in-loop, with tools still
// available (unlike the language-mismatch retry path, which rebuilds the
// request without tools and so could never have fixed this).
func TestStreamingChat_RetriesOnceWhenModelAsksForMissingToolArgument(t *testing.T) {
	t.Parallel()

	var round2Body []byte
	callCount := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		callCount++
		if callCount == 2 {
			round2Body = bodyBytes
		}
		w.Header().Set("Content-Type", "application/json")
		content := "It seems that either a query or a trace ID is missing from the function call. Could you please provide more details about what you're trying to investigate?"
		if callCount >= 2 {
			content = "No firing alerts right now."
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20},
		})
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(`{"mode": "chat", "prompt": "Are there any firing alerts right now?"}`),
	}
	var responses []*backend.CallResourceResponse
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{Status: res.Status, Headers: res.Headers, Body: append([]byte(nil), res.Body...)}
		responses = append(responses, cp)
		return nil
	})

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("LLM called %d times, want exactly 2 (original punt + one retry)", callCount)
	}

	var fullContent strings.Builder
	for _, resp := range responses {
		var chunk ChatResponse
		if err := json.Unmarshal(resp.Body, &chunk); err != nil {
			continue
		}
		fullContent.WriteString(chunk.Content)
	}
	if got, want := fullContent.String(), "No firing alerts right now."; got != want {
		t.Errorf("streamed content = %q, want %q -- the punt asking the user must never be shown as the final answer", got, want)
	}

	var round2 map[string]any
	if err := json.Unmarshal(round2Body, &round2); err != nil {
		t.Fatalf("round 2 request body wasn't valid JSON: %v", err)
	}
	if _, hasTools := round2["tools"]; !hasTools {
		t.Error("round 2 request must still include tools -- this retry, unlike the language-mismatch one, requires the model to be able to actually call a tool")
	}
}

func TestStreamingChat_DropsMismatchedPseudoToolCallPreamble(t *testing.T) {
	t.Parallel()

	// Real live case: a pseudo-tool-call round left "_icall_" as the
	// cleanText remainder after extractPseudoToolCalls stripped the
	// recognized JSON call syntax -- unlike the final-answer content, this
	// preamble text was never checked before being streamed, so it reached
	// the user directly ahead of whatever came next (in production, a
	// language-mismatch fallback message on a later round).
	callCount := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"_icall_{\"name\":\"list_alerts\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"No active alerts are currently firing."}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{"mode": "chat", "prompt": "What alerts are firing right now?"}`
	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var responses []*backend.CallResourceResponse
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{Status: res.Status, Headers: res.Headers, Body: append([]byte(nil), res.Body...)}
		responses = append(responses, cp)
		return nil
	})

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	var fullContent strings.Builder
	for _, resp := range responses {
		var chunk ChatResponse
		if err := json.Unmarshal(resp.Body, &chunk); err != nil {
			continue
		}
		fullContent.WriteString(chunk.Content)
	}

	got := fullContent.String()
	if strings.Contains(got, "_icall_") {
		t.Errorf("streamed content = %q, must not contain the dropped pseudo-tool-call preamble", got)
	}
}

func TestStreamingChat_MissingPrompt(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(`{"mode":"explain_panel","context":{}}`),
	}

	var statusCode int

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", statusCode, http.StatusBadRequest)
	}
}

func TestStreamingChat_ConversationHistory(t *testing.T) {
	t.Parallel()

	var receivedMessages []any
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		_ = json.Unmarshal(bodyBytes, &reqBody)

		isStream, _ := reqBody["stream"].(bool)
		messages, _ := reqBody["messages"].([]any)

		if !isStream {
			// Capture the messages sent to the LLM for verification
			receivedMessages = messages

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "Follow-up answer",
						},
						"finish_reason": "stop",
					},
				},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Follow-up answer"}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{
		"mode": "chat",
		"prompt": "What about memory?",
		"context": {},
		"messages": [
			{"role": "user", "content": "How is the cluster?"},
			{"role": "assistant", "content": "CPU is at 45%."}
		]
	}`

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var responses []*backend.CallResourceResponse
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{
			Status:  res.Status,
			Headers: res.Headers,
			Body:    make([]byte, len(res.Body)),
		}
		copy(cp.Body, res.Body)
		responses = append(responses, cp)
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	// Verify conversation history was included in messages to LLM.
	// Expected: system, user("How is the cluster?"), assistant("CPU is at 45%."), user("What about memory?")
	if len(receivedMessages) < 4 {
		t.Fatalf("expected at least 4 messages, got %d", len(receivedMessages))
	}

	// Check that the second message is the first history user message
	msg1, _ := receivedMessages[1].(map[string]any)
	if msg1["role"] != "user" || msg1["content"] != "How is the cluster?" {
		t.Errorf("message[1] = %v, want user/How is the cluster?", msg1)
	}

	// Check that the third message is the assistant history
	msg2, _ := receivedMessages[2].(map[string]any)
	if msg2["role"] != "assistant" || msg2["content"] != "CPU is at 45%." {
		t.Errorf("message[2] = %v, want assistant/CPU is at 45%%.", msg2)
	}

	// Check that the fourth message is the current prompt.
	msg3, _ := receivedMessages[3].(map[string]any)
	content, _ := msg3["content"].(string)
	if msg3["role"] != "user" || !strings.HasPrefix(content, "What about memory?") {
		t.Errorf("message[3] = %v, want user/What about memory?...", msg3)
	}
}

func TestStreamingChat_ReturnsTokenCounts(t *testing.T) {
	t.Parallel()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		_ = json.Unmarshal(bodyBytes, &reqBody)

		isStream, _ := reqBody["stream"].(bool)

		if !isStream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"index":         0,
						"message":       map[string]any{"role": "assistant", "content": "Done."},
						"finish_reason": "stop",
					},
				},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Done."}}]}` + "\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{"mode":"chat","prompt":"test","context":{}}`

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var responses []*backend.CallResourceResponse
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{
			Status: res.Status, Headers: res.Headers,
			Body: make([]byte, len(res.Body)),
		}
		copy(cp.Body, res.Body)
		responses = append(responses, cp)
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	// Find the done chunk and check it has token info
	var doneChunk ChatResponse
	for _, resp := range responses {
		var chunk ChatResponse
		if err := json.Unmarshal(resp.Body, &chunk); err != nil {
			continue
		}
		if chunk.Done {
			doneChunk = chunk
			break
		}
	}

	if doneChunk.ContextTokens <= 0 {
		t.Errorf("expected positive ContextTokens, got %d", doneChunk.ContextTokens)
	}
	if doneChunk.MaxTokens <= 0 {
		t.Errorf("expected positive MaxTokens, got %d", doneChunk.MaxTokens)
	}
}

func TestStreamingChat_ToolCalling(t *testing.T) {
	t.Parallel()

	callCount := 0
	var toolResultContent string
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		_ = json.Unmarshal(bodyBytes, &reqBody)
		callCount++

		isStream, _ := reqBody["stream"].(bool)

		if isStream {
			// Streaming response
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"CPU is at 45%"}}]}` + "\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		// Check if messages contain tool results
		messages, _ := reqBody["messages"].([]any)
		hasToolResult := false
		for _, m := range messages {
			msg, _ := m.(map[string]any)
			if msg["role"] == "tool" {
				hasToolResult = true
				toolResultContent, _ = msg["content"].(string)
			}
		}

		w.Header().Set("Content-Type", "application/json")

		if !hasToolResult {
			// First call: request a tool call
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "",
							"tool_calls": []map[string]any{
								{
									"id":   "call_123",
									"type": "function",
									"function": map[string]any{
										"name":      "query_prometheus",
										"arguments": `{"query":"up"}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
			})
		} else {
			// Second call: return content after tool results
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "CPU is at 45%",
						},
						"finish_reason": "stop",
					},
				},
			})
		}
	}))
	defer llmServer.Close()

	// Grafana mock for datasource proxy
	grafanaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/datasources":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "Prometheus", "type": "prometheus", "uid": "prom-uid"},
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"instance":"node1"},"value":[1234,"0.45"]}]}}`))
		}
	}))
	defer grafanaMock.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")
	app.toolExecutor = NewToolExecutor(grafanaMock.URL, log.DefaultLogger)

	chatReq := `{
		"mode": "explain_panel",
		"prompt": "What is the current CPU?",
		"context": {}
	}`

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var responses []*backend.CallResourceResponse

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		cp := &backend.CallResourceResponse{
			Status:  res.Status,
			Headers: res.Headers,
			Body:    make([]byte, len(res.Body)),
		}
		copy(cp.Body, res.Body)
		responses = append(responses, cp)
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if len(responses) == 0 {
		t.Fatal("expected responses")
	}

	// Should have a tool call notification and content chunks
	var gotToolCall bool
	var fullContent strings.Builder
	for _, resp := range responses {
		var chunk ChatResponse
		if err := json.Unmarshal(resp.Body, &chunk); err != nil {
			continue
		}
		if chunk.ToolCall != nil {
			gotToolCall = true
			if chunk.ToolCall.Name != "query_prometheus" {
				t.Errorf("tool name = %q, want query_prometheus", chunk.ToolCall.Name)
			}
		}
		fullContent.WriteString(chunk.Content)
	}

	if !gotToolCall {
		t.Error("expected tool call notification in stream")
	}

	if got := fullContent.String(); got != "CPU is at 45%" {
		t.Errorf("content = %q, want %q", got, "CPU is at 45%")
	}

	// Verify tool results include structural + text data-framing to prevent prompt injection
	if !strings.HasPrefix(toolResultContent, "<untrusted_tool_output>") {
		t.Errorf("tool result should have structural framing prefix, got: %q", toolResultContent[:min(80, len(toolResultContent))])
	}
	if !strings.Contains(toolResultContent, "[TOOL RESULT") {
		t.Errorf("tool result should still have the text framing prefix, got: %q", toolResultContent[:min(80, len(toolResultContent))])
	}
}

// TestStreamingChat_MalformedToolCallArgumentsDontPoisonHistory reproduces a
// real live incident (2026-08-08): the model's first tool_calls response had
// arguments that weren't valid JSON (a real quantized local model's common
// failure mode). a.toolExecutor.Execute handles that fine as an ordinary
// tool-result error, but the malformed string was also echoed verbatim into
// the assistant message appended to history -- an OpenAI-compatible API
// validates tool_calls[].function.arguments must be valid JSON on every
// request that includes it, so the SECOND round's request (now carrying
// that poisoned history) got rejected outright with a 400, permanently
// failing the whole turn since createChatCompletionWithRetry only retries
// 429. This asserts the second request's own body never contains the
// original invalid string.
func TestStreamingChat_MalformedToolCallArgumentsDontPoisonHistory(t *testing.T) {
	t.Parallel()

	var round2Body []byte
	callCount := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		callCount++
		if callCount == 2 {
			round2Body = bodyBytes
		}

		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "",
							"tool_calls": []map[string]any{
								{
									"id":   "call_1",
									"type": "function",
									// Truncated/invalid JSON -- the real live
									// failure mode this reproduces.
									"function": map[string]any{"name": "query_prometheus", "arguments": `{"query": "up"`},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"},
			},
		})
	}))
	defer llmServer.Close()

	grafanaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer grafanaMock.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")
	app.toolExecutor = NewToolExecutor(grafanaMock.URL, log.DefaultLogger)

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   []byte(`{"mode": "chat", "prompt": "check up metric"}`),
	}
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error { return nil })

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v -- the malformed tool call arguments must not fail the whole turn", err)
	}
	if callCount < 2 {
		t.Fatalf("got %d LLM calls, want at least 2 (a second round after the malformed tool call)", callCount)
	}

	var round2 map[string]any
	if err := json.Unmarshal(round2Body, &round2); err != nil {
		t.Fatalf("round 2 request body wasn't valid JSON: %v", err)
	}
	messages, _ := round2["messages"].([]any)
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		toolCalls, _ := msg["tool_calls"].([]any)
		for _, tcAny := range toolCalls {
			tc, _ := tcAny.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			args, _ := fn["arguments"].(string)
			if args == `{"query": "up"` {
				t.Errorf("round 2 request still contains the original invalid JSON arguments (%q) -- history was not sanitized", args)
			}
			if !json.Valid([]byte(args)) {
				t.Errorf("round 2 request's tool_calls arguments = %q, want valid JSON", args)
			}
		}
	}
}
