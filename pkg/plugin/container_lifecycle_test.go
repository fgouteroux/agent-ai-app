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

func TestAnalyzeContainerLifecycle_ExtractsReasonLabelAndLogs(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"},{"type":"loki","uid":"loki-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query().Get("query")
		switch {
		case strings.Contains(query, "last_terminated_reason"):
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"reason":"OOMKilled"},"values":[[1000,"1"]]}]}}`))
		case strings.Contains(query, "exitcode"):
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":[[1000,"137"]]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":[[1000,"85"]]}]}}`))
		}
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[{"values":[["1000000000","OOMKilled: memory limit exceeded"]]}]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeContainerLifecycle(context.Background(), `{"namespace":"prod","pod":"checkout-1"}`)
	if err != nil {
		t.Fatalf("analyzeContainerLifecycle returned error: %v", err)
	}

	var parsed containerLifecycleResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.LastTerminatedReason != "OOMKilled" {
		t.Errorf("lastTerminatedReason = %q, want OOMKilled", parsed.LastTerminatedReason)
	}
	if !parsed.LastExitCode.Found || parsed.LastExitCode.Value != 137 {
		t.Errorf("lastExitCode = %+v, want found=true value=137", parsed.LastExitCode)
	}
	if !strings.Contains(parsed.RecentLogs, "memory limit exceeded") {
		t.Errorf("recentLogs = %q, want the real log pattern", parsed.RecentLogs)
	}
}

// Same real, live-reproduced gap as diagnose_kubernetes_workload's own
// waiting-reason test: a container stuck in ImagePullBackOff never
// successfully started, so it has no last-terminated reason, no exit code,
// and no memory sample -- without its own waiting-reason check, this tool
// would report "no data ... cannot determine why it died" for exactly the
// case someone asking "why did this container die" most needs surfaced.
func TestAnalyzeContainerLifecycle_DetectsWaitingIssueEvenWithNoTerminatedData(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"},{"type":"loki","uid":"loki-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "waiting_reason") {
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"pod":"nginx-qa-6bc98f8d48-6pttk","container":"nginx","reason":"ImagePullBackOff"},"values":[[1000,"1"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeContainerLifecycle(context.Background(), `{"namespace":"dev","pod":"nginx-qa"}`)
	if err != nil {
		t.Fatalf("analyzeContainerLifecycle returned error: %v", err)
	}

	var parsed containerLifecycleResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.WaitingIssues) != 1 {
		t.Fatalf("WaitingIssues = %+v, want exactly one issue", parsed.WaitingIssues)
	}
	got := parsed.WaitingIssues[0]
	if got.Pod != "nginx-qa-6bc98f8d48-6pttk" || got.Container != "nginx" || got.Reason != "ImagePullBackOff" {
		t.Errorf("WaitingIssues[0] = %+v, want pod/container/reason from the live incident", got)
	}
	if parsed.IsPartial {
		t.Error("expected IsPartial = false -- a waiting issue was found, so this must not read as \"nothing to report\"")
	}
	if !strings.Contains(parsed.Summary, "ISSUE FOUND") || !strings.Contains(parsed.Summary, "ImagePullBackOff") {
		t.Errorf("Summary = %q, want it to prominently mention the found issue", parsed.Summary)
	}
}

func TestAnalyzeContainerLifecycle_RequiresNamespaceAndPod(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.analyzeContainerLifecycle(context.Background(), `{"namespace":"prod"}`); err == nil {
		t.Error("expected an error when pod is missing")
	}
}
