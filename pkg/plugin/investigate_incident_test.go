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

// investigateIncidentMock extends investigateAlertMock's shape with the
// Loki label-values endpoint investigate_incident's free-text-seed fallback
// needs, and an optional withTempo flag to simulate Tempo being absent
// (the real state of every environment this plugin runs in today).
func investigateIncidentMock(t *testing.T, alerts []map[string]any, labelValues map[string][]string, withTempo bool) *httptest.Server {
	t.Helper()
	var mux http.ServeMux
	mux.HandleFunc("/api/alertmanager/grafana/api/v2/alerts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(alerts)
	})
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		datasources := []map[string]any{{"type": "loki", "uid": "loki-uid"}}
		if withTempo {
			datasources = append(datasources, map[string]any{"type": "tempo", "uid": "tempo-uid"})
		}
		_ = json.NewEncoder(w).Encode(datasources)
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[{"values":[["1000000000","a log line about a crash"],["2000000000","a log line about a crash"]]}]}}`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/tempo-uid/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"traces":[{"traceID":"abc123"}]}`))
	})
	for label, values := range labelValues {
		values := values
		mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/label/"+label+"/values", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			out, _ := json.Marshal(map[string]any{"status": "success", "data": values})
			_, _ = w.Write(out)
		})
	}
	return httptest.NewServer(&mux)
}

func TestInvestigateIncident_ExactActiveAlertSeed_NoRegressionFromInvestigateAlert(t *testing.T) {
	t.Parallel()

	alerts := []map[string]any{
		{
			"labels":   map[string]string{"alertname": "HighCPU", "namespace": "prod", "pod": "checkout-1", "service": "checkout"},
			"state":    "active",
			"startsAt": "2026-07-20T10:00:00Z",
		},
	}
	server := investigateIncidentMock(t, alerts, nil, true)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.Execute(context.Background(), "investigate_incident", `{"seed":"HighCPU"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var parsed struct {
		Matches        int                     `json:"matches"`
		Investigations []incidentInvestigation `json:"investigations"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result failed: %v (%s)", err, result)
	}
	if parsed.Matches != 1 {
		t.Fatalf("matches = %d, want 1", parsed.Matches)
	}
	inv := parsed.Investigations[0]
	if !strings.Contains(inv.MatchedAs, "ACTIVE") {
		t.Errorf("matchedAs = %q, want it to say ACTIVE", inv.MatchedAs)
	}
	if strings.Contains(inv.MatchedAs, "resolved") {
		t.Errorf("matchedAs = %q, must never contain the word \"resolved\" -- a live incident showed a model misreading it as the alert itself being resolved", inv.MatchedAs)
	}
	if inv.LogPatternsQuery == "" || !strings.Contains(inv.LogPatterns, "crash") {
		t.Errorf("expected log patterns gathered automatically, got %+v", inv)
	}
	if inv.TracesQuery == "" || !strings.Contains(inv.Traces, "abc123") {
		t.Errorf("expected traces gathered automatically, got %+v", inv)
	}
}

func TestInvestigateIncident_ServiceSeedWithNoActiveAlert_ResolvesViaLokiLabel(t *testing.T) {
	t.Parallel()

	// No active alerts at all -- seed must resolve via a real Loki label
	// value instead of failing outright.
	server := investigateIncidentMock(t, nil, map[string][]string{
		"namespace": {"prod", "staging"},
		"pod":       {"checkout-service-abc123", "other-pod"},
	}, true)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.Execute(context.Background(), "investigate_incident", `{"seed":"checkout-service"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var parsed struct {
		Matches        int                     `json:"matches"`
		Investigations []incidentInvestigation `json:"investigations"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result failed: %v (%s)", err, result)
	}
	if parsed.Matches != 1 {
		t.Fatalf("matches = %d, want 1", parsed.Matches)
	}
	inv := parsed.Investigations[0]
	if !strings.Contains(inv.MatchedAs, "pod=") {
		t.Errorf("matchedAs = %q, want it to resolve via the matching pod label", inv.MatchedAs)
	}
	if inv.LogPatternsQuery == "" || !strings.Contains(inv.LogPatterns, "crash") {
		t.Errorf("expected log patterns gathered from the resolved label, got %+v", inv)
	}
}

func TestInvestigateIncident_TempoUnavailable_OmitsTracesButKeepsLogs(t *testing.T) {
	t.Parallel()

	alerts := []map[string]any{
		{"labels": map[string]string{"alertname": "HighCPU", "namespace": "prod", "pod": "checkout-1", "service": "checkout"}, "state": "active"},
	}
	// withTempo=false -- the real state of every environment this plugin
	// runs in today (no Tempo datasource provisioned anywhere).
	server := investigateIncidentMock(t, alerts, nil, false)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.Execute(context.Background(), "investigate_incident", `{"seed":"HighCPU"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var parsed struct {
		Investigations []incidentInvestigation `json:"investigations"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result failed: %v (%s)", err, result)
	}
	inv := parsed.Investigations[0]
	if inv.TracesError == "" {
		t.Errorf("expected a TracesError when Tempo isn't provisioned, got %+v", inv)
	}
	if !strings.Contains(inv.LogPatterns, "crash") {
		t.Errorf("Tempo failure must not suppress logs, got %+v", inv)
	}
}

func TestInvestigateIncident_RequiresSeed(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.Execute(context.Background(), "investigate_incident", `{}`); err == nil {
		t.Error("expected an error when seed is missing")
	}
}
