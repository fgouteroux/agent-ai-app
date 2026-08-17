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

func sloBurnRateMock(t *testing.T, goodValue, totalValue string) *httptest.Server {
	t.Helper()
	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "good") {
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":[[1000,"` + goodValue + `"]]}]}}`))
		} else {
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":[[1000,"` + totalValue + `"]]}]}}`))
		}
	})
	return httptest.NewServer(&mux)
}

func TestAnalyzeSLOBurnRate_HighBurnRateFlagsOverBudget(t *testing.T) {
	t.Parallel()

	// 900/1000 good = 90% success = 10% error rate. slo_target=0.999 means
	// the error budget is only 0.1% -- a 10% actual error rate burns it
	// roughly 100x too fast.
	server := sloBurnRateMock(t, "900", "1000")
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeSLOBurnRate(context.Background(), `{"good_query":"sum(good_total)","total_query":"sum(total_total)","slo_target":0.999,"budget_window":"30d"}`)
	if err != nil {
		t.Fatalf("analyzeSLOBurnRate returned error: %v", err)
	}

	var parsed sloBurnRateResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.BurnRate < 50 {
		t.Errorf("burn_rate = %v, want a large burn rate (~100x)", parsed.BurnRate)
	}
	if parsed.TimeToExhaustion == "" {
		t.Error("expected a time-to-exhaustion estimate when budget_window is given")
	}
	if !strings.Contains(parsed.Note, "faster than sustainable") {
		t.Errorf("note = %q, want it to flag the over-budget burn rate", parsed.Note)
	}
}

func TestAnalyzeSLOBurnRate_WithinBudgetNotFlagged(t *testing.T) {
	t.Parallel()

	// 9999/10000 good = 99.99% success, well within a 99.9% SLO target.
	server := sloBurnRateMock(t, "9999", "10000")
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeSLOBurnRate(context.Background(), `{"good_query":"sum(good_total)","total_query":"sum(total_total)","slo_target":0.999}`)
	if err != nil {
		t.Fatalf("analyzeSLOBurnRate returned error: %v", err)
	}

	var parsed sloBurnRateResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.BurnRate >= 1 {
		t.Errorf("burn_rate = %v, want < 1 (within budget)", parsed.BurnRate)
	}
	if strings.Contains(parsed.Note, "faster than sustainable") {
		t.Errorf("note = %q, must not flag a within-budget burn rate as over budget", parsed.Note)
	}
}

func TestAnalyzeSLOBurnRate_RejectsInvalidSLOTarget(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	for _, target := range []float64{0, 1, 1.5, -0.1} {
		body, _ := json.Marshal(map[string]any{"good_query": "g", "total_query": "t", "slo_target": target})
		if _, err := te.analyzeSLOBurnRate(context.Background(), string(body)); err == nil {
			t.Errorf("expected an error for slo_target=%v", target)
		}
	}
}

func TestAnalyzeSLOBurnRate_RequiresGoodAndTotalQuery(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.analyzeSLOBurnRate(context.Background(), `{"slo_target":0.999}`); err == nil {
		t.Error("expected an error when good_query/total_query are missing")
	}
}
