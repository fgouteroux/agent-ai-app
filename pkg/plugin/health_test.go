package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func newTestApp(t *testing.T, endpointURL, apiKey string) *App {
	t.Helper()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":    endpointURL,
		"model":          "test-model",
		"timeoutSeconds": 10,
		"maxTokens":      100,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}

	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": apiKey},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	return inst.(*App)
}

func TestHealthCheck_Success(t *testing.T) {
	t.Parallel()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "test-key")

	result, err := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{
		PluginContext: backend.PluginContext{},
	})
	if err != nil {
		t.Fatalf("CheckHealth returned error: %v", err)
	}

	if result.Status != backend.HealthStatusOk {
		t.Errorf("Status = %v, want %v", result.Status, backend.HealthStatusOk)
	}
}

// A stale/invalid brain-agent grafanaToken used to make CheckHealth report
// this whole plugin as HealthStatusError -- which /resources/health then
// surfaced as the chat UI's "LLM plugin unavailable" banner, even though the
// LLM itself (checked first, above) was completely healthy. Live-reported by
// a real user seeing that banner repeatedly with a working LLM behind it.
// brain-agent tools are an optional enhancement, not the core chat path, so
// this must degrade gracefully: Ok status, issue only noted in the message.
func TestHealthCheck_BrainAgentIntegrationDegradedDoesNotFailHealth(t *testing.T) {
	t.Parallel()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer llmServer.Close()

	brainAgentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid API key"}`))
	}))
	defer brainAgentServer.Close()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":           llmServer.URL + "/v1",
		"model":                 "test-model",
		"timeoutSeconds":        10,
		"maxTokens":             100,
		"grafanaURL":            brainAgentServer.URL,
		"enableBrainAgentTools": true,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "test-key", "grafanaToken": "stale-token"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	result, err := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil {
		t.Fatalf("CheckHealth returned error: %v", err)
	}
	if result.Status != backend.HealthStatusOk {
		t.Errorf("Status = %v, want %v (brain-agent is an optional integration, its failure must not fail the whole plugin's health)", result.Status, backend.HealthStatusOk)
	}
	if !strings.Contains(result.Message, "brain-agent tools integration degraded") {
		t.Errorf("Message = %q, want it to note the degraded brain-agent integration", result.Message)
	}
}

func TestHealthCheck_Failure(t *testing.T) {
	t.Parallel()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_api_key"}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "bad-key")

	result, err := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{
		PluginContext: backend.PluginContext{},
	})
	if err != nil {
		t.Fatalf("CheckHealth returned error: %v", err)
	}

	if result.Status != backend.HealthStatusError {
		t.Errorf("Status = %v, want %v", result.Status, backend.HealthStatusError)
	}
}

// Security-audit finding M-05: a failed health check's message includes the
// raw LLM endpoint URL/port (see humanizeConnectErr) -- useful for an Admin
// debugging Configuration's "Test connection" button, unnecessary internal
// topology for a Viewer just looking at the chat page. handleHealth must
// gate that detail on role, not show it to everyone regardless of who's
// asking.
func TestHandleHealth_DetailedMessageOnlyForAdmin(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://127.0.0.1:1/v1", "key")

	for _, tc := range []struct {
		name string
		role string
	}{
		{"admin sees the real endpoint", "Admin"},
		{"editor gets the generic message", "Editor"},
		{"viewer gets the generic message", "Viewer"},
		{"no role at all gets the generic message", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			ctx := backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Role: tc.role}})
			w := httptest.NewRecorder()

			app.handleHealth(w, req.WithContext(ctx))

			var body map[string]string
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if tc.role == "Admin" {
				if !strings.Contains(body["message"], "127.0.0.1:1") {
					t.Errorf("Admin message = %q, want it to contain the real endpoint", body["message"])
				}
				if body["model"] == "" {
					t.Error("Admin response should still include the model")
				}
			} else {
				if strings.Contains(body["message"], "127.0.0.1") {
					t.Errorf("non-Admin (role=%q) message = %q, must not leak the internal endpoint", tc.role, body["message"])
				}
				if body["model"] != "" {
					t.Errorf("non-Admin (role=%q) response should not include the model, got %q", tc.role, body["model"])
				}
			}
		})
	}
}

func TestHealthCheck_ConnectionRefused(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://127.0.0.1:1/v1", "key")

	result, err := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{
		PluginContext: backend.PluginContext{},
	})
	if err != nil {
		t.Fatalf("CheckHealth returned error: %v", err)
	}

	if result.Status != backend.HealthStatusError {
		t.Errorf("Status = %v, want %v", result.Status, backend.HealthStatusError)
	}
}

// Regression test: CheckHealth used to build its HTTP client with
// a.settings.TimeoutSeconds (up to 300s, meant for a real chat request) --
// a slow/hanging LLM endpoint made a health check take just as long, and
// the UI's mount-time hook retries that up to 4 times. This proves the
// health check now cuts off on its own short healthCheckTimeout regardless
// of how generous TimeoutSeconds is configured.
func TestHealthCheck_UsesItsOwnShortTimeoutRegardlessOfConfiguredTimeoutSeconds(t *testing.T) {
	t.Parallel()

	slowLLMServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(healthCheckTimeout + 2*time.Second)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer slowLLMServer.Close()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":    slowLLMServer.URL + "/v1",
		"model":          "test-model",
		"timeoutSeconds": 300, // generous, chat-sized -- must NOT be what bounds this call
		"maxTokens":      100,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "test-key"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	start := time.Now()
	result, err := app.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CheckHealth returned error: %v", err)
	}
	if result.Status != backend.HealthStatusError {
		t.Errorf("Status = %v, want %v (should time out before the slow endpoint ever responds)", result.Status, backend.HealthStatusError)
	}
	if elapsed >= healthCheckTimeout+2*time.Second {
		t.Errorf("elapsed = %v, want well under the server's %v delay -- healthCheckTimeout should have cut it off first", elapsed, healthCheckTimeout+2*time.Second)
	}
}

func TestHandleHealth_CachesResultAcrossRequests(t *testing.T) {
	t.Parallel()

	var hits int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "test-key")

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		w := httptest.NewRecorder()
		app.handleHealth(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, w.Code)
		}
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("LLM endpoint hit %d times across 3 requests within the cache TTL, want exactly 1", got)
	}
}

func TestHandleHealth_RefreshesAfterCacheExpires(t *testing.T) {
	t.Parallel()

	var hits int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "test-key")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	app.handleHealth(httptest.NewRecorder(), req)

	// Force the cache to look expired instead of sleeping healthCacheTTL
	// (15s) in a test.
	app.healthCacheMu.Lock()
	app.healthCacheTime = time.Now().Add(-healthCacheTTL - time.Second)
	app.healthCacheMu.Unlock()

	app.handleHealth(httptest.NewRecorder(), req)

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("LLM endpoint hit %d times across 2 requests spanning an expired cache, want exactly 2", got)
	}
}
