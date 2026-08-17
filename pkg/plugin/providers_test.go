package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func TestBuildProviders_PrimaryOnly(t *testing.T) {
	t.Parallel()

	settings := Settings{
		EndpointURL: "https://primary.example.com/v1",
		Model:       "primary-model",
		APIKey:      "primary-key",
	}

	providers := buildProviders(context.Background(), settings, "http://localhost:3000")
	if len(providers) != 1 {
		t.Fatalf("len(providers) = %d, want 1", len(providers))
	}
	if providers[0].model != "primary-model" {
		t.Errorf("providers[0].model = %q, want %q", providers[0].model, "primary-model")
	}
}

func TestBuildProviders_SkipsIncompleteFallbacks(t *testing.T) {
	t.Parallel()

	settings := Settings{
		EndpointURL: "https://primary.example.com/v1",
		Model:       "primary-model",
		APIKey:      "primary-key",
		FallbackProviders: []FallbackProvider{
			{EndpointURL: "https://missing-model.example.com/v1"},
			{Model: "missing-endpoint"},
		},
		FallbackAPIKeys: []string{"some-key", "some-key"},
	}

	providers := buildProviders(context.Background(), settings, "http://localhost:3000")
	if len(providers) != 1 {
		t.Fatalf("len(providers) = %d, want 1 (both fallbacks missing endpoint or model)", len(providers))
	}
}

// Live-confirmed real bug: CheckHealth never required a non-empty API key
// (health.go only sets the Authorization header when one is present), but
// buildProviders used to require one anyway -- so a genuinely no-auth
// endpoint (a local Ollama, reached directly, no key needed) reported
// healthy while chat silently failed with "no LLM provider configured". A
// missing key is no longer a reason to skip a slot; an endpoint that
// actually requires auth and wasn't given a key now fails with a real 401
// from that provider instead, at request time.
func TestBuildProviders_MissingAPIKeyIsNotDisqualifying(t *testing.T) {
	t.Parallel()

	settings := Settings{
		EndpointURL: "http://ollama.local:11434/v1",
		Model:       "primary-model",
		// APIKey deliberately empty -- a no-auth local endpoint.
		FallbackProviders: []FallbackProvider{
			{EndpointURL: "https://fallback.example.com/v1", Model: "fallback-model"},
		},
		// FallbackAPIKeys deliberately omitted/shorter than FallbackProviders.
	}

	providers := buildProviders(context.Background(), settings, "http://localhost:3000")
	if len(providers) != 2 {
		t.Fatalf("len(providers) = %d, want 2 (primary and fallback both usable without a key)", len(providers))
	}
	if providers[0].model != "primary-model" {
		t.Errorf("providers[0].model = %q, want %q", providers[0].model, "primary-model")
	}
	if providers[1].model != "fallback-model" {
		t.Errorf("providers[1].model = %q, want %q", providers[1].model, "fallback-model")
	}
}

func TestBuildProviders_IncludesCompleteFallback(t *testing.T) {
	t.Parallel()

	settings := Settings{
		EndpointURL: "https://primary.example.com/v1",
		Model:       "primary-model",
		APIKey:      "primary-key",
		FallbackProviders: []FallbackProvider{
			{EndpointURL: "https://fallback.example.com/v1", Model: "fallback-model"},
		},
		FallbackAPIKeys: []string{"fallback-key"},
	}

	providers := buildProviders(context.Background(), settings, "http://localhost:3000")
	if len(providers) != 2 {
		t.Fatalf("len(providers) = %d, want 2", len(providers))
	}
	if providers[1].model != "fallback-model" || providers[1].endpointURL != "https://fallback.example.com/v1" {
		t.Errorf("providers[1] = %+v, want fallback-model @ https://fallback.example.com/v1", providers[1])
	}
}

func TestBuildProviders_NoneConfigured(t *testing.T) {
	t.Parallel()

	if providers := buildProviders(context.Background(), Settings{}, "http://localhost:3000"); len(providers) != 0 {
		t.Fatalf("len(providers) = %d, want 0", len(providers))
	}
}

// newTestAppWithFallback builds an App whose primary provider points at
// primaryURL and whose single fallback slot points at fallbackURL -- used to
// exercise the "primary fails before any content, fallback answers" path
// end-to-end through the real NewApp settings-parsing code, not a hand-built
// App struct.
func newTestAppWithFallback(t *testing.T, primaryURL, fallbackURL string) *App {
	t.Helper()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":    primaryURL,
		"model":          "primary-model",
		"timeoutSeconds": 10,
		"maxTokens":      100,
		"fallbackProviders": []map[string]any{
			{"endpointURL": fallbackURL, "model": "fallback-model"},
		},
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}

	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData: jsonData,
		DecryptedSecureJSONData: map[string]string{
			"apiKey":          "primary-key",
			"fallbackApiKey1": "fallback-key",
		},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return inst.(*App)
}

func chatCompletionOKHandler(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": content},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3},
		})
	}
}

func TestChatCompletion_FallsBackToNextProviderOnFirstCallError(t *testing.T) {
	t.Parallel()

	primaryCalls := 0
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
	}))
	defer primaryServer.Close()

	fallbackCalls := 0
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		chatCompletionOKHandler("answer from fallback")(w, r)
	}))
	defer fallbackServer.Close()

	app := newTestAppWithFallback(t, primaryServer.URL+"/v1", fallbackServer.URL+"/v1")
	// No retries against the (deliberately failing) primary -- get to the
	// fallback quickly instead of waiting out the retry backoff.
	zero := 0
	app.settings.RateLimitMaxRetries = &zero

	content, _, err := app.chatCompletion(context.Background(), ChatRequest{Mode: "chat", Prompt: "hi"})
	if err != nil {
		t.Fatalf("chatCompletion() error = %v, want nil (should have fallen back)", err)
	}
	if content != "answer from fallback" {
		t.Errorf("content = %q, want %q", content, "answer from fallback")
	}
	if primaryCalls == 0 {
		t.Error("expected the primary provider to be tried at least once")
	}
	if fallbackCalls == 0 {
		t.Error("expected the fallback provider to be tried after the primary failed")
	}
}

func TestChatCompletion_NoFallbackWhenPrimaryWorks(t *testing.T) {
	t.Parallel()

	fallbackCalls := 0
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		chatCompletionOKHandler("should never be used")(w, r)
	}))
	defer fallbackServer.Close()

	primaryServer := httptest.NewServer(http.HandlerFunc(chatCompletionOKHandler("answer from primary")))
	defer primaryServer.Close()

	app := newTestAppWithFallback(t, primaryServer.URL+"/v1", fallbackServer.URL+"/v1")

	content, _, err := app.chatCompletion(context.Background(), ChatRequest{Mode: "chat", Prompt: "hi"})
	if err != nil {
		t.Fatalf("chatCompletion() error = %v", err)
	}
	if content != "answer from primary" {
		t.Errorf("content = %q, want %q", content, "answer from primary")
	}
	if fallbackCalls != 0 {
		t.Errorf("fallbackCalls = %d, want 0 (primary succeeded, should never touch fallback)", fallbackCalls)
	}
}
