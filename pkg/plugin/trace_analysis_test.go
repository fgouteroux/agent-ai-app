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

// tempoTraceMock serves a fixed /api/traces/{id} response body -- shape
// captured live from a real grafana/tempo:2.6.1 instance seeded via
// scripts/seed_traces.py, not guessed.
func tempoTraceMock(t *testing.T, traceBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/datasources" {
			_, _ = w.Write([]byte(`[{"type":"tempo","uid":"tempo-uid"}]`))
			return
		}
		_, _ = w.Write([]byte(traceBody))
	}))
}

// slowDBTraceJSON mirrors the exact shape returned live by Tempo for the
// "slow db call" trace emitted by scripts/seed_traces.py: demo-app's root
// span (~400ms) mostly consumed by demo-db's "SELECT inventory" child span
// (~350ms).
const slowDBTraceJSON = `{
	"batches": [
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-db"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "abc", "spanId": "db-span", "parentSpanId": "app-span",
				"name": "SELECT inventory",
				"startTimeUnixNano": "1000000000", "endTimeUnixNano": "1350000000",
				"status": {}
			}]}]
		},
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-app"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "abc", "spanId": "app-span",
				"name": "GET /checkout",
				"startTimeUnixNano": "1000000000", "endTimeUnixNano": "1400000000",
				"status": {}
			}]}]
		}
	]
}`

// errorTraceJSON mirrors the "payment error" trace: demo-payments' child
// span carries a real STATUS_CODE_ERROR.
const errorTraceJSON = `{
	"batches": [
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-payments"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "def", "spanId": "pay-span", "parentSpanId": "app-span",
				"name": "POST /charge",
				"startTimeUnixNano": "2000000000", "endTimeUnixNano": "2080000000",
				"status": {"code": "STATUS_CODE_ERROR", "message": "upstream payment gateway timeout"}
			}]}]
		},
		{
			"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo-app"}}]},
			"scopeSpans": [{"spans": [{
				"traceId": "def", "spanId": "app-span",
				"name": "POST /checkout/pay",
				"startTimeUnixNano": "2000000000", "endTimeUnixNano": "2080000000",
				"status": {"code": "STATUS_CODE_ERROR", "message": "payment failed"}
			}]}]
		}
	]
}`

func TestAnalyzeTraceBottlenecks_FlagsTheDominantSpan(t *testing.T) {
	t.Parallel()

	server := tempoTraceMock(t, slowDBTraceJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeTraceBottlenecks(context.Background(), `{"trace_id":"abc"}`)
	if err != nil {
		t.Fatalf("analyzeTraceBottlenecks failed: %v", err)
	}

	var parsed traceBottlenecksResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.Bottlenecks) != 1 {
		t.Fatalf("bottlenecks = %+v, want exactly 1 (the db span)", parsed.Bottlenecks)
	}
	if parsed.Bottlenecks[0].Service != "demo-db" {
		t.Errorf("bottleneck service = %q, want demo-db", parsed.Bottlenecks[0].Service)
	}
	if parsed.Bottlenecks[0].PercentOfTrace < 80 {
		t.Errorf("percent_of_trace = %v, want >=80 (350ms of a 400ms trace)", parsed.Bottlenecks[0].PercentOfTrace)
	}
	if len(parsed.Errors) != 0 {
		t.Errorf("errors = %+v, want none (this trace has no error status)", parsed.Errors)
	}
}

func TestAnalyzeTraceBottlenecks_FlagsErrorSpans(t *testing.T) {
	t.Parallel()

	server := tempoTraceMock(t, errorTraceJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeTraceBottlenecks(context.Background(), `{"trace_id":"def"}`)
	if err != nil {
		t.Fatalf("analyzeTraceBottlenecks failed: %v", err)
	}

	var parsed traceBottlenecksResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.Errors) != 2 {
		t.Fatalf("errors = %+v, want 2 (both spans errored)", parsed.Errors)
	}
	foundPayments := false
	for _, e := range parsed.Errors {
		if e.Service == "demo-payments" && strings.Contains(e.Message, "upstream payment gateway timeout") {
			foundPayments = true
		}
	}
	if !foundPayments {
		t.Errorf("errors = %+v, want the demo-payments span with its real error message", parsed.Errors)
	}
}

func TestAnalyzeTraceBottlenecks_RequiresTraceID(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.analyzeTraceBottlenecks(context.Background(), `{}`); err == nil {
		t.Error("expected an error when trace_id is missing")
	}
}

func TestAnalyzeTraceBottlenecks_NoSpansIsAnError(t *testing.T) {
	t.Parallel()

	server := tempoTraceMock(t, `{"batches": []}`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	if _, err := te.analyzeTraceBottlenecks(context.Background(), `{"trace_id":"missing"}`); err == nil {
		t.Error("expected an error when the trace has no spans")
	}
}
