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

// stableBaseline is 10 points hovering around 10 (values 8-12) -- low
// variance, so a real spike in the current window stands out clearly.
const stableBaselineValues = `[[1000,"9"],[1060,"10"],[1120,"11"],[1180,"9"],[1240,"10"],[1300,"11"],[1360,"8"],[1420,"12"],[1480,"10"],[1540,"9"]]`

func TestAnalyzeMetricAnomaly_FlagsASpikeAgainstAStableBaseline(t *testing.T) {
	t.Parallel()

	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/datasources":
			_, _ = w.Write([]byte(`[{"name":"Prometheus","type":"prometheus","uid":"prom-uid"}]`))
		case strings.Contains(r.URL.Path, "query_range"):
			call++
			if call == 1 {
				// Current window: one point spikes far above the baseline.
				_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"job":"api"},"values":[[1000,"10"],[1060,"95"],[1120,"11"]]}]}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"job":"api"},"values":` + stableBaselineValues + `}]}}`))
			}
		}
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeMetricAnomaly(context.Background(), `{"query":"up"}`)
	if err != nil {
		t.Fatalf("analyzeMetricAnomaly returned error: %v", err)
	}

	var parsed struct {
		Series []struct {
			BaselinePoints int `json:"baseline_points"`
			Anomalies      []struct {
				Value     float64 `json:"value"`
				Deviation float64 `json:"deviation_stddevs"`
			} `json:"anomalies"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.Series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(parsed.Series))
	}
	s := parsed.Series[0]
	if s.BaselinePoints != 10 {
		t.Errorf("baseline_points = %d, want 10", s.BaselinePoints)
	}
	if len(s.Anomalies) != 1 {
		t.Fatalf("expected exactly 1 anomaly (the spike), got %d: %+v", len(s.Anomalies), s.Anomalies)
	}
	if s.Anomalies[0].Value != 95 {
		t.Errorf("anomaly value = %v, want 95", s.Anomalies[0].Value)
	}
}

func TestAnalyzeMetricAnomaly_WarnsOnInsufficientBaseline(t *testing.T) {
	t.Parallel()

	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/datasources":
			_, _ = w.Write([]byte(`[{"name":"Prometheus","type":"prometheus","uid":"prom-uid"}]`))
		case strings.Contains(r.URL.Path, "query_range"):
			call++
			if call == 1 {
				_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"job":"api"},"values":[[1000,"10"]]}]}}`))
			} else {
				// Only 2 baseline points -- below minBaselinePoints.
				_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"job":"api"},"values":[[1000,"9"],[1060,"11"]]}]}}`))
			}
		}
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeMetricAnomaly(context.Background(), `{"query":"up"}`)
	if err != nil {
		t.Fatalf("analyzeMetricAnomaly returned error: %v", err)
	}
	if !strings.Contains(result, "warning") || !strings.Contains(result, "baseline too small") {
		t.Errorf("result = %q, want a warning about insufficient baseline points", result)
	}
}

func TestAnalyzeMetricAnomaly_RequiresQuery(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.analyzeMetricAnomaly(context.Background(), `{}`); err == nil {
		t.Error("expected an error when query is missing")
	}
}

func TestParseDayAwareDuration_HandlesDaySuffix(t *testing.T) {
	t.Parallel()

	d, err := parseDayAwareDuration("7d")
	if err != nil {
		t.Fatalf("parseDayAwareDuration(7d) error: %v", err)
	}
	if d.Hours() != 168 {
		t.Errorf("7d = %v hours, want 168", d.Hours())
	}

	d, err = parseDayAwareDuration("1h")
	if err != nil {
		t.Fatalf("parseDayAwareDuration(1h) error: %v", err)
	}
	if d.Hours() != 1 {
		t.Errorf("1h = %v hours, want 1", d.Hours())
	}
}
