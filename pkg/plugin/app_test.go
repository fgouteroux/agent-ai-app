package plugin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func TestNewApp_ReturnsNonNil(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test-model","timeoutSeconds":30,"maxTokens":1024}`),
	}

	app, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	if app == nil {
		t.Fatal("NewApp() returned nil")
	}
}

func TestNewApp_ParsesSettings(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"gpt-4o","timeoutSeconds":60,"maxTokens":4096}`),
		DecryptedSecureJSONData: map[string]string{
			"apiKey": "test-api-key",
		},
	}

	inst, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app, ok := inst.(*App)
	if !ok {
		t.Fatal("NewApp() did not return *App")
	}

	if app.settings.EndpointURL != "https://example.com/v1" {
		t.Errorf("EndpointURL = %q, want %q", app.settings.EndpointURL, "https://example.com/v1")
	}

	if app.settings.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", app.settings.Model, "gpt-4o")
	}

	if app.settings.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want %d", app.settings.TimeoutSeconds, 60)
	}

	if app.settings.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want %d", app.settings.MaxTokens, 4096)
	}

	if app.settings.APIKey != "test-api-key" {
		t.Errorf("APIKey = %q, want %q", app.settings.APIKey, "test-api-key")
	}
}

func TestNewApp_DefaultTimeoutSeconds(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test"}`),
	}

	inst, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app := inst.(*App)
	if app.settings.TimeoutSeconds != 180 {
		t.Errorf("TimeoutSeconds = %d, want default %d", app.settings.TimeoutSeconds, 180)
	}
}

func TestNewApp_DefaultMaxTokens(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test"}`),
	}

	inst, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app := inst.(*App)
	if app.settings.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want default %d", app.settings.MaxTokens, 4096)
	}
}

func TestNewApp_InvalidJSON(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{invalid json}`),
	}

	_, err := NewApp(context.Background(), settings)
	if err == nil {
		t.Fatal("NewApp() expected error for invalid JSON, got nil")
	}
}

func TestNewApp_GrafanaTokenPath(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test","grafanaTokenPath":"/var/run/secrets/grafana-sa/token"}`),
	}

	inst, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app, ok := inst.(*App)
	if !ok {
		t.Fatal("NewApp() did not return *App")
	}

	if app.settings.GrafanaTokenPath != "/var/run/secrets/grafana-sa/token" {
		t.Errorf("GrafanaTokenPath = %q, want %q", app.settings.GrafanaTokenPath, "/var/run/secrets/grafana-sa/token")
	}

	if app.toolExecutor.tokenPath != "/var/run/secrets/grafana-sa/token" {
		t.Errorf("toolExecutor.tokenPath = %q, want %q", app.toolExecutor.tokenPath, "/var/run/secrets/grafana-sa/token")
	}
}

func TestNewApp_PlaintextTokenIgnored(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test","grafanaServiceAccountToken":"plaintext-token"}`),
	}

	inst, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app := inst.(*App)
	// Plaintext token in jsonData must NOT be promoted to GrafanaToken
	if app.settings.GrafanaToken != "" {
		t.Errorf("GrafanaToken = %q, want empty (plaintext token should be ignored)", app.settings.GrafanaToken)
	}
}

func TestNewApp_ClampsOversizedAgentContext(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("a", maxAgentContextChars+500)
	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":   "https://example.com/v1",
		"model":         "test",
		"agentContexts": map[string]string{"agent-1": oversized},
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}

	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app := inst.(*App)
	if got := len(app.settings.AgentContexts["agent-1"]); got != maxAgentContextChars {
		t.Errorf("len(AgentContexts[agent-1]) = %d, want %d", got, maxAgentContextChars)
	}
}

