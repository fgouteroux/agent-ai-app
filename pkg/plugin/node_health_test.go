package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func TestAnalyzeNodeHealth_ReportsRealValuesWhenNodeExporterPresent(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":[[1000,"42"]]}]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeNodeHealth(context.Background(), `{"instance":"node-1"}`)
	if err != nil {
		t.Fatalf("analyzeNodeHealth returned error: %v", err)
	}

	var parsed nodeHealthResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.CPUBusyPct.Found || parsed.CPUBusyPct.Value != 42 {
		t.Errorf("cpuBusyPercent = %+v, want found=true value=42", parsed.CPUBusyPct)
	}
	if !parsed.DiskUsedPct.Found {
		t.Errorf("maxDiskUsedPercent = %+v, want found=true", parsed.DiskUsedPct)
	}
}

func TestAnalyzeNodeHealth_HonestlyReportsNoDataWhenNodeExporterAbsent(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeNodeHealth(context.Background(), `{"instance":"node-1"}`)
	if err != nil {
		t.Fatalf("analyzeNodeHealth returned error: %v", err)
	}

	var parsed nodeHealthResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.CPUBusyPct.Found {
		t.Error("expected CPUBusyPct.Found = false when node_exporter has no data")
	}
}

func TestAnalyzeNodeHealth_RequiresInstance(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.analyzeNodeHealth(context.Background(), `{}`); err == nil {
		t.Error("expected an error when instance is missing")
	}
}
