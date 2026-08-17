package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveGrafanaToken_PlainToken(t *testing.T) {
	got := resolveGrafanaToken(Settings{GrafanaToken: "plain-token"})
	if got != "plain-token" {
		t.Errorf("got %q, want %q", got, "plain-token")
	}
}

func TestResolveGrafanaToken_PathTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	got := resolveGrafanaToken(Settings{GrafanaToken: "plain-token", GrafanaTokenPath: path})
	if got != "file-token" {
		t.Errorf("got %q, want %q (path should win over plain token)", got, "file-token")
	}
}

func TestResolveGrafanaToken_UnreadablePathReturnsEmpty(t *testing.T) {
	got := resolveGrafanaToken(Settings{GrafanaToken: "plain-token", GrafanaTokenPath: "/nonexistent/path/token"})
	if got != "" {
		t.Errorf("got %q, want empty string when the token file can't be read", got)
	}
}

func TestResolveGrafanaToken_NeitherConfigured(t *testing.T) {
	if got := resolveGrafanaToken(Settings{}); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestDetectLLMApp_EmptyTokenNeverMakesARequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if detectLLMApp(context.Background(), server.URL, "") {
		t.Error("expected false with an empty token")
	}
	if called {
		t.Error("expected no HTTP request to be made when the token is empty")
	}
}

func TestDetectLLMApp_NewFormatHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-token")
		}
		if r.URL.Path != llmAppHealthPath {
			t.Errorf("request path = %q, want %q", r.URL.Path, llmAppHealthPath)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":{"ok":true},"vector":{"enabled":false,"ok":false},"version":"1.0.0"}}`))
	}))
	defer server.Close()

	if !detectLLMApp(context.Background(), server.URL, "test-token") {
		t.Error("expected true for a healthy new-format response")
	}
}

func TestDetectLLMApp_NewFormatNotOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":{"ok":false,"error":"no API key configured"}}}`))
	}))
	defer server.Close()

	// Installed but not configured -- must not be treated as usable.
	if detectLLMApp(context.Background(), server.URL, "test-token") {
		t.Error("expected false when llmProvider.ok is false, even though the app responded")
	}
}

func TestDetectLLMApp_OldFormatHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":true,"vector":false}}`))
	}))
	defer server.Close()

	if !detectLLMApp(context.Background(), server.URL, "test-token") {
		t.Error("expected true for a healthy old-format response")
	}
}

func TestDetectLLMApp_OldFormatDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":false,"vector":false}}`))
	}))
	defer server.Close()

	if detectLLMApp(context.Background(), server.URL, "test-token") {
		t.Error("expected false for a disabled old-format response")
	}
}

func TestDetectLLMApp_PluginNotInstalled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if detectLLMApp(context.Background(), server.URL, "test-token") {
		t.Error("expected false for a 404 (plugin not installed)")
	}
}

func TestDetectLLMApp_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer server.Close()

	if detectLLMApp(context.Background(), server.URL, "test-token") {
		t.Error("expected false for a malformed response body")
	}
}

func TestDetectLLMApp_UnreachableServer(t *testing.T) {
	if detectLLMApp(context.Background(), "http://127.0.0.1:1", "test-token") {
		t.Error("expected false when the health endpoint can't be reached at all")
	}
}

func TestNewLLMAppProvider_UsesBaseModelAndCorrectPath(t *testing.T) {
	p := newLLMAppProvider("http://grafana.example.com", "test-token", 30)
	wantURL := "http://grafana.example.com" + llmAppAPIPath
	if p.endpointURL != wantURL {
		t.Errorf("endpointURL = %q, want %q", p.endpointURL, wantURL)
	}
	if p.model != llmAppModel {
		t.Errorf("model = %q, want %q", p.model, llmAppModel)
	}
}

func TestBuildProviders_AppendsLLMAppWhenDetected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":{"ok":true}}}`))
	}))
	defer server.Close()

	settings := Settings{
		EndpointURL:  "https://primary.example.com/v1",
		Model:        "primary-model",
		APIKey:       "primary-key",
		GrafanaToken: "grafana-token",
	}
	providers := buildProviders(context.Background(), settings, server.URL)
	if len(providers) != 2 {
		t.Fatalf("len(providers) = %d, want 2 (primary + grafana-llm-app)", len(providers))
	}
	if providers[1].model != llmAppModel {
		t.Errorf("providers[1].model = %q, want %q (grafana-llm-app should be appended last)", providers[1].model, llmAppModel)
	}
}

func TestBuildProviders_LLMAppBecomesOnlyProviderWhenNothingElseConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":{"ok":true}}}`))
	}))
	defer server.Close()

	// Zero-config story: nothing configured except the Grafana token this
	// plugin already requires for tool calls.
	settings := Settings{GrafanaToken: "grafana-token"}
	providers := buildProviders(context.Background(), settings, server.URL)
	if len(providers) != 1 {
		t.Fatalf("len(providers) = %d, want 1 (grafana-llm-app as the sole provider)", len(providers))
	}
	if providers[0].model != llmAppModel {
		t.Errorf("providers[0].model = %q, want %q", providers[0].model, llmAppModel)
	}
}

func TestBuildProviders_NoChangeWhenLLMAppNotDetected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	settings := Settings{
		EndpointURL:  "https://primary.example.com/v1",
		Model:        "primary-model",
		APIKey:       "primary-key",
		GrafanaToken: "grafana-token",
	}
	providers := buildProviders(context.Background(), settings, server.URL)
	if len(providers) != 1 {
		t.Fatalf("len(providers) = %d, want 1 (unchanged -- grafana-llm-app not installed)", len(providers))
	}
	if providers[0].model != "primary-model" {
		t.Errorf("providers[0].model = %q, want %q", providers[0].model, "primary-model")
	}
}

func TestBuildProviders_NoLLMAppWithoutGrafanaToken(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"details":{"llmProvider":{"ok":true}}}`))
	}))
	defer server.Close()

	// No GrafanaToken configured at all -- must not even attempt detection.
	settings := Settings{
		EndpointURL: "https://primary.example.com/v1",
		Model:       "primary-model",
		APIKey:      "primary-key",
	}
	providers := buildProviders(context.Background(), settings, server.URL)
	if len(providers) != 1 {
		t.Fatalf("len(providers) = %d, want 1", len(providers))
	}
	if called {
		t.Error("expected no detection request without a configured Grafana token")
	}
}
