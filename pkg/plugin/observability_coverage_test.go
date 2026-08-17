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

func observabilityCoverageMock(t *testing.T, lokiHasMatch, promHasSeries, dashboardsHaveMatch bool) *httptest.Server {
	t.Helper()
	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"loki","uid":"loki-uid"},{"type":"prometheus","uid":"prom-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/label/namespace/values", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if lokiHasMatch {
			_, _ = w.Write([]byte(`{"status":"success","data":["checkout-service"]}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"success","data":["unrelated"]}`))
		}
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if promHasSeries {
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"job":"checkout-service"},"values":[[1000,"1"]]}]}}`))
		} else {
			_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
		}
	})
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("type") {
		case "dash-folder":
			_, _ = w.Write([]byte(`[]`))
		case "dash-db":
			if dashboardsHaveMatch {
				_, _ = w.Write([]byte(`[{"title":"Checkout Overview","uid":"d1"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		}
	})
	return httptest.NewServer(&mux)
}

// Regression test: check_observability_coverage used to hardcode traces as
// always "NOT CHECKED" regardless of whether a Tempo datasource actually
// existed. Now that Tempo is a real, supported datasource elsewhere in this
// plugin, it must be checked for real whenever one is configured -- this
// verifies that path (Tempo present) both when a match is found and when it
// isn't, distinct from the no-Tempo-at-all tests above which still expect
// the honest "not checked" wording.
func observabilityCoverageMockWithTempo(t *testing.T, tempoHasMatch bool) *httptest.Server {
	t.Helper()
	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"loki","uid":"loki-uid"},{"type":"prometheus","uid":"prom-uid"},{"type":"tempo","uid":"tempo-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/label/namespace/values", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":["unrelated"]}`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/tempo-uid/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if tempoHasMatch {
			_, _ = w.Write([]byte(`{"traces":[{"traceID":"abc","rootServiceName":"checkout-service"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"traces":[]}`))
		}
	})
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	return httptest.NewServer(&mux)
}

func TestCheckObservabilityCoverage_ChecksTracesForRealWhenTempoExists(t *testing.T) {
	t.Parallel()

	server := observabilityCoverageMockWithTempo(t, true)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.checkObservabilityCoverage(context.Background(), `{"service_name":"checkout-service"}`)
	if err != nil {
		t.Fatalf("checkObservabilityCoverage failed: %v", err)
	}

	var parsed observabilityCoverageResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.Traces.Found {
		t.Errorf("traces.found = false, want true (Tempo returned a matching trace)")
	}
	if strings.Contains(parsed.Traces.Detail, "NOT CHECKED") {
		t.Errorf("traces.detail = %q, must not say NOT CHECKED when Tempo was actually queried", parsed.Traces.Detail)
	}
	if parsed.IsPartial {
		t.Error("is_partial = true, want false -- all 4 signals were genuinely checked")
	}
}

func TestCheckObservabilityCoverage_TracesCheckedButNotFoundWhenTempoExists(t *testing.T) {
	t.Parallel()

	server := observabilityCoverageMockWithTempo(t, false)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.checkObservabilityCoverage(context.Background(), `{"service_name":"checkout-service"}`)
	if err != nil {
		t.Fatalf("checkObservabilityCoverage failed: %v", err)
	}

	var parsed observabilityCoverageResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.Traces.Found {
		t.Error("traces.found = true, want false (Tempo returned no matching trace)")
	}
	if strings.Contains(parsed.Traces.Detail, "NOT CHECKED") {
		t.Errorf("traces.detail = %q, must not say NOT CHECKED -- this is a real checked negative, not an unchecked gap", parsed.Traces.Detail)
	}
}

func TestCheckObservabilityCoverage_FullCoverageFound(t *testing.T) {
	t.Parallel()

	server := observabilityCoverageMock(t, true, true, true)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.checkObservabilityCoverage(context.Background(), `{"service_name":"checkout-service"}`)
	if err != nil {
		t.Fatalf("checkObservabilityCoverage returned error: %v", err)
	}

	var parsed observabilityCoverageResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.Logs.Found {
		t.Errorf("expected logs.found = true, got %+v", parsed.Logs)
	}
	if !parsed.Metrics.Found {
		t.Errorf("expected metrics.found = true, got %+v", parsed.Metrics)
	}
	if !parsed.Dashboards.Found {
		t.Errorf("expected dashboards.found = true, got %+v", parsed.Dashboards)
	}
	if parsed.Traces.Found {
		t.Error("traces.found must always be false -- no Tempo in this environment")
	}
	if !strings.Contains(parsed.Traces.Detail, "NOT CHECKED") {
		t.Errorf("traces.detail = %q, want it to explicitly say NOT CHECKED", parsed.Traces.Detail)
	}
}

func TestCheckObservabilityCoverage_NoCoverageFound(t *testing.T) {
	t.Parallel()

	server := observabilityCoverageMock(t, false, false, false)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.checkObservabilityCoverage(context.Background(), `{"service_name":"ghost-service"}`)
	if err != nil {
		t.Fatalf("checkObservabilityCoverage returned error: %v", err)
	}

	var parsed observabilityCoverageResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.Logs.Found || parsed.Metrics.Found || parsed.Dashboards.Found {
		t.Errorf("expected no coverage found anywhere, got %+v", parsed)
	}
}

func TestCheckObservabilityCoverage_RequiresServiceName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.checkObservabilityCoverage(context.Background(), `{}`); err == nil {
		t.Error("expected an error when service_name is missing")
	}
}
