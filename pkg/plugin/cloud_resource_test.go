package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// cloudwatchDsQueryMock serves a fixed /api/ds/query response body -- shape
// captured live from a real Grafana instance querying its CloudWatch
// datasource against LocalStack (local-stack-cloudwatch), not guessed.
func cloudwatchDsQueryMock(t *testing.T, values [][]float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/datasources" {
			_, _ = w.Write([]byte(`[{"type":"cloudwatch","uid":"cw-uid"}]`))
			return
		}
		resp := map[string]any{
			"results": map[string]any{
				"A": map[string]any{
					"status": 200,
					"frames": []map[string]any{
						{
							"schema": map[string]any{"name": "ErrorCount", "refId": "A"},
							"data":   map[string]any{"values": values},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// Regression test for a real live-validation finding: doGrafanaRequest
// never set Content-Type on a POST body (every prior tool only ever sent
// GET requests) -- Grafana's own /api/ds/query request binding silently
// failed a perfectly well-formed JSON body as "bad request data" (400)
// without it. Only surfaced once a tool (this one) actually sent a POST
// body for the first time.
func TestAnalyzeCloudResource_SendsJSONContentTypeOnPost(t *testing.T) {
	t.Parallel()

	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/datasources" {
			_, _ = w.Write([]byte(`[{"type":"cloudwatch","uid":"cw-uid"}]`))
			return
		}
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": map[string]any{"A": map[string]any{"status": 200, "frames": []map[string]any{}}},
		})
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	if _, err := te.analyzeCloudResource(context.Background(), `{"namespace":"DemoApp/Checkout","metric_name":"ErrorCount"}`); err != nil {
		t.Fatalf("analyzeCloudResource failed: %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestAnalyzeCloudResource_FlagsAnomalousSpike(t *testing.T) {
	t.Parallel()

	// Baseline of 1s/0s (quiet), then a real spike at the end -- same shape
	// as scripts/seed_cloudwatch.sh's seeded ErrorCount data.
	times := []float64{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000}
	values := []float64{1, 0, 1, 0, 1, 0, 1, 0, 46, 45}
	server := cloudwatchDsQueryMock(t, [][]float64{times, values})
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeCloudResource(context.Background(), `{"namespace":"DemoApp/Checkout","metric_name":"ErrorCount","dimensions":{"InstanceId":"i-demo"}}`)
	if err != nil {
		t.Fatalf("analyzeCloudResource failed: %v", err)
	}

	var parsed cloudResourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.Found {
		t.Fatal("found = false, want true (data was returned)")
	}
	if !parsed.IsAnomaly {
		t.Errorf("is_anomaly = false, want true (latest value 45 is far from the ~0.5 baseline)")
	}
	if parsed.Latest != 45 {
		t.Errorf("latest = %v, want 45 (the last datapoint)", parsed.Latest)
	}
	if len(parsed.Datapoints) != 10 {
		t.Errorf("datapoints = %d, want 10", len(parsed.Datapoints))
	}
}

// Regression test for a real live-validation finding: with few total
// datapoints, excluding only the single latest point from the baseline left
// the OTHER point of a 2-point spike inside the baseline, inflating its
// mean/stddev enough to mask the anomaly. Exact shape captured live against
// a real LocalStack CloudWatch instance an hour after seeding (some earlier
// baseline points had aged out of the now-1h window by query time).
func TestAnalyzeCloudResource_FlagsSpikeEvenWithFewTotalDatapoints(t *testing.T) {
	t.Parallel()

	times := []float64{1000, 2000, 3000, 4000, 5000, 6000, 7000}
	values := []float64{0, 1, 0, 1, 0, 46, 45}
	server := cloudwatchDsQueryMock(t, [][]float64{times, values})
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeCloudResource(context.Background(), `{"namespace":"DemoApp/Checkout","metric_name":"ErrorCount","dimensions":{"InstanceId":"i-demo"}}`)
	if err != nil {
		t.Fatalf("analyzeCloudResource failed: %v", err)
	}

	var parsed cloudResourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.IsAnomaly {
		t.Errorf("is_anomaly = false, want true -- the baseline must exclude both spike points (46 and 45), not just the last one")
	}
}

// Regression test for a real live-validation finding: a real CloudWatch
// query returning exactly 1 datapoint made recentWindow (floored at 2)
// exceed len(values), so values[:len(values)-recentWindow] sliced with a
// negative index and panicked with "slice bounds out of range [:-1]" --
// this crashed the ENTIRE plugin backend process (every concurrent user's
// chat session, not just this one request), confirmed live via Grafana's
// own "plugin process exited ... exit status 2" log after this exact
// query shape (DemoApp/Checkout ErrorCount, 1h window) came back sparse.
func TestAnalyzeCloudResource_SingleDatapointDoesNotPanic(t *testing.T) {
	t.Parallel()

	times := []float64{1000}
	values := []float64{5}
	server := cloudwatchDsQueryMock(t, [][]float64{times, values})
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeCloudResource(context.Background(), `{"namespace":"DemoApp/Checkout","metric_name":"ErrorCount"}`)
	if err != nil {
		t.Fatalf("analyzeCloudResource failed: %v", err)
	}

	var parsed cloudResourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.Found {
		t.Error("found = false, want true (1 real datapoint was returned)")
	}
	if parsed.IsAnomaly {
		t.Error("is_anomaly = true, want false -- not enough data to judge, must not be flagged")
	}
	if parsed.Latest != 5 {
		t.Errorf("latest = %v, want 5", parsed.Latest)
	}
}

func TestAnalyzeCloudResource_NoAnomalyOnStableMetric(t *testing.T) {
	t.Parallel()

	times := []float64{1000, 2000, 3000, 4000, 5000, 6000}
	values := []float64{35, 38, 41, 35, 38, 41}
	server := cloudwatchDsQueryMock(t, [][]float64{times, values})
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeCloudResource(context.Background(), `{"namespace":"DemoApp/Checkout","metric_name":"CPUUtilization","dimensions":{"InstanceId":"i-demo"}}`)
	if err != nil {
		t.Fatalf("analyzeCloudResource failed: %v", err)
	}

	var parsed cloudResourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.IsAnomaly {
		t.Errorf("is_anomaly = true, want false for a stable oscillating metric")
	}
}

func TestAnalyzeCloudResource_ReportsNotFoundHonestly(t *testing.T) {
	t.Parallel()

	server := cloudwatchDsQueryMock(t, [][]float64{{}, {}})
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeCloudResource(context.Background(), `{"namespace":"DemoApp/Checkout","metric_name":"NonexistentMetric"}`)
	if err != nil {
		t.Fatalf("analyzeCloudResource failed: %v", err)
	}

	var parsed cloudResourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.Found {
		t.Error("found = true, want false when CloudWatch returned no datapoints")
	}
}

func TestAnalyzeCloudResource_RequiresNamespaceAndMetricName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	for _, args := range []string{`{}`, `{"namespace":"AWS/EC2"}`, `{"metric_name":"CPUUtilization"}`} {
		if _, err := te.analyzeCloudResource(context.Background(), args); err == nil {
			t.Errorf("args=%s: expected an error", args)
		}
	}
}
