package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func TestAnalyzeLogPatterns_GroupsSimilarLinesIntoOnePattern(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/datasources":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"Loki","type":"loki","uid":"loki-uid"}]`))
		default:
			w.Header().Set("Content-Type", "application/json")
			// 3 lines are the same underlying message with different
			// IPs/timestamps/request IDs; 1 line is a genuinely different
			// message -- must end up as 2 patterns, not 4.
			_, _ = w.Write([]byte(`{"data":{"result":[{"stream":{},"values":[
				["1000000000","connection refused to 10.0.0.1:8080 at 2026-07-29T10:00:00Z req=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"],
				["2000000000","connection refused to 10.0.0.2:9090 at 2026-07-29T10:05:00Z req=11111111-2222-3333-4444-555555555555"],
				["3000000000","connection refused to 10.0.0.3:7070 at 2026-07-29T10:10:00Z req=99999999-8888-7777-6666-555544443333"],
				["4000000000","disk usage at 95 percent on /data"]
			]}]}}`))
		}
	}))
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.analyzeLogPatterns(context.Background(), `{"selector":"{app=\"checkout\"}"}`)
	if err != nil {
		t.Fatalf("analyzeLogPatterns returned error: %v", err)
	}

	var parsed struct {
		PatternCount int `json:"pattern_count"`
		LinesScanned int `json:"lines_scanned"`
		Patterns     []struct {
			Pattern string `json:"pattern"`
			Count   int    `json:"count"`
		} `json:"patterns"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}

	if parsed.LinesScanned != 4 {
		t.Errorf("lines_scanned = %d, want 4", parsed.LinesScanned)
	}
	if parsed.PatternCount != 2 {
		t.Errorf("pattern_count = %d, want 2 (3 similar lines + 1 distinct)", parsed.PatternCount)
	}

	foundConnectionRefused := false
	for _, p := range parsed.Patterns {
		if p.Count == 3 {
			foundConnectionRefused = true
		}
	}
	if !foundConnectionRefused {
		t.Errorf("expected one pattern with count=3 (the 3 near-duplicate connection-refused lines), got %+v", parsed.Patterns)
	}
}

func TestAnalyzeLogPatterns_RequiresSelector(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.analyzeLogPatterns(context.Background(), `{}`); err == nil {
		t.Error("expected an error when selector is missing")
	}
}

func TestNormalizeLogLine_ReplacesVariableParts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"connection refused to 10.0.0.1:8080", "connection refused to <ip>:<num>"},
		{"req=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee failed", "req=<uuid> failed"},
		{"at 2026-07-29T10:00:00Z", "at <ts>"},
	}
	for _, c := range cases {
		if got := normalizeLogLine(c.in); got != c.want {
			t.Errorf("normalizeLogLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
