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

func TestDiagnoseKubernetesWorkload_ReportsRealValuesWhenMetricsExist(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Every kube-state-metrics query in this tool gets the same
		// generic non-empty series -- the test cares that a real value
		// surfaces, not the specific number.
		_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{},"values":[[1000,"5"]]}]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.diagnoseKubernetesWorkload(context.Background(), `{"namespace":"prod","name":"checkout"}`)
	if err != nil {
		t.Fatalf("diagnoseKubernetesWorkload returned error: %v", err)
	}

	var parsed workloadDiagnosis
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.Restarts.Found || parsed.Restarts.Value != 5 {
		t.Errorf("restarts = %+v, want found=true value=5", parsed.Restarts)
	}
	if parsed.NoDataAllAround {
		t.Error("expected NoDataAllAround = false when metrics are present")
	}
}

func TestDiagnoseKubernetesWorkload_HonestlyReportsNoDataWhenMetricsAbsent(t *testing.T) {
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
	result, err := te.diagnoseKubernetesWorkload(context.Background(), `{"namespace":"prod","name":"checkout"}`)
	if err != nil {
		t.Fatalf("diagnoseKubernetesWorkload returned error: %v", err)
	}

	var parsed workloadDiagnosis
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.Restarts.Found {
		t.Error("expected Restarts.Found = false when kube-state-metrics has no data")
	}
	if !parsed.NoDataAllAround {
		t.Error("expected NoDataAllAround = true when every check comes back empty")
	}
}

// Real, live-reproduced bug this guards against: asked "are there any pods
// restarting or crashing right now?" against a real ImagePullBackOff pod,
// this tool's restarts/memory/cpu/ready checks all came back 0 or "no data"
// (a container that never started has zero restarts by definition), and
// both a generic and a specialist agent concluded "no problem" -- neither
// ever surfaced the container that was, in fact, actively broken.
func TestDiagnoseKubernetesWorkload_DetectsWaitingIssueEvenWithZeroRestartsAndNoOtherData(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"prometheus","uid":"prom-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom-uid/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "waiting_reason") {
			// Mirrors the real ImagePullBackOff pod found live: restarts=0
			// (it never successfully started), but the waiting-reason
			// series is actively 1 for this exact container.
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"pod":"nginx-qa-namespaces-test-6bc98f8d48-6pttk","container":"nginx","reason":"ImagePullBackOff"},"values":[[1000,"1"]]}]}}`))
			return
		}
		// Every other check (restarts, memory, cpu, ready) genuinely has
		// nothing -- the container never ran, so cAdvisor has no data for
		// it, and the OLD replicas from before the rollout keep the
		// deployment's "available" count looking fine.
		_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.diagnoseKubernetesWorkload(context.Background(), `{"namespace":"dev","name":"nginx-qa-namespaces-test"}`)
	if err != nil {
		t.Fatalf("diagnoseKubernetesWorkload returned error: %v", err)
	}

	var parsed workloadDiagnosis
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.WaitingIssues) != 1 {
		t.Fatalf("WaitingIssues = %+v, want exactly one issue", parsed.WaitingIssues)
	}
	got := parsed.WaitingIssues[0]
	if got.Pod != "nginx-qa-namespaces-test-6bc98f8d48-6pttk" || got.Container != "nginx" || got.Reason != "ImagePullBackOff" {
		t.Errorf("WaitingIssues[0] = %+v, want pod/container/reason from the live incident", got)
	}
	if parsed.NoDataAllAround {
		t.Error("expected NoDataAllAround = false -- a waiting issue was found, so this must not read as \"nothing to report\"")
	}
	if !strings.Contains(parsed.Summary, "ISSUE FOUND") || !strings.Contains(parsed.Summary, "ImagePullBackOff") {
		t.Errorf("Summary = %q, want it to prominently mention the found issue, not just the other (empty) metrics", parsed.Summary)
	}
}

func TestDiagnoseKubernetesWorkload_RequiresNamespaceAndName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.diagnoseKubernetesWorkload(context.Background(), `{"namespace":"prod"}`); err == nil {
		t.Error("expected an error when name is missing")
	}
}
