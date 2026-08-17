package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func TestInspectKubernetesEvents_GroupsStructuredJSONByInvolvedObjectAndReason(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"loki","uid":"loki-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		event1 := `{"reason":"FailedScheduling","type":"Warning","message":"0/3 nodes available","involvedObject":{"kind":"Pod","name":"checkout-1"}}`
		event2 := `{"reason":"FailedScheduling","type":"Warning","message":"0/3 nodes available","involvedObject":{"kind":"Pod","name":"checkout-1"}}`
		other := `{"reason":"OOMKilling","type":"Warning","message":"memory limit exceeded","involvedObject":{"kind":"Pod","name":"db-1"}}`
		_, _ = w.Write([]byte(`{"data":{"result":[{"values":[
			["1000000000",` + jsonQuote(event1) + `],
			["2000000000",` + jsonQuote(event2) + `],
			["3000000000",` + jsonQuote(other) + `]
		]}]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.inspectKubernetesEvents(context.Background(), `{"selector":"{job=\"kube-events\"}"}`)
	if err != nil {
		t.Fatalf("inspectKubernetesEvents returned error: %v", err)
	}

	var parsed struct {
		StructuredJSON bool            `json:"structured_json"`
		GroupCount     int             `json:"group_count"`
		Events         []k8sEventGroup `json:"events"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.StructuredJSON {
		t.Error("expected structured_json = true")
	}
	if parsed.GroupCount != 2 {
		t.Fatalf("group_count = %d, want 2 (FailedScheduling x2 same object, OOMKilling x1)", parsed.GroupCount)
	}
	found := false
	for _, e := range parsed.Events {
		if e.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected one group with count=2 (the duplicate FailedScheduling event), got %+v", parsed.Events)
	}
}

func TestInspectKubernetesEvents_FallsBackToPatternGroupingForUnstructuredLogs(t *testing.T) {
	t.Parallel()

	var mux http.ServeMux
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"type":"loki","uid":"loki-uid"}]`))
	})
	mux.HandleFunc("/api/datasources/proxy/uid/loki-uid/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"result":[{"values":[
			["1000000000","Event: FailedScheduling on pod checkout-1 at 10.0.0.5"],
			["2000000000","Event: FailedScheduling on pod checkout-2 at 10.0.0.6"]
		]}]}}`))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.inspectKubernetesEvents(context.Background(), `{"selector":"{job=\"kube-events\"}"}`)
	if err != nil {
		t.Fatalf("inspectKubernetesEvents returned error: %v", err)
	}

	var parsed struct {
		StructuredJSON bool `json:"structured_json"`
		GroupCount     int  `json:"group_count"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.StructuredJSON {
		t.Error("expected structured_json = false for plain-text events")
	}
	// Both lines normalize to the same pattern (IP and pod-suffix number
	// replaced with placeholders) -- must collapse to 1 group.
	if parsed.GroupCount != 1 {
		t.Errorf("group_count = %d, want 1 (both lines share the same normalized pattern)", parsed.GroupCount)
	}
}

func TestInspectKubernetesEvents_RequiresSelector(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.inspectKubernetesEvents(context.Background(), `{}`); err == nil {
		t.Error("expected an error when selector is missing")
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
