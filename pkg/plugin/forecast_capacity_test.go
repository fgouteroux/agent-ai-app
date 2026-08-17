package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func forecastCapacityMock(t *testing.T, valuesJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/datasources":
			_, _ = w.Write([]byte(`[{"name":"Prometheus","type":"prometheus","uid":"prom-uid"}]`))
		default:
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":` + valuesJSON + `}]}}`))
		}
	}))
}

func TestForecastCapacity_RisingTrendProjectsFutureCrossing(t *testing.T) {
	t.Parallel()

	// A clean rising line: 10, 20, 30, 40, 50 an hour apart -- slope is
	// exactly 10/hour, well below a threshold of 100 today.
	server := forecastCapacityMock(t, `[[0,"10"],[3600,"20"],[7200,"30"],[10800,"40"],[14400,"50"]]`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.forecastCapacity(context.Background(), `{"query":"avg(disk_used_percent)","threshold":100}`)
	if err != nil {
		t.Fatalf("forecastCapacity returned error: %v", err)
	}

	var parsed capacityForecastResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.WillCrossThreshold {
		t.Errorf("expected WillCrossThreshold = true for a rising trend below threshold, got %+v", parsed)
	}
	if parsed.EstimatedCrossingAt == "" {
		t.Error("expected a non-empty estimated_crossing_at")
	}
	if parsed.SlopePerHour <= 9 || parsed.SlopePerHour >= 11 {
		t.Errorf("slope_per_hour = %v, want ~10", parsed.SlopePerHour)
	}
}

func TestForecastCapacity_FlatTrendDoesNotProjectCrossing(t *testing.T) {
	t.Parallel()

	server := forecastCapacityMock(t, `[[0,"50"],[3600,"50"],[7200,"50"],[10800,"50"],[14400,"50"]]`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.forecastCapacity(context.Background(), `{"query":"avg(disk_used_percent)","threshold":100}`)
	if err != nil {
		t.Fatalf("forecastCapacity returned error: %v", err)
	}

	var parsed capacityForecastResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.WillCrossThreshold {
		t.Errorf("expected WillCrossThreshold = false for a flat trend, got %+v", parsed)
	}
	if parsed.Note == "" {
		t.Error("expected an explanatory note when no crossing is projected")
	}
}

func TestForecastCapacity_MultipleSeriesRejected(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/datasources":
			_, _ = w.Write([]byte(`[{"name":"Prometheus","type":"prometheus","uid":"prom-uid"}]`))
		default:
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"pod":"a"},"values":[[0,"1"]]},{"metric":{"pod":"b"},"values":[[0,"2"]]}]}}`))
		}
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	if _, err := te.forecastCapacity(context.Background(), `{"query":"disk_used_percent","threshold":100}`); err == nil {
		t.Error("expected an error when the query returns more than one series")
	}
}

func TestForecastCapacity_TooFewPointsRejected(t *testing.T) {
	t.Parallel()

	server := forecastCapacityMock(t, `[[0,"10"],[3600,"20"]]`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	if _, err := te.forecastCapacity(context.Background(), `{"query":"avg(disk_used_percent)","threshold":100}`); err == nil {
		t.Error("expected an error when fewer than forecastCapacityMinPoints are available")
	}
}

func TestForecastCapacity_RequiresQuery(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.forecastCapacity(context.Background(), `{"threshold":100}`); err == nil {
		t.Error("expected an error when query is missing")
	}
}

func TestLinearRegression_FitsAPerfectLine(t *testing.T) {
	t.Parallel()

	xs := []float64{0, 1, 2, 3, 4}
	ys := []float64{10, 20, 30, 40, 50}
	slope, intercept := linearRegression(xs, ys)
	if slope != 10 {
		t.Errorf("slope = %v, want 10", slope)
	}
	if intercept != 10 {
		t.Errorf("intercept = %v, want 10", intercept)
	}
}
