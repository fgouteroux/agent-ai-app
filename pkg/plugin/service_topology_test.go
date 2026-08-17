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

// healthyChainTraceJSON mirrors scripts/seed_traces.py's "healthy" 3-service
// trace: demo-app calls both demo-db and demo-payments as siblings.
const healthyChainTraceJSON = `{
	"batches": [
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-app"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "t1", "spanId": "app-span",
				"name": "GET /checkout",
				"startTimeUnixNano": "1000000000", "endTimeUnixNano": "1025000000",
				"status": {}
			}]}]
		},
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-db"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "t1", "spanId": "db-span", "parentSpanId": "app-span",
				"name": "SELECT inventory",
				"startTimeUnixNano": "1000000000", "endTimeUnixNano": "1010000000",
				"status": {}
			}]}]
		},
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-payments"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "t1", "spanId": "pay-span", "parentSpanId": "app-span",
				"name": "POST /charge",
				"startTimeUnixNano": "1010000000", "endTimeUnixNano": "1025000000",
				"status": {}
			}]}]
		}
	]
}`

func serviceTopologyMock(t *testing.T, traceIDs []string, traceBody string) *httptest.Server {
	t.Helper()
	// Build a search response listing every traceID -- real shape captured
	// live from Tempo's /api/search.
	var traces []map[string]any
	for _, id := range traceIDs {
		traces = append(traces, map[string]any{"traceID": id, "rootServiceName": "demo-app"})
	}
	searchBody, err := json.Marshal(map[string]any{"traces": traces})
	if err != nil {
		t.Fatalf("marshal search body: %v", err)
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/datasources":
			_, _ = w.Write([]byte(`[{"type":"tempo","uid":"tempo-uid"}]`))
		case strings.Contains(r.URL.Path, "/api/search"):
			_, _ = w.Write(searchBody)
		case strings.Contains(r.URL.Path, "/api/traces/"):
			_, _ = w.Write([]byte(traceBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestBuildServiceTopology_DerivesEdgesFromRealTraces(t *testing.T) {
	t.Parallel()

	server := serviceTopologyMock(t, []string{"t1", "t2", "t3"}, healthyChainTraceJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.buildServiceTopology(context.Background(), `{"service_name":"demo-app"}`)
	if err != nil {
		t.Fatalf("buildServiceTopology failed: %v", err)
	}

	var parsed serviceTopologyResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.TracesWalked != 3 {
		t.Errorf("traces_walked = %d, want 3", parsed.TracesWalked)
	}
	if len(parsed.Services) != 3 {
		t.Errorf("services = %v, want 3 (demo-app, demo-db, demo-payments)", parsed.Services)
	}
	if len(parsed.Edges) != 2 {
		t.Fatalf("edges = %+v, want exactly 2 (demo-app->demo-db, demo-app->demo-payments)", parsed.Edges)
	}
	for _, e := range parsed.Edges {
		if e.From != "demo-app" {
			t.Errorf("edge %+v: want From=demo-app", e)
		}
		if e.CallCount != 3 {
			t.Errorf("edge %+v: want call_count=3 (one per sampled trace)", e)
		}
	}
}

func TestBuildServiceTopology_RequiresServiceName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.buildServiceTopology(context.Background(), `{}`); err == nil {
		t.Error("expected an error when service_name is missing")
	}
}

func TestBuildServiceTopology_NoTracesFoundIsAnError(t *testing.T) {
	t.Parallel()

	server := serviceTopologyMock(t, nil, healthyChainTraceJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	if _, err := te.buildServiceTopology(context.Background(), `{"service_name":"nonexistent-service"}`); err == nil {
		t.Error("expected an error when no traces are found for the service")
	}
}
