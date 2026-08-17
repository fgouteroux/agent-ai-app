package plugin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// healthCheckTimeout bounds CheckHealth's own HTTP call -- independent of
// the user-configured TimeoutSeconds (which can go up to 5 minutes, meant
// for a real chat request). Worst case before this existed: a 60s+ default
// TimeoutSeconds, retried up to 4 times by the UI's mount-time hook, could
// lock the chat input for minutes over a single slow/unreachable endpoint.
const healthCheckTimeout = 5 * time.Second

// CheckHealth verifies the LLM endpoint is reachable and the API key is valid.
func (a *App) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	url := strings.TrimSuffix(a.settings.EndpointURL, "/") + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("create request: %v", err),
		}, nil
	}

	if a.settings.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.settings.APIKey)
	}

	// Deliberately NOT a.settings.TimeoutSeconds (meant for a real chat
	// request/response, up to 300s) -- a health check that itself takes
	// that long defeats the purpose. This is what the frontend's mount-time
	// retry loop and the admin's "Test connection" button both wait on.
	client := &http.Client{Timeout: healthCheckTimeout}

	resp, err := client.Do(req)
	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: humanizeConnectErr(a.settings.EndpointURL, err),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("LLM endpoint returned status %d", resp.StatusCode),
		}, nil
	}

	message := fmt.Sprintf("Connected to %s using model %s", a.settings.EndpointURL, a.settings.Model)

	// brain-agent tools are an optional enhancement (memory/RAG), not the
	// core chat path -- a stale/invalid grafanaToken there used to report
	// this whole plugin as HealthStatusError, which /resources/health then
	// surfaced as the chat UI's "LLM plugin unavailable" banner even though
	// the LLM itself was working fine. Degrade gracefully instead: the LLM
	// connection above already succeeded, so report Ok and just note the
	// integration issue in the message for whoever is looking at health
	// diagnostics -- the chat keeps working, just without brain-agent tools.
	if a.settings.EnableBrainAgentTools != nil && *a.settings.EnableBrainAgentTools && a.toolExecutor.mcp != nil {
		if err := a.toolExecutor.mcp.CheckHealth(ctx); err != nil {
			message = fmt.Sprintf("%s (brain-agent tools integration degraded: %v)", message, err)
		}
	}

	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: message,
	}, nil
}

// cachedCheckHealth is what /resources/health (the chat UI's own health
// poll, not Grafana's internal CheckHealth entry point) actually calls --
// reuses the last real result for healthCacheTTL so several tabs mounting
// at once, or the UI's own retry loop, share one real LLM-endpoint round
// trip instead of each triggering their own.
func (a *App) cachedCheckHealth(ctx context.Context) (*backend.CheckHealthResult, error) {
	a.healthCacheMu.Lock()
	if a.healthCacheResult != nil && time.Since(a.healthCacheTime) < healthCacheTTL {
		result := a.healthCacheResult
		a.healthCacheMu.Unlock()
		return result, nil
	}
	a.healthCacheMu.Unlock()

	result, err := a.CheckHealth(ctx, &backend.CheckHealthRequest{})
	if err != nil {
		return result, err
	}

	a.healthCacheMu.Lock()
	a.healthCacheResult = result
	a.healthCacheTime = time.Now()
	a.healthCacheMu.Unlock()
	return result, nil
}

// humanizeConnectErr turns the raw dial/transport error from client.Do into a
// short, actionable message -- the raw error (e.g. "Get \"http://host/v1/models\":
// dial tcp host:port: connect: connection refused") is Go-internal plumbing
// that means nothing to whoever is staring at the plugin's health check panel.
func humanizeConnectErr(endpoint string, err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Sprintf("Can't reach the LLM endpoint (%s): nothing is listening there. Make sure the model server is running and the endpoint URL/port are correct.", endpoint)
	case strings.Contains(msg, "no such host"):
		return fmt.Sprintf("Can't resolve the LLM endpoint (%s): check that the hostname in the endpoint URL is correct.", endpoint)
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "timeout"):
		return fmt.Sprintf("Timed out reaching the LLM endpoint (%s). The model server may be overloaded or unreachable from here.", endpoint)
	default:
		return fmt.Sprintf("Can't reach the LLM endpoint (%s): %v", endpoint, err)
	}
}
