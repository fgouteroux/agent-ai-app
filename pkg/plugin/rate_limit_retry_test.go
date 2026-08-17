package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// newTestOpenAIClientWithRetryAfterCapture is like newTestOpenAIClient, but
// wires in retryAfterTransport -- the only way a test server's Retry-After
// header actually reaches createChatCompletionWithRetry's capture, mirroring
// how newLLMProvider wires it for a real provider in providers.go.
func newTestOpenAIClientWithRetryAfterCapture(t *testing.T, handler http.HandlerFunc) *openai.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL = server.URL
	cfg.HTTPClient = &http.Client{Transport: &retryAfterTransport{base: http.DefaultTransport}}
	return openai.NewClientWithConfig(cfg)
}

func TestRateLimitMaxRetries_DefaultsWhenNil(t *testing.T) {
	if got := rateLimitMaxRetries(Settings{}); got != defaultRateLimitMaxRetries {
		t.Errorf("expected default %d, got %d", defaultRateLimitMaxRetries, got)
	}
	zero := 0
	if got := rateLimitMaxRetries(Settings{RateLimitMaxRetries: &zero}); got != 0 {
		t.Errorf("expected explicit 0 to be respected, got %d", got)
	}
}

func TestCreateChatCompletionWithRetry_RetriesOn429ThenSucceeds(t *testing.T) {
	attempts := 0
	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})

	origBackoff := rateLimitBaseBackoff
	rateLimitBaseBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { rateLimitBaseBackoff = origBackoff })

	var retryWaits []time.Duration
	resp, err := createChatCompletionWithRetry(context.Background(), client, openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	}, 3, func(wait time.Duration) { retryWaits = append(retryWaits, wait) })

	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected response content: %q", resp.Choices[0].Message.Content)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (2 failures + 1 success), got %d", attempts)
	}
	if len(retryWaits) != 2 {
		t.Errorf("expected onRetry called twice, got %d", len(retryWaits))
	}
}

func TestCreateChatCompletionWithRetry_ZeroMaxRetriesFailsImmediately(t *testing.T) {
	attempts := 0
	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
	})

	_, err := createChatCompletionWithRetry(context.Background(), client, openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	}, 0, nil)

	if err == nil {
		t.Fatal("expected an error with maxRetries=0")
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt with maxRetries=0, got %d", attempts)
	}
}

func TestCreateChatCompletionWithRetry_RespectsRetryAfterHeader(t *testing.T) {
	attempts := 0
	client := newTestOpenAIClientWithRetryAfterCapture(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			// 1s is deliberately far larger than the 1ms fixed backoff
			// schedule set below -- if this header is ever ignored in favor
			// of the fixed schedule, the assertion below catches it.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})

	origBackoff := rateLimitBaseBackoff
	rateLimitBaseBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { rateLimitBaseBackoff = origBackoff })

	var retryWaits []time.Duration
	_, err := createChatCompletionWithRetry(context.Background(), client, openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	}, 1, func(wait time.Duration) { retryWaits = append(retryWaits, wait) })

	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if len(retryWaits) != 1 {
		t.Fatalf("expected onRetry called once, got %d", len(retryWaits))
	}
	if retryWaits[0] != time.Second {
		t.Errorf("expected the 1s Retry-After header to be respected instead of the fixed backoff, got %v", retryWaits[0])
	}
}

func TestCreateChatCompletionWithRetry_CapsExcessiveRetryAfterHeader(t *testing.T) {
	attempts := 0
	client := newTestOpenAIClientWithRetryAfterCapture(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.Header().Set("Retry-After", "3600") // an hour -- must be capped
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})

	// Shrink the cap itself for the test -- otherwise this test would really
	// sleep the full real maxRespectedRetryAfter (60s) via time.After.
	origCap := maxRespectedRetryAfter
	maxRespectedRetryAfter = 5 * time.Millisecond
	t.Cleanup(func() { maxRespectedRetryAfter = origCap })

	var retryWaits []time.Duration
	_, err := createChatCompletionWithRetry(context.Background(), client, openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	}, 1, func(wait time.Duration) { retryWaits = append(retryWaits, wait) })

	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if len(retryWaits) != 1 {
		t.Fatalf("expected onRetry called once, got %d", len(retryWaits))
	}
	if retryWaits[0] != maxRespectedRetryAfter {
		t.Errorf("expected the excessive Retry-After to be capped at %v, got %v", maxRespectedRetryAfter, retryWaits[0])
	}
}

func TestCreateChatCompletionWithRetry_FallsBackToFixedBackoffWithoutHeader(t *testing.T) {
	attempts := 0
	// Deliberately NOT using the retry-after-capturing client -- mirrors any
	// provider that never sends the header, or a caller that (like
	// newTestOpenAIClient) never wired the capturing transport in.
	client := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})

	origBackoff := rateLimitBaseBackoff
	rateLimitBaseBackoff = []time.Duration{42 * time.Millisecond}
	t.Cleanup(func() { rateLimitBaseBackoff = origBackoff })

	var retryWaits []time.Duration
	_, err := createChatCompletionWithRetry(context.Background(), client, openai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	}, 1, func(wait time.Duration) { retryWaits = append(retryWaits, wait) })

	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if len(retryWaits) != 1 {
		t.Fatalf("expected onRetry called once, got %d", len(retryWaits))
	}
	if retryWaits[0] != 42*time.Millisecond {
		t.Errorf("expected the fixed backoff to be used, got %v", retryWaits[0])
	}
}
