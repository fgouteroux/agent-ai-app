package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func newTestClientWithTransport(t *testing.T, baseURL string) *openai.Client {
	t.Helper()
	config := openai.DefaultConfig("test-key")
	config.BaseURL = baseURL
	config.HTTPClient = &http.Client{Transport: &reasoningKeyRewriteTransport{base: http.DefaultTransport}}
	return openai.NewClientWithConfig(config)
}

func TestReasoningKeyRewrite_NonStreaming_OllamaReasoningFieldReachesReasoningContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "deepseek-r1:14b",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "42", "reasoning": "let me think"}, "finish_reason": "stop"}]
		}`))
	}))
	defer server.Close()

	client := newTestClientWithTransport(t, server.URL)
	resp, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    "deepseek-r1:14b",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "what is the answer?"}},
	})
	if err != nil {
		t.Fatalf("CreateChatCompletion failed: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "42" {
		t.Errorf("Content = %q, want %q", resp.Choices[0].Message.Content, "42")
	}
	if resp.Choices[0].Message.ReasoningContent != "let me think" {
		t.Errorf("ReasoningContent = %q, want %q (the rewrite should have moved Ollama's \"reasoning\" key here)", resp.Choices[0].Message.ReasoningContent, "let me think")
	}
}

func TestReasoningKeyRewrite_NonStreaming_NoOpWhenNoReasoningField(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "llama-3.3-70b-versatile",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}]
		}`))
	}))
	defer server.Close()

	client := newTestClientWithTransport(t, server.URL)
	resp, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    "llama-3.3-70b-versatile",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateChatCompletion failed: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello" {
		t.Errorf("Content = %q, want %q (a normal provider's response must pass through untouched)", resp.Choices[0].Message.Content, "hello")
	}
	if resp.Choices[0].Message.ReasoningContent != "" {
		t.Errorf("ReasoningContent = %q, want empty", resp.Choices[0].Message.ReasoningContent)
	}
}

func TestReasoningKeyRewrite_Streaming_OllamaReasoningDeltaReachesReasoningContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		lines := []string{
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"deepseek-r1:14b","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"thinking..."},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"deepseek-r1:14b","choices":[{"index":0,"delta":{"content":"42"},"finish_reason":null}]}`,
		}
		for _, line := range lines {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := newTestClientWithTransport(t, server.URL)
	stream, err := client.CreateChatCompletionStream(context.Background(), openai.ChatCompletionRequest{
		Model:    "deepseek-r1:14b",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "what is the answer?"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream failed: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var gotReasoning, gotContent strings.Builder
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream.Recv failed: %v", err)
		}
		if len(resp.Choices) == 0 {
			continue
		}
		gotReasoning.WriteString(resp.Choices[0].Delta.ReasoningContent)
		gotContent.WriteString(resp.Choices[0].Delta.Content)
	}

	if gotReasoning.String() != "thinking..." {
		t.Errorf("reasoning = %q, want %q", gotReasoning.String(), "thinking...")
	}
	if gotContent.String() != "42" {
		t.Errorf("content = %q, want %q", gotContent.String(), "42")
	}
}

func TestRewriteReasoningKeyInChoices_PreservesUnrelatedFields(t *testing.T) {
	t.Parallel()

	in := []byte(`{"id":"x","usage":{"prompt_tokens":10,"total_tokens":25},"choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning":"trace"},"finish_reason":"stop"}]}`)
	out := rewriteReasoningKeyInChoices(in, "message")

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output isn't valid JSON: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), `"prompt_tokens":10`) {
		t.Errorf("output = %s, want usage.prompt_tokens preserved exactly", out)
	}
	if !strings.Contains(string(out), `"reasoning_content":"trace"`) {
		t.Errorf("output = %s, want reasoning renamed to reasoning_content", out)
	}
	if strings.Contains(string(out), `"reasoning":`) {
		t.Errorf("output = %s, want the original \"reasoning\" key gone", out)
	}
}

// sanity-check helper used only to confirm the bufio.Scanner-based SSE
// reader doesn't require a trailing newline to flush its last line.
func TestSSEReasoningRewriteReader_ReadsToEOF(t *testing.T) {
	t.Parallel()
	r := &sseReasoningRewriteReader{scanner: bufio.NewScanner(strings.NewReader("data: [DONE]\n")), closer: io.NopCloser(nil)}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "data: [DONE]" {
		t.Errorf("out = %q, want %q", out, "data: [DONE]")
	}
}