// A raw byte-slice truncation (s[:max]) can split a multi-byte UTF-8
// character in half, producing invalid UTF-8 -- e.g. an admin pasting emoji
// or accented characters into "Additional guardrails" (free text reflected
// straight into the LLM system prompt) right at the length cap. Confirms the
// clamp is rune-aware for both CustomGuardrails and AgentContexts.
func TestNewApp_TruncatesCustomGuardrailsAndAgentContextsOnRuneBoundary(t *testing.T) {
	t.Parallel()

	// "e" is a single multi-byte rune (3 bytes in UTF-8); repeating it past
	// the cap and truncating on a byte boundary that lands mid-rune would
	// corrupt the last character instead of just dropping it whole.
	oversizedGuardrails := strings.Repeat("é", maxCustomGuardrailsChars+50)
	oversizedContext := strings.Repeat("é", maxAgentContextChars+50)

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":      "https://example.com/v1",
		"model":            "test",
		"customGuardrails": oversizedGuardrails,
		"agentContexts":    map[string]string{"agent-1": oversizedContext},
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}

	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}
	app := inst.(*App)

	if !utf8.ValidString(app.settings.CustomGuardrails) {
		t.Errorf("CustomGuardrails is not valid UTF-8 after truncation: %q", app.settings.CustomGuardrails)
	}
	if got := utf8.RuneCountInString(app.settings.CustomGuardrails); got != maxCustomGuardrailsChars {
		t.Errorf("rune count of CustomGuardrails = %d, want %d", got, maxCustomGuardrailsChars)
	}

	if !utf8.ValidString(app.settings.AgentContexts["agent-1"]) {
		t.Errorf("AgentContexts[agent-1] is not valid UTF-8 after truncation: %q", app.settings.AgentContexts["agent-1"])
	}
	if got := utf8.RuneCountInString(app.settings.AgentContexts["agent-1"]); got != maxAgentContextChars {
		t.Errorf("rune count of AgentContexts[agent-1] = %d, want %d", got, maxAgentContextChars)
	}
}

func TestNewApp_ClampsExcessiveTimeoutMaxTokensAndRetries(t *testing.T) {
	t.Parallel()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":            "https://example.com/v1",
		"model":                  "test",
		"timeoutSeconds":         999999,
		"maxTokens":              99999999,
		"rateLimitMaxRetries":    999,
		"chatRateLimitPerMinute": 999,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}

	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app := inst.(*App)
	if app.settings.TimeoutSeconds != maxTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want clamped to %d", app.settings.TimeoutSeconds, maxTimeoutSeconds)
	}
	if app.settings.MaxTokens != maxMaxTokens {
		t.Errorf("MaxTokens = %d, want clamped to %d", app.settings.MaxTokens, maxMaxTokens)
	}
	if *app.settings.RateLimitMaxRetries != maxRateLimitMaxRetries {
		t.Errorf("RateLimitMaxRetries = %d, want clamped to %d", *app.settings.RateLimitMaxRetries, maxRateLimitMaxRetries)
	}
	if *app.settings.ChatRateLimitPerMinute != maxChatRateLimitPerMinute {
		t.Errorf("ChatRateLimitPerMinute = %d, want clamped to %d", *app.settings.ChatRateLimitPerMinute, maxChatRateLimitPerMinute)
	}
}

func TestNewApp_ChatRateLimitPerMinute_DefaultsWhenAbsent(t *testing.T) {
	t.Parallel()

	jsonData, err := json.Marshal(map[string]any{"endpointURL": "https://example.com/v1", "model": "test"})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}
	app := inst.(*App)
	if *app.settings.ChatRateLimitPerMinute != defaultChatRateLimitPerMinute {
		t.Errorf("ChatRateLimitPerMinute = %d, want default %d", *app.settings.ChatRateLimitPerMinute, defaultChatRateLimitPerMinute)
	}
}

func TestNewApp_ChatRateLimitPerMinute_ClampsBelowOne(t *testing.T) {
	t.Parallel()

	jsonData, err := json.Marshal(map[string]any{"endpointURL": "https://example.com/v1", "model": "test", "chatRateLimitPerMinute": -5})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}
	app := inst.(*App)
	if *app.settings.ChatRateLimitPerMinute != 1 {
		t.Errorf("ChatRateLimitPerMinute = %d, want clamped to 1", *app.settings.ChatRateLimitPerMinute)
	}
}

