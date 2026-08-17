package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func TestToolExecutor_InvestigateAlert_NoMatchReturnsGracefulMessage(t *testing.T) {
	t.Parallel()

	grafanaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"labels": map[string]string{"alertname": "OtherAlert"}, "state": "active"},
		})
	}))
	defer grafanaMock.Close()

	te := NewToolExecutor(grafanaMock.URL, log.DefaultLogger)
	result, err := te.Execute(context.Background(), "investigate_alert", `{"alertname":"HighCPU"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(result, `No firing alert named "HighCPU" found`) {
		t.Errorf("result = %q, want a graceful no-match message", result)
	}
}

func TestToolExecutor_InvestigateAlert_RequiresAlertName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	_, err := te.Execute(context.Background(), "investigate_alert", `{}`)
	if err == nil {
		t.Fatal("expected an error when alertname is missing, got nil")
	}
}

// investigateAlertMock serves the alertmanager alert list plus datasource
// listing/proxy endpoints investigate_alert needs (Loki logs, Tempo traces),
// and optionally an MCP tools/list+call endpoint.
func investigateAlertMock(t *testing.T, alerts []map[string]any, withMCP bool) *httptest.Server {
	t.Helper()
	var mux http.ServeMux
	mux.HandleFunc("/api/alertmanager/grafana/api/v2/alerts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(alerts)
	})
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"type": "loki", "uid": "loki-uid"},
			{"type": "tempo", "uid": "tempo-uid"},
		})
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[{"values":[["0","a log line about a crash"]]}]}}`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/tempo-uid/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"traces":[{"traceID":"abc123"}]}`))
	})
	if withMCP {
		mux.HandleFunc(mcpToolsPath, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]any{
					"content": []map[string]string{
						{"type": "text", "text": "2026-01-10: HighCPU root-caused to a runaway batch job"},
					},
				},
			})
		})
	}
	return httptest.NewServer(&mux)
}

func TestToolExecutor_InvestigateAlert_GathersLogsAndTracesWithoutMCP(t *testing.T) {
	t.Parallel()

	alerts := []map[string]any{
		{
			"labels":   map[string]string{"alertname": "HighCPU", "namespace": "prod", "pod": "checkout-1", "service": "checkout"},
			"state":    "active",
			"startsAt": "2026-07-20T10:00:00Z",
		},
	}
	server := investigateAlertMock(t, alerts, false)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.Execute(context.Background(), "investigate_alert", `{"alertname":"HighCPU"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var parsed struct {
		Matches        int                  `json:"matches"`
		Investigations []alertInvestigation `json:"investigations"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result failed: %v (%s)", err, result)
	}
	if parsed.Matches != 1 || len(parsed.Investigations) != 1 {
		t.Fatalf("parsed = %+v, want exactly 1 match", parsed)
	}
	inv := parsed.Investigations[0]
	if inv.LogsQuery == "" || !strings.Contains(inv.Logs, "crash") {
		t.Errorf("expected real logs gathered automatically, got %+v", inv)
	}
	if inv.TracesQuery == "" || !strings.Contains(inv.Traces, "abc123") {
		t.Errorf("expected real traces gathered automatically, got %+v", inv)
	}
	if inv.HistoricalCorrelation != "" {
		t.Errorf("expected no historical correlation without an mcp client, got %q", inv.HistoricalCorrelation)
	}
}

func TestToolExecutor_InvestigateAlert_WithMCPCorrelation(t *testing.T) {
	t.Parallel()

	alerts := []map[string]any{
		{"labels": map[string]string{"alertname": "HighCPU", "namespace": "prod", "service": "checkout"}, "state": "active"},
	}
	server := investigateAlertMock(t, alerts, true)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	te.mcp = newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)

	result, err := te.Execute(context.Background(), "investigate_alert", `{"alertname":"HighCPU"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(result, "runaway batch job") {
		t.Errorf("result missing historical correlation: %q", result)
	}
}

func TestToolExecutor_InvestigateAlert_NamespaceDisambiguatesSharedAlertname(t *testing.T) {
	t.Parallel()

	alerts := []map[string]any{
		{"labels": map[string]string{"alertname": "HighCPU", "namespace": "staging", "pod": "staging-1"}, "state": "active"},
		{"labels": map[string]string{"alertname": "HighCPU", "namespace": "prod", "pod": "prod-1"}, "state": "active"},
	}
	server := investigateAlertMock(t, alerts, false)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.Execute(context.Background(), "investigate_alert", `{"alertname":"HighCPU","namespace":"prod"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var parsed struct {
		Matches        int                  `json:"matches"`
		Investigations []alertInvestigation `json:"investigations"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result failed: %v (%s)", err, result)
	}
	if parsed.Matches != 1 {
		t.Fatalf("matches = %d, want exactly 1 (namespace should disambiguate)", parsed.Matches)
	}
	if parsed.Investigations[0].Alert["labels"].(map[string]any)["namespace"] != "prod" {
		t.Errorf("investigated the wrong alert: %+v", parsed.Investigations[0].Alert)
	}
}
