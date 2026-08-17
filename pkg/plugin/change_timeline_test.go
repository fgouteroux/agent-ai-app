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

func TestBuildChangeTimeline_SortsEventsChronologically(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately out of order -- the tool must sort them.
		_, _ = w.Write([]byte(`[
			{"time": 3000, "text": "third event", "tags": ["deploy"]},
			{"time": 1000, "text": "first event", "tags": ["deploy"]},
			{"time": 2000, "text": "second event", "tags": []}
		]`))
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.buildChangeTimeline(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("buildChangeTimeline returned error: %v", err)
	}

	var parsed struct {
		Events []timelineEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(parsed.Events))
	}
	if parsed.Events[0].Text != "first event" || parsed.Events[1].Text != "second event" || parsed.Events[2].Text != "third event" {
		t.Errorf("events not sorted chronologically: %+v", parsed.Events)
	}
}

func TestBuildChangeTimeline_NotesAnnotationOnlyScope(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.buildChangeTimeline(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("buildChangeTimeline returned error: %v", err)
	}
	if !strings.Contains(result, "Annotations API only") {
		t.Errorf("result = %q, want the scope note about annotation-only sourcing", result)
	}
}