// getLimiter must actually honor the configured rate, not just store it --
// with a 1/min limit, a burst of 2 immediate requests for the same user must
// let exactly 1 through.
func TestGetLimiter_HonorsConfiguredRate(t *testing.T) {
	t.Parallel()

	jsonData, err := json.Marshal(map[string]any{"endpointURL": "https://example.com/v1", "model": "test", "chatRateLimitPerMinute": 1})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}
	app := inst.(*App)

	limiter := app.getLimiter("rate-test-user")
	if !limiter.Allow() {
		t.Fatal("1st request should be allowed (burst >= 1)")
	}
	if limiter.Allow() {
		t.Error("2nd immediate request should be rejected at a 1/min rate")
	}
}

func TestNewApp_RejectsFileSchemeEndpointURL(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"file:///etc/passwd","model":"test"}`),
	}

	_, err := NewApp(context.Background(), settings)
	if err == nil {
		t.Fatal("expected error for file:// endpointURL")
	}
}

func TestNewApp_RejectsFileSchemeGrafanaURL(t *testing.T) {
	t.Parallel()

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test","grafanaURL":"gopher://evil.com"}`),
	}

	_, err := NewApp(context.Background(), settings)
	if err == nil {
		t.Fatal("expected error for gopher:// grafanaURL")
	}
}

func TestNewApp_GrafanaTokenPathTakesPrecedence(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	tokenFile := tmpDir + "/token"
	if err := os.WriteFile(tokenFile, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	settings := backend.AppInstanceSettings{
		JSONData: []byte(`{"endpointURL":"https://example.com/v1","model":"test","grafanaTokenPath":"` + tokenFile + `"}`),
		DecryptedSecureJSONData: map[string]string{
			"grafanaToken": "static-token",
		},
	}

	inst, err := NewApp(context.Background(), settings)
	if err != nil {
		t.Fatalf("NewApp() returned error: %v", err)
	}

	app, ok := inst.(*App)
	if !ok {
		t.Fatal("NewApp() did not return *App")
	}

	// tokenPath should be set on executor; static token should NOT be in defaultHeaders
	if app.toolExecutor.tokenPath != tokenFile {
		t.Errorf("tokenPath = %q, want %q", app.toolExecutor.tokenPath, tokenFile)
	}
}

func TestTruncateRunes_CutsOnRuneBoundaryNotByteBoundary(t *testing.T) {
	// "café" is 4 runes but 5 bytes (é is 2 bytes) -- a raw s[:3] byte slice
	// would split é in half and produce invalid UTF-8.
	got := truncateRunes("café", 3)
	if got != "caf" {
		t.Errorf("truncateRunes(%q, 3) = %q, want %q", "café", got, "caf")
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncateRunes(%q, 3) = %q, not valid UTF-8", "café", got)
	}
}

func TestTruncateRunes_ShorterThanMaxReturnsUnchanged(t *testing.T) {
	if got := truncateRunes("short", 100); got != "short" {
		t.Errorf("truncateRunes(short) = %q, want unchanged", got)
	}
}

func TestTruncateRunes_ExactlyMaxReturnsUnchanged(t *testing.T) {
	if got := truncateRunes("exact", 5); got != "exact" {
		t.Errorf("truncateRunes(exact, 5) = %q, want unchanged", got)
	}
}

func TestTruncateRunes_ZeroOrNegativeMaxReturnsEmpty(t *testing.T) {
	if got := truncateRunes("anything", 0); got != "" {
		t.Errorf("truncateRunes(anything, 0) = %q, want empty", got)
	}
	if got := truncateRunes("anything", -1); got != "" {
		t.Errorf("truncateRunes(anything, -1) = %q, want empty", got)
	}
}
